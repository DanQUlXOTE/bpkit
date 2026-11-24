package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"bpkit/internal/bpclient"
	"bpkit/internal/profiles"
)

// fakeBP serves enough Bindplane for the CLI to plan and apply. Resources
// belong to the project named by srcProject; every other project is empty,
// which is what --to-project migrates into.
func fakeBP(t *testing.T, srcProject string, res map[string]map[string]any, applied *[][]map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Bindplane-Account-ID") != srcProject {
			// A different project: empty, and an apply there is recorded.
			if r.URL.Path == "/v1/apply" {
				var body struct {
					Resources []map[string]any `json:"resources"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				if applied != nil {
					*applied = append(*applied, body.Resources)
				}
				json.NewEncoder(w).Encode(map[string]any{"updates": []any{}})
				return
			}
			parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/v1/"), "/", 2)
			if len(parts) == 1 {
				json.NewEncoder(w).Encode(map[string]any{parts[0]: []any{}})
				return
			}
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		if r.URL.Path == "/v1/apply" {
			var body struct {
				Resources []map[string]any `json:"resources"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if applied != nil {
				*applied = append(*applied, body.Resources)
			}
			json.NewEncoder(w).Encode(map[string]any{"updates": []any{}})
			return
		}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/v1/"), "/", 2)
		if len(parts) == 1 {
			var list []any
			for k, v := range res {
				if strings.HasPrefix(k, parts[0]+"/") {
					list = append(list, v)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{parts[0]: list})
			return
		}
		got, ok := res[parts[0]+"/"+parts[1]]
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{strings.TrimSuffix(parts[0], "s"): got})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cfgWithSecretDest() map[string]map[string]any {
	return map[string]map[string]any{
		"configurations/cfg": {
			"apiVersion": "bindplane.observiq.com/v1", "kind": "Configuration",
			"metadata": map[string]any{"name": "cfg", "displayName": "cfg"},
			"spec": map[string]any{"destinations": []any{
				map[string]any{"name": "splunk"},
			}},
		},
		"destinations/splunk": {
			"kind":     "Destination",
			"metadata": map[string]any{"name": "splunk", "displayName": "splunk"},
			"spec": map[string]any{"type": "splunk", "parameters": []any{
				map[string]any{"name": "token", "value": "(sensitive)"},
			}},
		},
	}
}

func setupCLI(t *testing.T, applied *[][]map[string]any) (*profiles.Store, profiles.Profile, *bpclient.Client) {
	t.Helper()
	srv := fakeBP(t, "src", cfgWithSecretDest(), applied)
	store := profiles.NewStore(filepath.Join(t.TempDir(), "p.json"))
	src, err := bpclient.New(bpclient.Options{BaseURL: srv.URL, APIKey: "k", Project: "src"})
	if err != nil {
		t.Fatal(err)
	}
	return store, profiles.Profile{Name: "source", Server: srv.URL, APIKey: "k", Project: "src"}, src
}

// A run without --apply must describe the migration and send nothing.
func TestMigrateCLIDryRunWritesNothing(t *testing.T) {
	var applied [][]map[string]any
	store, active, src := setupCLI(t, &applied)
	var out strings.Builder
	err := runMigrate(store, active, src, migrateOptions{toProject: "dst"}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"Migrating source (src) → dst", "DRY RUN", "nothing was sent"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if len(applied) != 0 {
		t.Fatalf("a dry run must not apply: %+v", applied)
	}
}

// Without --placeholder-credentials, a resource needing one is left out and
// named, rather than applied and refused.
func TestMigrateCLISkipsCredentialResources(t *testing.T) {
	store, active, src := setupCLI(t, nil)
	var out strings.Builder
	if err := runMigrate(store, active, src, migrateOptions{toProject: "dst"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "need a credential and were left out") {
		t.Fatalf("should explain the omission:\n%s", got)
	}
	if !strings.Contains(got, "destination splunk") {
		t.Fatalf("should name the resource:\n%s", got)
	}
	if !strings.Contains(got, "--placeholder-credentials") {
		t.Fatalf("should point at the alternative:\n%s", got)
	}
}

// With a placeholder, the resource copies, and the warning says the config
// will not work until the real value is entered.
func TestMigrateCLIPlaceholderFillsAndWarns(t *testing.T) {
	var applied [][]map[string]any
	store, active, src := setupCLI(t, &applied)
	var out strings.Builder
	o := migrateOptions{toProject: "dst", usePlace: true, placeholder: "PLACEHOLDER", apply: true, assumeYes: true}
	if err := runMigrate(store, active, src, o, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"WILL NOT WORK", "splunk", "token", "PLACEHOLDER"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	body, _ := json.Marshal(applied)
	if !strings.Contains(string(body), "PLACEHOLDER") {
		t.Fatalf("the placeholder never reached the server: %s", body)
	}
	if strings.Contains(string(body), "(sensitive)") {
		t.Fatalf("the masked value was applied: %s", body)
	}
}

// --apply still asks before writing.
func TestMigrateCLIRequiresConfirmation(t *testing.T) {
	var applied [][]map[string]any
	store, active, src := setupCLI(t, &applied)
	o := migrateOptions{toProject: "dst", usePlace: true, placeholder: "P", apply: true}

	var out strings.Builder
	err := runMigrate(store, active, src, o, strings.NewReader("wrong\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "confirmation") {
		t.Fatalf("a wrong answer must abort, got %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("nothing should have been applied: %+v", applied)
	}

	out.Reset()
	if err := runMigrate(store, active, src, o, strings.NewReader("dst\n"), &out); err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 {
		t.Fatal("typing the destination name should have applied")
	}
}

func TestMigrateCLIDestinationErrors(t *testing.T) {
	store, active, src := setupCLI(t, nil)
	cases := []struct {
		name string
		o    migrateOptions
		want string
	}{
		{"none given", migrateOptions{}, "--to-project"},
		{"both given", migrateOptions{toProject: "a", toProfile: "b"}, "not both"},
		{"same project", migrateOptions{toProject: "src"}, "must differ"},
		{"unknown profile", migrateOptions{toProfile: "nope"}, "no profile named"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out strings.Builder
			err := runMigrate(store, active, src, c.o, strings.NewReader(""), &out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error mentioning %q, got %v", c.want, err)
			}
		})
	}
}

// Skipping a resource for want of a credential must also skip whatever
// references it, or that resource is applied only to be refused with
// "unknown Destination".
func TestMigrateCLISkipCascades(t *testing.T) {
	var applied [][]map[string]any
	srv := fakeBP(t, "src", cfgWithSecretDest(), &applied)
	store := profiles.NewStore(filepath.Join(t.TempDir(), "p.json"))
	src, err := bpclient.New(bpclient.Options{BaseURL: srv.URL, APIKey: "k", Project: "src"})
	if err != nil {
		t.Fatal(err)
	}
	active := profiles.Profile{Name: "source", Server: srv.URL, APIKey: "k", Project: "src"}

	var out strings.Builder
	o := migrateOptions{toProject: "dst", apply: true, assumeYes: true}
	if err := runMigrate(store, active, src, o, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "destination splunk (needs a credential)") {
		t.Fatalf("should say why splunk was dropped:\n%s", got)
	}
	if !strings.Contains(got, "configuration cfg (depends on splunk)") {
		t.Fatalf("the config depending on it should be dropped too:\n%s", got)
	}
	// With both dropped there is nothing left, so nothing may be sent.
	if len(applied) != 0 {
		t.Fatalf("nothing should have been applied: %+v", applied)
	}
}
