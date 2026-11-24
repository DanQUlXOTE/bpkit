package migrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bpkit/internal/bpclient"
	"bpkit/internal/ext"
	"bpkit/internal/migrate"
	"bpkit/internal/profiles"

	tea "github.com/charmbracelet/bubbletea"
)

type fakeBP struct {
	res     map[string]map[string]any
	applied [][]map[string]any
	srv     *httptest.Server
}

func newFakeBP(t *testing.T) *fakeBP {
	f := &fakeBP{res: map[string]map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/apply" {
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
			return
		}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/v1/"), "/", 2)
		collection := parts[0]
		if i := strings.Index(collection, "?"); i >= 0 {
			collection = collection[:i]
		}
		if len(parts) == 1 { // listing
			var list []any
			for k, v := range f.res {
				if strings.HasPrefix(k, collection+"/") {
					list = append(list, v)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{collection: list})
			return
		}
		res, ok := f.res[collection+"/"+parts[1]]
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{strings.TrimSuffix(collection, "s"): res})
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
	if _, ok := meta["displayName"]; !ok {
		meta["displayName"] = name
	}
	f.res[kind+"/"+name] = res
}

// send delivers messages to the model and feeds back any data message the
// resulting commands produce. Blink timers never resolve in time and are
// dropped, which keeps the test fast.
func send(m Model, msgs ...tea.Msg) Model {
	for _, msg := range msgs {
		nm, cmd := m.Update(msg)
		m = nm.(Model)
		for cmd != nil {
			out, ok := runCmd(cmd)
			if !ok || out == nil {
				break
			}
			switch out.(type) {
			case configsMsg, planMsg, appliedMsg:
				nm, cmd = m.Update(out)
				m = nm.(Model)
			default:
				cmd = nil
			}
		}
	}
	return m
}

// runCmd runs cmd, giving up on anything slower than a local API call. The
// cursor-blink timer ticks every ~530ms and would otherwise dominate the
// test's runtime; the real commands here talk to an httptest server.
func runCmd(cmd tea.Cmd) (tea.Msg, bool) {
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		return msg, true
	case <-time.After(250 * time.Millisecond):
		return nil, false
	}
}

func key(s string) tea.Msg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "space":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// typeIn types into the focused input. Each keystroke's only command is the
// cursor blink, so there is nothing worth running.
func typeIn(m Model, s string) Model {
	for _, r := range s {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(Model)
	}
	return m
}

// setup wires a migrator whose source is srcBP and whose profile list offers
// a destination pointing at dstBP.
func setup(t *testing.T, srcBP, dstBP *fakeBP) Model {
	t.Helper()
	store := profiles.NewStore(filepath.Join(t.TempDir(), "p.json"))
	if err := store.Save("", profiles.Profile{Name: "dest", Server: dstBP.srv.URL, APIKey: "k", Project: "proj-dst"}); err != nil {
		t.Fatal(err)
	}
	srcClient, err := bpclient.New(bpclient.Options{BaseURL: srcBP.srv.URL, APIKey: "k", Project: "proj-src"})
	if err != nil {
		t.Fatal(err)
	}
	return New(ext.Context{
		Client: srcClient,
		Active: profiles.Profile{Name: "source", Server: srcBP.srv.URL, APIKey: "k", Project: "proj-src"},
		Store:  store,
		Dial: func(p profiles.Profile) (*bpclient.Client, error) {
			return bpclient.New(bpclient.Options{BaseURL: p.Server, APIKey: p.APIKey, Project: p.Project})
		},
		Width: 120, Height: 40,
	})
}

func config(name string, spec map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "bindplane.observiq.com/v1",
		"kind":       "Configuration",
		"metadata":   map[string]any{"name": name, "displayName": name},
		"spec":       spec,
	}
}

