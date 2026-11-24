package migrate

// A diagnostic harness, not part of the suite. It runs a real migration
// against a real Bindplane and enumerates every distinct failure instead of
// stopping at the first, which is what Plan.Apply does.
//
// Run with:
//	BPKIT_LIVE=1 BPKIT_SRC_PROJECT= BPKIT_DST_PROJECT=<id> go test ./internal/migrate/ -run TestLiveProbe -v -timeout 60m

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"bpkit/internal/bpclient"
)

func liveClients(t *testing.T) (src, dst *bpclient.Client) {
	t.Helper()
	key := os.Getenv("BPKIT_KEY")
	if key == "" {
		t.Skip("BPKIT_KEY not set")
	}
	mk := func(project string) *bpclient.Client {
		c, err := bpclient.New(bpclient.Options{
			BaseURL: "https://app.bindplane.com", APIKey: key,
			Project: project, Timeout: 120 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return mk(os.Getenv("BPKIT_SRC_PROJECT")), mk(os.Getenv("BPKIT_DST_PROJECT"))
}

func TestLiveProbe(t *testing.T) {
	if os.Getenv("BPKIT_LIVE") == "" {
		t.Skip("set BPKIT_LIVE=1 to run")
	}
	ctx := context.Background()
	src, dst := liveClients(t)

	configs, err := src.ListResources(ctx, bpclient.KindConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range configs {
		if n := c.Name(); n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	t.Logf("source holds %d configurations", len(names))

	start := time.Now()
	p, err := Build(ctx, src, dst, names)
	if err != nil {
		t.Fatalf("BUILD FAILED: %v", err)
	}
	t.Logf("plan built in %s: %d items, %d secrets, %d warnings",
		time.Since(start).Round(time.Second), len(p.Items), len(p.Secrets), len(p.Warnings))

	// What the plan holds, by kind and status.
	type key struct {
		kind   bpclient.Kind
		status Status
	}
	counts := map[key]int{}
	for _, it := range p.Items {
		counts[key{it.Kind, it.Status}]++
	}
	t.Log("--- plan by kind and status ---")
	for _, k := range bpclient.ResourceKinds {
		for _, st := range []Status{StatusNew, StatusSame, StatusConflict, StatusUnknown} {
			if n := counts[key{k, st}]; n > 0 {
				t.Logf("  %-18s %-10s %d", k, st, n)
			}
		}
	}

	// Warnings, grouped so one repeated shape does not drown the rest.
	if len(p.Warnings) > 0 {
		t.Logf("--- %d warnings ---", len(p.Warnings))
		kinds := map[string][]string{}
		for _, w := range p.Warnings {
			switch {
			case strings.Contains(w, "please report"):
				kinds["SURVIVING PIN"] = append(kinds["SURVIVING PIN"], w)
			case strings.Contains(w, "could not read"):
				kinds["UNREADABLE"] = append(kinds["UNREADABLE"], w)
			case strings.Contains(w, "current version"):
				kinds["unpinned"] = append(kinds["unpinned"], w)
			default:
				kinds["other"] = append(kinds["other"], w)
			}
		}
		for _, g := range []string{"SURVIVING PIN", "UNREADABLE", "other", "unpinned"} {
			ws := kinds[g]
			if len(ws) == 0 {
				continue
			}
			t.Logf("  [%s] %d", g, len(ws))
			limit := len(ws)
			if g == "unpinned" && limit > 5 {
				limit = 5
			}
			for _, w := range ws[:limit] {
				t.Logf("      %s", w)
			}
			if limit < len(ws) {
				t.Logf("      ... %d more", len(ws)-limit)
			}
		}
	}

	if len(p.Secrets) > 0 {
		t.Logf("--- %d credentials the source would not disclose (left unset) ---", len(p.Secrets))
		for i, s := range p.Secrets {
			if i == 8 {
				t.Logf("      ... %d more", len(p.Secrets)-8)
				break
			}
			t.Logf("      %s → %s", s.Where, s.Param)
		}
	}

	if os.Getenv("BPKIT_APPLY") == "" {
		t.Log("set BPKIT_APPLY=1 to apply")
		return
	}

	// Apply through the real code path, which isolates a failed batch
	// itself, then group what it reports.
	t.Log("--- applying ---")
	results, applyErr := p.Apply(ctx, dst)
	for _, r := range results {
		if r.OK() {
			t.Logf("  OK      %-18s %d", r.Kind, r.Applied)
			continue
		}
		t.Logf("  PARTIAL %-18s %d of %d", r.Kind, r.Applied, r.Count)
	}
	if applyErr == nil {
		t.Log("--- every resource applied ---")
		return
	}

	failures := map[string][]string{}
	for _, r := range results {
		for _, f := range r.Failures {
			sh := shape(f.Err.Error())
			failures[sh] = append(failures[sh], string(r.Kind)+" "+f.Title)
		}
	}
	t.Logf("--- %d distinct failure shapes ---", len(failures))
	var shapes []string
	for s := range failures {
		shapes = append(shapes, s)
	}
	sort.Slice(shapes, func(i, j int) bool { return len(failures[shapes[i]]) > len(failures[shapes[j]]) })
	for _, s := range shapes {
		who := failures[s]
		t.Logf("\n  [%d resource(s)] %s", len(who), s)
		for i, w := range who {
			if i == 5 {
				t.Logf("      ... %d more", len(who)-5)
				break
			}
			t.Logf("      %s", w)
		}
	}
}

// shape reduces an error to its recognizable form so repeats group together.
func shape(msg string) string {
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.ReplaceAll(msg, "\t", " ")
	msg = strings.Join(strings.Fields(msg), " ")
	// Drop the resource-specific prefix Bindplane puts before the reason.
	if i := strings.Index(msg, "errors\":[\""); i >= 0 {
		msg = msg[i+len("errors\":[\""):]
	}
	for _, marker := range []string{"* ", "error occurred: "} {
		if i := strings.LastIndex(msg, marker); i >= 0 {
			msg = msg[i+len(marker):]
		}
	}
	if len(msg) > 220 {
		msg = msg[:220] + "…"
	}
	return fmt.Sprintf("%s", msg)
}
