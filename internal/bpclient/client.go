// Package bpclient is the Bindplane REST client shared by every bpkit
// extension. One client is one connection: a server, a credential, and
// optionally a project to scope requests to.
package bpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Header names Bindplane authenticates and scopes requests with.
const (
	headerAPIKey = "X-Bindplane-Api-Key"
	// headerAccountID selects the project. Keys scoped to more than one
	// project require it; a key that carries its own project does not.
	headerAccountID = "X-Bindplane-Account-ID"
)

type Client struct {
	baseURL string
	apiKey  string
	user    string
	pass    string
	project string
	http    *http.Client
}

type Options struct {
	BaseURL string
	APIKey  string
	User    string
	Pass    string
	Project string // optional; sent as X-Bindplane-Account-ID
	// Proxy is an explicit proxy URL, e.g. http://proxy.corp:8080 or
	// http://user:pass@proxy.corp:8080. Empty means read HTTP_PROXY,
	// HTTPS_PROXY and NO_PROXY from the environment, which is the default
	// and covers most setups.
	Proxy string
	// CACert is a PEM file of extra certificate authorities to trust, for
	// a proxy that terminates TLS and presents its own certificate. They
	// are added to the system roots rather than replacing them.
	CACert        string
	SkipTLSVerify bool
	Timeout       time.Duration
}

func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("server URL required")
	}
	if opts.APIKey == "" && opts.User == "" {
		return nil, fmt.Errorf("either API key or user/pass required")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 60 * time.Second
	}
	// An explicit proxy wins; otherwise fall back to the environment, so
	// HTTPS_PROXY keeps working for anyone already relying on it.
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if p := strings.TrimSpace(opts.Proxy); p != "" {
		u, err := parseProxy(p)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(u)
	}
	if opts.SkipTLSVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	} else if ca := strings.TrimSpace(opts.CACert); ca != "" {
		pool, err := caPool(ca)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	base := strings.TrimRight(opts.BaseURL, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	return &Client{
		baseURL: base,
		apiKey:  opts.APIKey,
		user:    opts.User,
		pass:    opts.Pass,
		project: strings.TrimSpace(opts.Project),
		http:    &http.Client{Timeout: opts.Timeout, Transport: tr},
	}, nil
}

// BaseURL is the normalized server URL, for building links into the UI.
func (c *Client) BaseURL() string { return c.baseURL }

// Project is the project requests are scoped to, or "".
func (c *Client) Project() string { return c.project }

// WithProject returns a copy of c scoped to a different project, sharing the
// same HTTP transport. The migrator uses it to reach a second project with
// one scoped key.
func (c *Client) WithProject(project string) *Client {
	cp := *c
	cp.project = strings.TrimSpace(project)
	return &cp
}

// parseProxy accepts a proxy with or without a scheme, defaulting to http,
// which is what a proxy URL almost always is.
func parseProxy(raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("proxy %q: %w", raw, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy %q: no host", raw)
	}
	return u, nil
}

// caPool returns the system roots plus the authorities in the given PEM
// file. A TLS-terminating proxy signs with its own CA, so without this the
// handshake fails with "certificate signed by unknown authority" and the
// only other way through is turning verification off entirely.
func caPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("CA certificate: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// No system roots to build on (Windows before Go 1.18, or a
		// stripped container): trust only what was given.
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA certificate %s: no certificates found; it should be PEM encoded", path)
	}
	return pool, nil
}

func (c *Client) do(req *http.Request) (int, []byte, error) {
	if c.apiKey != "" {
		req.Header.Set(headerAPIKey, c.apiKey)
	} else {
		req.SetBasicAuth(c.user, c.pass)
	}
	if c.project != "" {
		req.Header.Set(headerAccountID, c.project)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	const maxResponseBytes = 50 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	return resp.StatusCode, body, err
}

// ErrNotFound reports that the server has no such resource. Callers use it
// to tell "absent" apart from "could not be read", which are very different
// answers when deciding whether to create something.
var ErrNotFound = errors.New("not found")

// get fetches path and fails on any 4xx/5xx.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	status, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("bindplane GET %s: %w", path, ErrNotFound)
	}
	if status >= 400 {
		return nil, fmt.Errorf("bindplane GET %s: %d %s", path, status, truncate(string(body), 500))
	}
	return body, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Ping verifies the server, the credential and the project scope with a
// one-agent listing.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/agents?limit=1&offset=0", nil)
	if err != nil {
		return err
	}
	status, body, err := c.do(req)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("%d %s", status, truncate(string(body), 200))
	}
	return nil
}

