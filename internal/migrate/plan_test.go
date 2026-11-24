package migrate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bpkit/internal/bpclient"
)

// fakeBP serves a tiny Bindplane: resources keyed by "kind/name", and a
// record of what was applied to it.
type fakeBP struct {
	t        *testing.T
	res      map[string]map[string]any
	applied  [][]map[string]any
	accountH []string
	srv      *httptest.Server
}

func newFakeBP(t *testing.T) *fakeBP {
	f := &fakeBP{t: t, res: map[string]map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.accountH = append(f.accountH, r.Header.Get("X-Bindplane-Account-ID"))
		switch {
		case r.URL.Path == "/v1/apply":
			var body struct {
				Resources []map[string]any `json:"resources"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			f.applied = append(f.applied, body.Resources)
			var updates []map[string]any
			for _, res := range body.Resources {
				updates = append(updates, map[string]any{"resource": res, "status": "created"})
			}
			json.NewEncoder(w).Encode(map[string]any{"updates": updates})
		case strings.HasPrefix(r.URL.Path, "/v1/"):
			parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/v1/"), "/", 2)
			if len(parts) != 2 { // a collection listing
				w.Write([]byte(`{"` + parts[0] + `":[]}`))
				return
			}
			res, ok := f.res[parts[0]+"/"+parts[1]]
			if !ok {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			// A single resource comes back wrapped under its singular kind.
			singular := strings.TrimSuffix(parts[0], "s")
			json.NewEncoder(w).Encode(map[string]any{singular: res})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBP) put(kind, name string, res map[string]any) {
	meta, _ := res["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		res["metadata"] = meta
	}
	meta["name"] = name
	f.res[kind+"/"+name] = res
}

func (f *fakeBP) client(t *testing.T, project string) *bpclient.Client {
	t.Helper()
	c, err := bpclient.New(bpclient.Options{BaseURL: f.srv.URL, APIKey: "k", Project: project})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// config builds a configuration whose sources list holds the given entries.
func config(name string, sources ...map[string]any) map[string]any {
	list := make([]any, 0, len(sources))
	for _, s := range sources {
		list = append(list, s)
	}
	return map[string]any{
		"apiVersion": "bindplane.observiq.com/v1",
		"kind":       "Configuration",
		"metadata":   map[string]any{"name": name, "displayName": name, "id": "srv-id", "version": 7},
		"spec":       map[string]any{"sources": list},
	}
}

func ref(name string) map[string]any { return map[string]any{"name": name} }

func inline(typ string, params ...map[string]any) map[string]any {
	ps := make([]any, 0, len(params))
	for _, p := range params {
		ps = append(ps, p)
	}
	return map[string]any{"name": "s-generated-id", "type": typ, "parameters": ps}
}

func param(name string, value any) map[string]any {
	return map[string]any{"name": name, "value": value}
}

func TestBuildFollowsReferences(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	// A configuration referencing a library source, which in turn
	// references a library processor.
	src.put("configurations", "cfg1", config("cfg1", ref("src1"), inline("otlp")))
	src.put("sources", "src1", map[string]any{
		"kind": "Source",
		"spec": map[string]any{
			"type":       "windows_event",
			"processors": []any{ref("proc1")},
		},
	})
	src.put("processors", "proc1", map[string]any{"kind": "Processor", "spec": map[string]any{"type": "batch"}})

	p, err := Build(context.Background(), src.client(t, ""), dst.client(t, ""), []string{"cfg1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 0 {
		t.Fatalf("warnings: %v", p.Warnings)
	}

	got := map[string]bpclient.Kind{}
	for _, it := range p.Items {
		got[it.Title()] = it.Kind
	}
	for name, want := range map[string]bpclient.Kind{
		"cfg1":  bpclient.KindConfiguration,
		"src1":  bpclient.KindSource,
		"proc1": bpclient.KindProcessor,
	} {
		if got[name] != want {
			t.Fatalf("%s: kind %q, want %q (items %+v)", name, got[name], want, got)
		}
	}
	if len(p.Items) != 3 {
		t.Fatalf("want 3 items (the inline source travels with its config), got %d", len(p.Items))
	}

	// Dependencies must be ordered before the configuration that needs them.
	cfgAt := -1
	for i, it := range p.Items {
		if it.Kind == bpclient.KindConfiguration {
			cfgAt = i
		}
	}
	if cfgAt != len(p.Items)-1 {
		t.Fatalf("configuration must come last, found at %d of %d", cfgAt, len(p.Items))
	}

	// Everything is new in an empty destination, so everything applies.
	apply, skip, conflict := p.Counts()
	if apply != 3 || skip != 0 || conflict != 0 {
		t.Fatalf("counts: apply=%d skip=%d conflict=%d", apply, skip, conflict)
	}
	// Server-assigned identity must not travel.
	for _, it := range p.Items {
		meta, _ := it.Resource["metadata"].(map[string]any)
		if _, ok := meta["id"]; ok {
			t.Fatalf("%s still carries metadata.id", it.Title())
		}
		if _, ok := meta["version"]; ok {
			t.Fatalf("%s still carries metadata.version", it.Title())
		}
	}
}

func TestBuildStatusAgainstDestination(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	src.put("configurations", "same", config("same", inline("otlp")))
	src.put("configurations", "diff", config("diff", inline("otlp")))

	// The destination already has both: one identical, one changed.
	dst.put("configurations", "same", config("same", inline("otlp")))
	dst.put("configurations", "diff", config("diff", inline("filelog")))

	p, err := Build(context.Background(), src.client(t, ""), dst.client(t, ""), []string{"same", "diff"})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Item{}
	for _, it := range p.Items {
		byName[it.Title()] = it
	}
	if got := byName["same"].Status; got != StatusSame {
		t.Fatalf("same: %v", got)
	}
	if got := byName["diff"].Status; got != StatusConflict {
		t.Fatalf("diff: %v", got)
	}
	if d := byName["diff"].Diff; len(d) != 1 || d[0] != "sources" {
		t.Fatalf("diff fields: %v", d)
	}
	// Neither applies without the user saying so.
	for _, it := range p.Items {
		if !it.Skip {
			t.Fatalf("%s (%v) must start skipped", it.Title(), it.Status)
		}
	}
	apply, skip, conflict := p.Counts()
	if apply != 0 || skip != 2 || conflict != 1 {
		t.Fatalf("counts: apply=%d skip=%d conflict=%d", apply, skip, conflict)
	}
}

func TestSecretsAreFoundAndWrittenBack(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	src.put("configurations", "cfg", config("cfg",
		inline("splunk", param("token", SensitivePlaceholder), param("endpoint", "https://x")),
	))
	src.put("destinations", "dest1", map[string]any{
		"kind": "Destination",
		"spec": map[string]any{"type": "s3", "parameters": []any{param("secret_key", SensitivePlaceholder)}},
	})
	// Reference the destination from the configuration.
	cfg := src.res["configurations/cfg"]
	cfg["spec"].(map[string]any)["destinations"] = []any{ref("dest1")}

	p, err := Build(context.Background(), src.client(t, ""), dst.client(t, ""), []string{"cfg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Secrets) != 2 {
		for _, s := range p.Secrets {
			t.Logf("secret %s %s", s.Where, s.Param)
		}
		t.Fatalf("want 2 secrets, got %d", len(p.Secrets))
	}
	if len(p.Unfilled()) != 2 {
		t.Fatalf("all should start unfilled, got %d", len(p.Unfilled()))
	}
	for _, s := range p.Secrets {
		s.Set("real-" + s.Param)
	}
	if n := len(p.Unfilled()); n != 0 {
		t.Fatalf("%d still unfilled", n)
	}

	// Setting a secret must reach the resource that gets applied.
	if _, err := p.Apply(context.Background(), dst.client(t, "")); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for _, batch := range dst.applied {
		data, _ := json.Marshal(batch)
		body.Write(data)
	}
	for _, want := range []string{"real-token", "real-secret_key"} {
		if !strings.Contains(body.String(), want) {
			t.Fatalf("applied body missing %q: %s", want, body.String())
		}
	}
	if strings.Contains(body.String(), SensitivePlaceholder) {
		t.Fatalf("placeholder was applied: %s", body.String())
	}
}

func TestApplyOrdersKindsAndSkips(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	src.put("configurations", "cfg", config("cfg", ref("src1")))
	src.put("sources", "src1", map[string]any{"kind": "Source", "spec": map[string]any{"type": "otlp"}})

	p, err := Build(context.Background(), src.client(t, ""), dst.client(t, ""), []string{"cfg"})
	if err != nil {
		t.Fatal(err)
	}
	// Skip the source; only the configuration should be sent.
	for i := range p.Items {
		if p.Items[i].Kind == bpclient.KindSource {
			p.Items[i].Skip = true
		}
	}
	res, err := p.Apply(context.Background(), dst.client(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Kind != bpclient.KindConfiguration {
		t.Fatalf("results: %+v", res)
	}
	if len(dst.applied) != 1 || len(dst.applied[0]) != 1 {
		t.Fatalf("applied: %+v", dst.applied)
	}
}

func TestMissingDependencyWarnsInsteadOfFailing(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	src.put("configurations", "cfg", config("cfg", ref("gone")))

	p, err := Build(context.Background(), src.client(t, ""), dst.client(t, ""), []string{"cfg"})
	if err != nil {
		t.Fatalf("a missing dependency must not fail the build: %v", err)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "gone") {
		t.Fatalf("warnings: %v", p.Warnings)
	}
	if len(p.Items) != 1 {
		t.Fatalf("items: %d", len(p.Items))
	}
}

// The project must be sent on every request, since a scoped API key is
// rejected without it.
func TestProjectHeaderIsSent(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	src.put("configurations", "cfg", config("cfg"))

	p, err := Build(context.Background(), src.client(t, "proj-src"), dst.client(t, "proj-dst"), []string{"cfg"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(context.Background(), dst.client(t, "proj-dst")); err != nil {
		t.Fatal(err)
	}
	for _, h := range src.accountH {
		if h != "proj-src" {
			t.Fatalf("source saw account header %q", h)
		}
	}
	if len(dst.accountH) == 0 {
		t.Fatal("destination was never called")
	}
	for _, h := range dst.accountH {
		if h != "proj-dst" {
			t.Fatalf("destination saw account header %q", h)
		}
	}
}

// A client with no project must not send the header at all, so an unscoped
// key keeps using the project it carries.
func TestNoProjectNoHeader(t *testing.T) {
	f := newFakeBP(t)
	f.put("configurations", "cfg", config("cfg"))
	if _, err := f.client(t, "").GetResource(context.Background(), bpclient.KindConfiguration, "cfg"); err != nil {
		t.Fatal(err)
	}
	for _, h := range f.accountH {
		if h != "" {
			t.Fatalf("header sent without a project: %q", h)
		}
	}
}