// The whole flow: choose a destination, pick a configuration, fill in the
// credential the source would not disclose, review and apply.
func TestMigratorHappyPath(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	src.put("configurations", "cfg1", config("cfg1", map[string]any{
		"sources": []any{map[string]any{"name": "src1"}},
		"destinations": []any{map[string]any{"name": "s-inline", "type": "splunk", "parameters": []any{
			map[string]any{"name": "token", "value": migrate.SensitivePlaceholder},
		}}},
	}))
	src.put("sources", "src1", map[string]any{"kind": "Source", "spec": map[string]any{"type": "otlp"}})

	m := setup(t, src, dst)

	// The route line names both ends before anything is chosen.
	if v := m.View(); !strings.Contains(v, "source") || !strings.Contains(v, "choose a destination") {
		t.Fatalf("route line:\n%s", v)
	}

	// Choose the destination profile.
	m = send(m, key("enter"))
	if m.stage != stConfigs || m.dst == nil {
		t.Fatalf("stage=%v dst=%v msg=%q", m.stage, m.dst, m.msg)
	}

	// Pick cfg1 and build the plan.
	if !strings.Contains(m.View(), "cfg1") {
		t.Fatalf("configuration not listed:\n%s", m.View())
	}
	m = send(m, key("space"), key("enter"))

	// The redacted token must stop us at the credentials screen.
	if m.stage != stSecrets {
		t.Fatalf("stage=%v, want secrets; msg=%q", m.stage, m.msg)
	}
	if len(m.plan.Secrets) != 1 || m.plan.Secrets[0].Param != "token" {
		t.Fatalf("secrets: %+v", m.plan.Secrets)
	}
	if v := m.View(); !strings.Contains(v, "1 of 1 still unset") {
		t.Fatalf("secrets view:\n%s", v)
	}

	// Fill it in.
	m = send(m, key("enter"))
	m = typeIn(m, "s3cret")
	m = send(m, key("enter"))
	if len(m.plan.Unfilled()) != 0 {
		t.Fatalf("still unfilled: %d", len(m.plan.Unfilled()))
	}

	// Continue to the plan: both resources are new, so both apply.
	m = send(m, key("c"))
	if m.stage != stPlan {
		t.Fatalf("stage=%v", m.stage)
	}
	apply, skip, conflict := m.plan.Counts()
	if apply != 2 || skip != 0 || conflict != 0 {
		t.Fatalf("counts: apply=%d skip=%d conflict=%d", apply, skip, conflict)
	}
	if v := m.View(); !strings.Contains(v, "2 to apply") {
		t.Fatalf("plan view:\n%s", v)
	}

	// Apply needs a typed confirmation.
	m = send(m, key("enter"))
	if m.stage != stConfirm {
		t.Fatalf("stage=%v", m.stage)
	}
	m = send(m, key("enter"))
	if m.stage != stConfirm {
		t.Fatal("empty confirmation must not apply")
	}
	m = typeIn(m, "confirm")
	m = send(m, key("enter"))

	if m.stage != stDone {
		t.Fatalf("stage=%v msg=%q", m.stage, m.msg)
	}
	if len(dst.applied) != 2 {
		t.Fatalf("want sources then configurations, got %d batches", len(dst.applied))
	}
	// Dependencies land first.
	first, _ := json.Marshal(dst.applied[0])
	if !strings.Contains(string(first), `"Source"`) {
		t.Fatalf("first batch should be the source: %s", first)
	}
	all, _ := json.Marshal(dst.applied)
	if !strings.Contains(string(all), "s3cret") {
		t.Fatalf("the credential never reached the destination: %s", all)
	}
	if strings.Contains(string(all), migrate.SensitivePlaceholder) {
		t.Fatalf("placeholder was applied: %s", all)
	}
	if v := m.View(); !strings.Contains(v, "OK") {
		t.Fatalf("done view:\n%s", v)
	}
}

// A resource that already exists in the destination must not be overwritten
// without the user asking for it.
func TestMigratorConflictStartsSkipped(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	src.put("configurations", "cfg1", config("cfg1", map[string]any{
		"sources": []any{map[string]any{"name": "s-1", "type": "otlp"}},
	}))
	dst.put("configurations", "cfg1", config("cfg1", map[string]any{
		"sources": []any{map[string]any{"name": "s-1", "type": "filelog"}},
	}))

	m := setup(t, src, dst)
	m = send(m, key("enter"), key("space"), key("enter"))
	if m.stage != stPlan {
		t.Fatalf("stage=%v msg=%q", m.stage, m.msg)
	}
	if got := m.plan.Items[0].Status; got != migrate.StatusConflict {
		t.Fatalf("status=%v", got)
	}
	if v := m.View(); !strings.Contains(v, "conflict") || !strings.Contains(v, "0 to apply") {
		t.Fatalf("plan view:\n%s", v)
	}
	// Enter must refuse while nothing is selected.
	m = send(m, key("enter"))
	if m.stage != stPlan || m.msg == "" {
		t.Fatalf("stage=%v msg=%q", m.stage, m.msg)
	}
	// The diff names the field that changed.
	m = send(m, key("d"))
	if m.stage != stDetail {
		t.Fatalf("stage=%v", m.stage)
	}
	if v := m.View(); !strings.Contains(v, "sources") || !strings.Contains(v, "filelog") {
		t.Fatalf("detail view:\n%s", v)
	}
	// o takes every conflict.
	m = send(m, key("esc"), key("o"))
	if apply, _, _ := m.plan.Counts(); apply != 1 {
		t.Fatalf("after overwrite-all: apply=%d", apply)
	}
}

// The destination must not be the source, or a migration would be a no-op
// against the project it just read.
func TestMigratorRejectsSameProfile(t *testing.T) {
	src, dst := newFakeBP(t), newFakeBP(t)
	m := setup(t, src, dst)
	m.profs = []profiles.Profile{{Name: "source", Server: src.srv.URL, APIKey: "k", Project: "proj-src"}}
	m = send(m, key("enter"))
	if m.stage != stDest || !strings.Contains(m.msg, "must differ") {
		t.Fatalf("stage=%v msg=%q", m.stage, m.msg)
	}
}