// ---------------------------------------------------------------- projects

// Project is one Bindplane project (an "account" in the older API).
type Project struct {
	ID          string
	Name        string
	DisplayName string
}

func (p Project) Title() string {
	switch {
	case p.DisplayName != "":
		return p.DisplayName
	case p.Name != "":
		return p.Name
	}
	return p.ID
}

// ListProjects returns the projects the credential can reach, so the profile
// form can offer a picker instead of asking for a pasted ID.
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	body, err := c.get(ctx, "/v1/projects")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Accounts []struct {
			Metadata struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
			} `json:"metadata"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode projects: %w", err)
	}
	out := make([]Project, 0, len(resp.Accounts))
	for _, a := range resp.Accounts {
		out = append(out, Project{a.Metadata.ID, a.Metadata.Name, a.Metadata.DisplayName})
	}
	return out, nil
}

// ------------------------------------------------------------------ agents

// Agent is the subset of a Bindplane collector the labeler cares about.
type Agent struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	HostName  string            `json:"hostname"`
	Type      string            `json:"type"`
	Version   string            `json:"version"`
	Platform  string            `json:"platform"`
	Connected bool              `json:"connected"`
	Labels    map[string]string `json:"labels"`
}

// Fleet is a Bindplane fleet. Agents join a fleet through the "fleet" label,
// whose value is the fleet's resource Name (not its DisplayName).
type Fleet struct {
	Name          string
	DisplayName   string
	Configuration string
}

func (f Fleet) Title() string {
	if f.DisplayName != "" {
		return f.DisplayName
	}
	return f.Name
}

// BulkResult is the response of PATCH /v1/agents/labels.
type BulkResult struct {
	Errors    []string `json:"errors"`
	Count     int64    `json:"count"`
	FailedIDs []string `json:"failedIds,omitempty"`
}

const pageSize = 500

// ListAgents returns every agent matching query and selector, paging until
// the server stops returning new agents. Empty query and selector list all.
func (c *Client) ListAgents(ctx context.Context, query, selector string) ([]Agent, error) {
	var out []Agent
	seen := map[string]bool{}
	for offset := 0; ; {
		q := url.Values{}
		q.Set("query", query)
		q.Set("selector", selector)
		q.Set("offset", strconv.Itoa(offset))
		q.Set("limit", strconv.Itoa(pageSize))
		body, err := c.get(ctx, "/v1/agents?"+q.Encode())
		if err != nil {
			return nil, err
		}
		var page struct {
			Agents []Agent `json:"agents"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode agents: %w", err)
		}
		added := 0
		for _, a := range page.Agents {
			if !seen[a.ID] {
				seen[a.ID] = true
				out = append(out, a)
				added++
			}
		}
		// Stop on an empty page, or if the server ignored offset and
		// handed back agents we already have.
		if added == 0 {
			return out, nil
		}
		offset += len(page.Agents)
	}
}

