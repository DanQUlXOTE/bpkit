package bpclient

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// agentsOK is a Bindplane that answers a listing.
func agentsOK() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") != "0" {
			w.Write([]byte(`{"agents":[]}`))
			return
		}
		w.Write([]byte(`{"agents":[{"id":"a1","hostname":"h1"}]}`))
	}
}

// An explicit proxy must actually be used, not merely accepted.
func TestProxyIsUsed(t *testing.T) {
	origin := httptest.NewServer(agentsOK())
	defer origin.Close()

	// A proxy that records what it was asked for and forwards it.
	var proxied []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = append(proxied, r.URL.String())
		// A forward proxy receives the absolute URL.
		resp, err := http.Get(r.URL.String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		w.Write(buf[:n])
	}))
	defer proxy.Close()

	c, err := New(Options{BaseURL: origin.URL, APIKey: "k", Proxy: proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	agents, err := c.ListAgents(context.Background(), "", "")
	if err != nil {
		t.Fatalf("request through the proxy failed: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("agents: %+v", agents)
	}
	if len(proxied) == 0 {
		t.Fatal("the proxy was never asked for anything")
	}
	if !strings.Contains(proxied[0], "/v1/agents") {
		t.Fatalf("proxy saw %q", proxied[0])
	}
}

// With no explicit proxy the client must still read the environment, so
// anyone already relying on HTTPS_PROXY keeps working.
//
// This asserts the wiring rather than making a request: net/http resolves
// the proxy environment once per process and caches it, so setting it from
// a test that runs after any other HTTP call has no effect.
func TestProxyDefaultsToTheEnvironment(t *testing.T) {
	c, err := New(Options{BaseURL: "https://example.com", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", c.http.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("no proxy resolver: HTTP_PROXY and HTTPS_PROXY would be ignored")
	}
	want := reflect.ValueOf(http.ProxyFromEnvironment).Pointer()
	if got := reflect.ValueOf(tr.Proxy).Pointer(); got != want {
		t.Fatal("the default proxy resolver is not http.ProxyFromEnvironment")
	}
}

// Setting a proxy replaces that resolver with a fixed one.
func TestExplicitProxyReplacesTheResolver(t *testing.T) {
	c, err := New(Options{BaseURL: "https://example.com", APIKey: "k", Proxy: "http://proxy.corp:8080"})
	if err != nil {
		t.Fatal(err)
	}
	tr := c.http.Transport.(*http.Transport)
	u, err := tr.Proxy(&http.Request{URL: mustURL(t, "https://example.com/v1/agents")})
	if err != nil {
		t.Fatal(err)
	}
	if u == nil || u.Host != "proxy.corp:8080" {
		t.Fatalf("proxy resolved to %v", u)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// An explicit proxy overrides the environment, which is the point of having
// it per profile: one connection through a proxy, another direct.
func TestExplicitProxyBeatsEnvironment(t *testing.T) {
	origin := httptest.NewServer(agentsOK())
	defer origin.Close()

	envProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the environment proxy should not have been used")
		http.Error(w, "nope", http.StatusTeapot)
	}))
	defer envProxy.Close()

	var used int
	explicit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		used++
		resp, err := http.Get(r.URL.String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		w.Write(buf[:n])
	}))
	defer explicit.Close()

	t.Setenv("HTTP_PROXY", envProxy.URL)
	t.Setenv("NO_PROXY", "")

	c, err := New(Options{BaseURL: origin.URL, APIKey: "k", Proxy: explicit.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListAgents(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	if used == 0 {
		t.Fatal("the explicit proxy was not used")
	}
}

func TestProxyAcceptsHostWithoutScheme(t *testing.T) {
	u, err := parseProxy("proxy.corp:8080")
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "http" || u.Host != "proxy.corp:8080" {
		t.Fatalf("got %s://%s", u.Scheme, u.Host)
	}
	// Credentials in the URL must survive, since corporate proxies use them.
	u, err = parseProxy("http://user:pw@proxy.corp:8080")
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := u.User.Password(); u.User.Username() != "user" || pw != "pw" {
		t.Fatalf("credentials lost: %v", u.User)
	}
	if _, err := parseProxy("://nope"); err == nil {
		t.Fatal("a malformed proxy should be rejected")
	}
}

// selfSignedCA returns a CA PEM and a TLS server it signed, standing in for
// a proxy that terminates TLS with its own certificate.
func selfSignedCA(t *testing.T) (caPEM []byte, srv *httptest.Server) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "bpkit test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	pair, err := tls.X509KeyPair(caPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv = httptest.NewUnstartedServer(agentsOK())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return caPEM, srv
}

// A server presenting a certificate from an unknown CA must fail, and must
// succeed once that CA is trusted — which is what a TLS-inspecting proxy
// needs, without turning verification off.
func TestCACertTrustsAnInterceptingProxy(t *testing.T) {
	caPEM, srv := selfSignedCA(t)

	c, err := New(Options{BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListAgents(context.Background(), "", ""); err == nil {
		t.Fatal("an unknown CA should not be trusted by default")
	} else if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expected a certificate error, got %v", err)
	}

	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = New(Options{BaseURL: srv.URL, APIKey: "k", CACert: path})
	if err != nil {
		t.Fatal(err)
	}
	agents, err := c.ListAgents(context.Background(), "", "")
	if err != nil {
		t.Fatalf("the CA should have been trusted: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("agents: %+v", agents)
	}
}

// Skipping verification must still work, and must win over a CA file.
func TestSkipTLSVerifyStillWorks(t *testing.T) {
	_, srv := selfSignedCA(t)
	c, err := New(Options{BaseURL: srv.URL, APIKey: "k", SkipTLSVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListAgents(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
}

// A CA file that is missing or not PEM must fail loudly, rather than
// silently leaving the connection on the system roots.
func TestBadCACertIsRejected(t *testing.T) {
	if _, err := New(Options{BaseURL: "https://x", APIKey: "k", CACert: "/no/such/file.pem"}); err == nil {
		t.Fatal("a missing CA file should be an error")
	}
	junk := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(Options{BaseURL: "https://x", APIKey: "k", CACert: junk})
	if err == nil || !strings.Contains(err.Error(), "no certificates found") {
		t.Fatalf("a non-PEM CA file should be rejected, got %v", err)
	}
}

func TestBadProxyIsRejected(t *testing.T) {
	_, err := New(Options{BaseURL: "https://x", APIKey: "k", Proxy: "://bad"})
	if err == nil {
		t.Fatal("a malformed proxy should be an error")
	}
}
