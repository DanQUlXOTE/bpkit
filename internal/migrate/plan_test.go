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

// putType registers a type definition, the way a seeded Bindplane has them.
// collection is the REST collection, e.g. "destination-types".
func (f *fakeBP) putType(collection, name string) {
	f.put(collection, name, map[string]any{
		"kind": typeKind(collection),
		"spec": map[string]any{"parameters": []any{}},
	})
}

// typeKind turns "destination-types" into "DestinationType".
func typeKind(collection string) string {
	var b strings.Builder
	for _, word := range strings.Split(strings.TrimSuffix(collection, "s"), "-") {
		b.WriteString(strings.ToUpper(word[:1]) + word[1:])
	}
	return b.String()
}

// put stores a resource under the given lookup key. The key may carry a
// version pin; metadata.name is left alone when the caller set it, so a
// fixture can model a server that answers a pinned read with a bare name.
func (f *fakeBP) put(key, lookup string, res map[string]any) {
	meta, _ := res["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		res["metadata"] = meta
	}
	if n, _ := meta["name"].(string); n == "" {
		meta["name"] = lookup
	}
	f.res[key+"/"+lookup] = res
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
	src.putType("source-types", "windows_event")
	src.putType("source-types", "otlp")
	src.putType("processor-types", "batch")

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
		"cfg1":          bpclient.KindConfiguration,
		"src1":          bpclient.KindSource,
		"proc1":         bpclient.KindProcessor,
		"windows_event": bpclient.KindSourceType,
		"otlp":          bpclient.KindSourceType,
		"batch":         bpclient.KindProcessorType,
	} {
		if got[name] != want {
			t.Fatalf("%s: kind %q, want %q (items %+v)", name, got[name], want, got)
		}
	}
	// 3 resources plus the 3 type definitions they are built on.
	if len(p.Items) != 6 {
		t.Fatalf("want 6 items, got %d: %+v", len(p.Items), got)
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

	// The 3 resources apply; the 3 type definitions are the server's to
	// seed and start skipped.
	apply, skip, conflict := p.Counts()
	if apply != 3 || skip != 3 || conflict != 0 {
		t.Fatalf("counts: apply=%d skip=%d conflict=%d", apply, skip, conflict)
	}
	for _, it := range p.Items {
		if it.TypeDef != bpclient.IsResourceTypeKind(it.Kind) {
			t.Fatalf("%s: TypeDef=%v for kind %s", it.Title(), it.TypeDef, it.Kind)
		}
		if it.TypeDef && !it.Skip {
			t.Fatalf("type definition %q must start skipped", it.Title())
		}
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
	src.putType("destination-types", "splunk")
	src.putType("destination-types", "s3")
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
	src.putType("source-types", "otlp")

	p, err := Build(context.Background(), src.client(t, ""), dst.client(t, ""), []string{"cfg"})
	if err != nil {
		t.Fatal(err)
	}
	// Skip the source and its type; only the configuration should be sent.
	for i := range p.Items {
		if p.Items[i].Kind != bpclient.KindConfiguration {
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

// A library destination is built on a DestinationType. If that type is
// missing in the destination project the apply fails with
// "unknown DestinationType: dev_null", so the type must travel too — and
// must be applied before the destination that needs it.
func TestLibraryResourceTypesTravel(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	cfg := config("Dan Demo")
	cfg["spec"].(map[string]any)["destinations"] = []any{
		ref("devnull"), ref("GoogleCloud"), ref("NFR"),
	}
	src.put("configurations", "Dan Demo", cfg)
	for name, typ := range map[string]string{
		"devnull":     "dev_null",
		"GoogleCloud": "googlecloud",
		"NFR":         "chronicle:2", // a pinned version still resolves by bare name
	} {
		src.put("destinations", name, map[string]any{
			"kind": "Destination",
			"spec": map[string]any{"type": typ},
		})
	}
	src.putType("destination-types", "dev_null")
	src.putType("destination-types", "googlecloud")
	src.putType("destination-types", "chronicle")

	p, err := Build(context.Background(), src.client(t, ""), dst.client(t, ""), []string{"Dan Demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 0 {
		t.Fatalf("warnings: %v", p.Warnings)
	}

	types := map[string]bool{}
	for _, it := range p.Items {
		if it.Kind == bpclient.KindDestinationType {
			types[it.Title()] = true
		}
	}
	for _, want := range []string{"dev_null", "googlecloud", "chronicle"} {
		if !types[want] {
			t.Fatalf("DestinationType %q did not travel; got %v", want, types)
		}
	}

	// They start skipped, because the server seeds its own built-in types
	// and applying one over the JSON API can fail validation.
	for _, it := range p.Items {
		if it.TypeDef && !it.Skip {
			t.Fatalf("type %q must start skipped", it.Title())
		}
		if it.TypeDef && it.Note == "" {
			t.Fatalf("type %q should explain why it is skipped", it.Title())
		}
	}

	// Opting in applies them, ahead of the destinations that need them.
	for i := range p.Items {
		p.Items[i].Skip = false
	}
	if _, err := p.Apply(context.Background(), dst.client(t, "")); err != nil {
		t.Fatal(err)
	}
	// Types must land in an earlier batch than the destinations using them.
	typeBatch, destBatch := -1, -1
	for i, batch := range dst.applied {
		for _, r := range batch {
			switch bpclient.Resource(r).Kind() {
			case "DestinationType":
				if typeBatch < 0 {
					typeBatch = i
				}
			case "Destination":
				if destBatch < 0 {
					destBatch = i
				}
			}
		}
	}
	if typeBatch < 0 || destBatch < 0 {
		t.Fatalf("batches: type=%d dest=%d (%d batches)", typeBatch, destBatch, len(dst.applied))
	}
	if typeBatch >= destBatch {
		t.Fatalf("types must apply before the destinations that need them: type batch %d, destination batch %d", typeBatch, destBatch)
	}
}

// A connector has no /v1/connectors collection, so it must be walked for
// its type rather than fetched as a library resource.
func TestConnectorsAreNotFetched(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	cfg := config("cfg")
	cfg["spec"].(map[string]any)["connectors"] = []any{
		map[string]any{"name": "c-inline", "type": "routing"},
	}
	src.put("configurations", "cfg", cfg)

	p, err := Build(context.Background(), src.client(t, ""), dst.client(t, ""), []string{"cfg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 0 {
		t.Fatalf("a connector must not be fetched as a library resource: %v", p.Warnings)
	}
	if len(p.Items) != 1 {
		t.Fatalf("only the configuration should travel, got %d items", len(p.Items))
	}
}

// A destination that cannot be read is not the same as one that has no such
// resource: assuming "absent" would turn a permissions or decoding problem
// into an unintended create.
func TestUnreadableDestinationIsNotTreatedAsNew(t *testing.T) {
	src := newFakeBP(t)
	src.put("configurations", "cfg", config("cfg"))

	// A destination that answers 500 to every read.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	defer broken.Close()
	dst, err := bpclient.New(bpclient.Options{BaseURL: broken.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}

	p, err := Build(context.Background(), src.client(t, ""), dst, []string{"cfg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 1 {
		t.Fatalf("items: %d", len(p.Items))
	}
	it := p.Items[0]
	if it.Status != StatusUnknown {
		t.Fatalf("status = %v, want unreadable", it.Status)
	}
	if !it.Skip {
		t.Fatal("an unreadable resource must not be applied by default")
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "could not read the destination") {
		t.Fatalf("warnings: %v", p.Warnings)
	}
	if apply, _, _ := p.Counts(); apply != 0 {
		t.Fatalf("apply=%d, want 0", apply)
	}
}

// A configuration pins its library references to a version ("Aurepoll:17").
// The destination's fresh copy is at version 1, so a pinned reference cannot
// resolve there and the render fails with "unknown Source: Aurepoll:17".
// References must arrive bare, tracking whatever version the destination
// assigned.
func TestVersionPinsAreStrippedFromReferences(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	cfg := config("Dan Demo", ref("Aurepoll:17"))
	cfg["spec"].(map[string]any)["destinations"] = []any{ref("devnull:9")}
	src.put("configurations", "Dan Demo", cfg)
	// The source serves the pinned reads; metadata.name stays bare.
	src.put("sources", "Aurepoll:17", map[string]any{
		"kind":     "Source",
		"metadata": map[string]any{"name": "Aurepoll"},
		"spec":     map[string]any{"type": "azure_blob_polling"},
	})
	src.put("destinations", "devnull:9", map[string]any{
		"kind":     "Destination",
		"metadata": map[string]any{"name": "devnull"},
		"spec":     map[string]any{"type": "dev_null"},
	})

	p, err := Build(context.Background(), src.client(t, ""), dst.client(t, ""), []string{"Dan Demo"})
	if err != nil {
		t.Fatal(err)
	}

	// The pinned versions were read from the source: that is the content the
	// configuration actually uses.
	names := map[string]bool{}
	for _, it := range p.Items {
		names[string(it.Kind)+"/"+it.Resource.Name()] = true
	}
	for _, want := range []string{"sources/Aurepoll", "destinations/devnull"} {
		if !names[want] {
			t.Fatalf("%s did not travel; got %v", want, names)
		}
	}

	// The configuration's references must be bare.
	var cfgItem Item
	for _, it := range p.Items {
		if it.Kind == bpclient.KindConfiguration {
			cfgItem = it
		}
	}
	spec := cfgItem.Resource.Spec()
	srcRef := spec["sources"].([]any)[0].(map[string]any)["name"]
	dstRef := spec["destinations"].([]any)[0].(map[string]any)["name"]
	if srcRef != "Aurepoll" {
		t.Fatalf("source reference = %q, want %q", srcRef, "Aurepoll")
	}
	if dstRef != "devnull" {
		t.Fatalf("destination reference = %q, want %q", dstRef, "devnull")
	}

	// And the user is told the pins were dropped.
	var pinned int
	for _, w := range p.Warnings {
		if strings.Contains(w, "current version") {
			pinned++
		}
	}
	if pinned != 2 {
		t.Fatalf("want a warning per dropped pin, got %d: %v", pinned, p.Warnings)
	}

	// Nothing with a ":" survives into what is applied.
	for i := range p.Items {
		p.Items[i].Skip = p.Items[i].TypeDef
	}
	if _, err := p.Apply(context.Background(), dst.client(t, "")); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(dst.applied)
	for _, bad := range []string{"Aurepoll:17", "devnull:9"} {
		if strings.Contains(string(body), bad) {
			t.Fatalf("pinned reference %q was applied: %s", bad, body)
		}
	}
}

// An inline component's own references are pinned the same way and need the
// same treatment.
func TestVersionPinsStrippedInsideInlineComponents(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	inlineSrc := inline("otlp")
	inlineSrc["processors"] = []any{ref("scrub:4")}
	src.put("configurations", "cfg", config("cfg", inlineSrc))
	src.put("processors", "scrub:4", map[string]any{
		"kind":     "Processor",
		"metadata": map[string]any{"name": "scrub"},
		"spec":     map[string]any{"type": "batch"},
	})

	p, err := Build(context.Background(), src.client(t, ""), dst.client(t, ""), []string{"cfg"})
	if err != nil {
		t.Fatal(err)
	}
	var cfgItem Item
	for _, it := range p.Items {
		if it.Kind == bpclient.KindConfiguration {
			cfgItem = it
		}
	}
	got := cfgItem.Resource.Spec()["sources"].([]any)[0].(map[string]any)["processors"].([]any)[0].(map[string]any)["name"]
	if got != "scrub" {
		t.Fatalf("nested reference = %q, want %q", got, "scrub")
	}
}

// Defensive: if the server answers a pinned read by echoing the pinned name
// back in metadata.name, the copy must still be created under the bare name
// rather than one containing a colon.
func TestPinnedNameNeverBecomesTheResourceName(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	src.put("configurations", "cfg", config("cfg", ref("Aurepoll:17")))
	src.put("sources", "Aurepoll:17", map[string]any{
		"kind": "Source",
		// no explicit metadata.name: the fixture echoes the pinned lookup
		"spec": map[string]any{"type": "otlp"},
	})

	p, err := Build(context.Background(), src.client(t, ""), dst.client(t, ""), []string{"cfg"})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range p.Items {
		if strings.Contains(it.Resource.Name(), ":") {
			t.Fatalf("resource name carries a version pin: %q", it.Resource.Name())
		}
	}
}