// ListFleets returns every fleet.
func (c *Client) ListFleets(ctx context.Context) ([]Fleet, error) {
	var out []Fleet
	seen := map[string]bool{}
	for offset := 0; ; {
		q := url.Values{}
		q.Set("offset", strconv.Itoa(offset))
		q.Set("limit", strconv.Itoa(pageSize))
		body, err := c.get(ctx, "/v1/fleets?"+q.Encode())
		if err != nil {
			return nil, err
		}
		var page struct {
			Fleets []struct {
				Metadata struct {
					Name        string `json:"name"`
					DisplayName string `json:"displayName"`
				} `json:"metadata"`
				Spec struct {
					Configuration string `json:"configuration"`
				} `json:"spec"`
			} `json:"fleets"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode fleets: %w", err)
		}
		added := 0
		for _, f := range page.Fleets {
			if !seen[f.Metadata.Name] {
				seen[f.Metadata.Name] = true
				out = append(out, Fleet{f.Metadata.Name, f.Metadata.DisplayName, f.Spec.Configuration})
				added++
			}
		}
		if added == 0 {
			return out, nil
		}
		offset += len(page.Fleets)
	}
}

// LabelAgents applies labels to the given agent IDs. An empty value deletes
// the label. overwrite must be true when changing or deleting an existing
// label.
func (c *Client) LabelAgents(ctx context.Context, ids []string, labels map[string]string, overwrite bool) (BulkResult, error) {
	payload := map[string]any{
		"ids":              ids,
		"labels":           labels,
		"overwrite":        overwrite,
		"includeFailedIds": true,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return BulkResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+"/v1/agents/labels", bytes.NewReader(data))
	if err != nil {
		return BulkResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, body, err := c.do(req)
	if err != nil {
		return BulkResult{}, err
	}
	var res BulkResult
	jsonErr := json.Unmarshal(body, &res)
	// 409 carries a partial-failure body in the same shape.
	if status >= 400 && status != http.StatusConflict {
		return res, fmt.Errorf("bindplane PATCH /v1/agents/labels: %d %s", status, truncate(string(body), 500))
	}
	if jsonErr != nil {
		return res, fmt.Errorf("decode label response: %w", jsonErr)
	}
	if len(res.Errors) > 0 {
		return res, fmt.Errorf("%s", strings.Join(res.Errors, "; "))
	}
	return res, nil
}

// AgentVersion is a collector version Bindplane offers for install/upgrade.
type AgentVersion struct {
	Type            string
	Version         string
	Prerelease      bool
	ReleaseNotesURL string
}

// ListAgentVersions returns the versions Bindplane has available, excluding drafts.
func (c *Client) ListAgentVersions(ctx context.Context) ([]AgentVersion, error) {
	body, err := c.get(ctx, "/v1/agent-versions")
	if err != nil {
		return nil, err
	}
	var resp struct {
		AgentVersions []struct {
			Spec struct {
				Type            string `json:"type"`
				Version         string `json:"version"`
				ReleaseNotesURL string `json:"releaseNotesURL"`
				Draft           bool   `json:"draft"`
				Prerelease      bool   `json:"prerelease"`
			} `json:"spec"`
		} `json:"agentVersions"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode agent versions: %w", err)
	}
	var out []AgentVersion
	for _, v := range resp.AgentVersions {
		if v.Spec.Draft || v.Spec.Version == "" {
			continue
		}
		out = append(out, AgentVersion{v.Spec.Type, v.Spec.Version, v.Spec.Prerelease, v.Spec.ReleaseNotesURL})
	}
	return out, nil
}

// UpgradeAgents requests an upgrade of the given agents to version. Bindplane
// answers 204 when the request is accepted; it silently skips agents that
// don't exist or don't support upgrades.
func (c *Client) UpgradeAgents(ctx context.Context, ids []string, version string) error {
	data, err := json.Marshal(map[string]any{"ids": ids, "version": version})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+"/v1/agents/version", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	status, body, err := c.do(req)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("bindplane PATCH /v1/agents/version: %d %s", status, truncate(string(body), 500))
	}
	return nil
}

// --------------------------------------------------------------- resources

// Kind is a Bindplane resource kind, named by its REST collection.
type Kind string

const (
	KindConfiguration Kind = "configurations"
	KindSource        Kind = "sources"
	KindProcessor     Kind = "processors"
	KindDestination   Kind = "destinations"
	KindExtension     Kind = "extensions"

	// The *Type kinds hold the definitions a resource is built on. A
	// configuration cannot render against a server that lacks them, which
	// is why they travel with a migration.
	KindSourceType      Kind = "source-types"
	KindProcessorType   Kind = "processor-types"
	KindDestinationType Kind = "destination-types"
	KindExtensionType   Kind = "extension-types"
)

// ResourceKinds are the kinds the migrator walks, in apply order: the type
// definitions first, then the resources built on them, then the
// configurations that reference those. Nothing is ever applied before what
// it depends on.
//
// Connectors are absent on purpose: Bindplane has no /v1/connectors
// collection, so a connector is always defined inline and travels with the
// configuration that holds it.
var ResourceKinds = []Kind{
	KindSourceType, KindProcessorType, KindDestinationType, KindExtensionType,
	KindSource, KindProcessor, KindDestination, KindExtension,
	KindConfiguration,
}

// ResourceTypeKind is the kind holding the type definitions that resources
// of kind k are built on, or "" when k has none.
func ResourceTypeKind(k Kind) Kind {
	switch k {
	case KindSource:
		return KindSourceType
	case KindProcessor:
		return KindProcessorType
	case KindDestination:
		return KindDestinationType
	case KindExtension:
		return KindExtensionType
	}
	return ""
}

// IsResourceTypeKind reports whether k holds type definitions rather than
// the resources built on them.
func IsResourceTypeKind(k Kind) bool {
	switch k {
	case KindSourceType, KindProcessorType, KindDestinationType, KindExtensionType:
		return true
	}
	return false
}

// TrimVersion drops the ":version" suffix from a type reference such as
// "googlecloud:3". Bindplane resolves a component's type by the bare name,
// so that is what we fetch.
func TrimVersion(typeRef string) string {
	if i := strings.Index(typeRef, ":"); i >= 0 {
		return typeRef[:i]
	}
	return typeRef
}

// Resource is a Bindplane resource kept as a raw map so unknown fields
// round-trip intact through a read/apply cycle.
type Resource map[string]any

func (r Resource) meta() map[string]any {
	m, _ := r["metadata"].(map[string]any)
	return m
}

func (r Resource) metaString(key string) string {
	s, _ := r.meta()[key].(string)
	return s
}

// Name is the resource's stable identifier, the one other resources
// reference it by.
func (r Resource) Name() string { return r.metaString("name") }

// DisplayName is the UI label, which may be empty.
func (r Resource) DisplayName() string { return r.metaString("displayName") }

// Title prefers the display name, falling back to the name.
func (r Resource) Title() string {
	if d := r.DisplayName(); d != "" {
		return d
	}
	return r.Name()
}

// Kind is the resource's own "kind" field, e.g. "Configuration".
func (r Resource) Kind() string {
	s, _ := r["kind"].(string)
	return s
}

// Spec is the resource's spec map, or nil.
func (r Resource) Spec() map[string]any {
	m, _ := r["spec"].(map[string]any)
	return m
}

// StripServerFields removes the identity and bookkeeping fields the server
// assigns, so the resource can be applied into a different project without
// colliding with its origin or triggering a version conflict.
func (r Resource) StripServerFields() {
	delete(r, "status")
	m := r.meta()
	for _, k := range []string{"id", "version", "dateModified", "hash", "rolloutStatus"} {
		delete(m, k)
	}
}

// ListResources returns every resource of kind.
func (c *Client) ListResources(ctx context.Context, kind Kind) ([]Resource, error) {
	body, err := c.get(ctx, "/v1/"+string(kind))
	if err != nil {
		return nil, err
	}
	// Each collection answers with its own plural key, so decode generically
	// and take the first list of objects we find.
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode %s: %w", kind, err)
	}
	for _, v := range envelope {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		out := make([]Resource, 0, len(list))
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				out = append(out, Resource(m))
			}
		}
		return out, nil
	}
	return nil, nil
}

// GetResource fetches one resource by name.
func (c *Client) GetResource(ctx context.Context, kind Kind, name string) (Resource, error) {
	body, err := c.get(ctx, "/v1/"+string(kind)+"/"+url.PathEscape(name))
	if err != nil {
		return nil, err
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w", kind, name, err)
	}
	// A single resource comes back wrapped under its singular kind name.
	for _, v := range envelope {
		if m, ok := v.(map[string]any); ok {
			if _, hasMeta := m["metadata"]; hasMeta {
				return Resource(m), nil
			}
		}
	}
	if _, hasMeta := envelope["metadata"]; hasMeta {
		return Resource(envelope), nil
	}
	return nil, fmt.Errorf("%s %q: unexpected response shape", kind, name)
}

// GetConfigurationRaw fetches a configuration and returns the raw JSON body,
// for callers that round-trip it as YAML.
func (c *Client) GetConfigurationRaw(ctx context.Context, name string) ([]byte, error) {
	return c.get(ctx, "/v1/configurations/"+url.PathEscape(name))
}

// ApplyStatus is the per-resource outcome Bindplane reports from an apply.
type ApplyStatus struct {
	Resource Resource `json:"resource"`
	Status   string   `json:"status"`
	Reason   string   `json:"reason,omitempty"`
}

// Apply posts resources to /v1/apply and returns the per-resource statuses.
func (c *Client) Apply(ctx context.Context, resources []Resource) ([]ApplyStatus, error) {
	payload := map[string]any{"resources": resources}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/apply", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("bindplane POST /v1/apply: %d %s", status, truncate(string(body), 500))
	}
	var resp struct {
		Updates []ApplyStatus `json:"updates"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode apply response: %w", err)
	}
	// Bindplane reports per-resource failures in the body, not the status code.
	var failed []string
	for _, u := range resp.Updates {
		if strings.EqualFold(u.Status, "invalid") || strings.EqualFold(u.Status, "error") {
			failed = append(failed, fmt.Sprintf("%s: %s", u.Resource.Title(), u.Reason))
		}
	}
	if len(failed) > 0 {
		return resp.Updates, fmt.Errorf("%s", strings.Join(failed, "; "))
	}
	return resp.Updates, nil
}
