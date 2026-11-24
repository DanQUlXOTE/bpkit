package migrate

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"bpkit/internal/bpclient"
)

// DefaultPlaceholder is what --placeholder-credentials writes into a
// credential the source would not disclose. It is deliberately conspicuous:
// it has to be obvious in the Bindplane UI that this value is not real.
const DefaultPlaceholder = "REPLACE-ME-bpkit-migrated"

// FillPlaceholders writes value into every credential the plan still needs,
// and returns what it filled so the caller can list them. The resources
// then apply — Bindplane refuses only the "(sensitive)" placeholder itself,
// not an implausible value — but they cannot work until the real values are
// entered.
func (p *Plan) FillPlaceholders(value string) []*Secret {
	filled := p.Unfilled()
	for _, s := range filled {
		s.Set(value)
	}
	return filled
}

// SkipNeedingCredentials excludes every resource that still needs a
// credential, and everything that references one, returning a line per
// resource saying why. Without this a plan applied unattended would have
// those resources refused outright, and then every configuration pointing
// at one refused too, for a reason that reads like a bug.
func (p *Plan) SkipNeedingCredentials() []string {
	label := func(it Item) string {
		return fmt.Sprintf("%s %s", strings.TrimSuffix(string(it.Kind), "s"), it.Title())
	}

	var skipped []string
	dropped := map[string]bool{} // "kind/name" of everything left out
	drop := func(i int, why string) {
		p.Items[i].Skip = true
		dropped[string(p.Items[i].Kind)+"/"+p.Items[i].Resource.Name()] = true
		skipped = append(skipped, label(p.Items[i])+why)
	}

	seen := map[int]bool{}
	for _, s := range p.Unfilled() {
		if s.item < 0 || s.item >= len(p.Items) || seen[s.item] {
			continue
		}
		seen[s.item] = true
		drop(s.item, " (needs a credential)")
	}

	// A resource referencing something left out cannot render either, so
	// drop it too, and repeat until nothing else falls out.
	for changed := true; changed; {
		changed = false
		for i := range p.Items {
			if p.Items[i].Skip {
				continue
			}
			if ref := firstDroppedReference(p.Items[i], dropped); ref != "" {
				drop(i, " (depends on "+ref+")")
				changed = true
			}
		}
	}
	return skipped
}

// firstDroppedReference names a library resource this item points at that
// is being left out, or "".
func firstDroppedReference(it Item, dropped map[string]bool) string {
	var found string
	eachComponent(it.Resource.Spec(), func(kind bpclient.Kind, c map[string]any) {
		if found != "" {
			return
		}
		name, ok := isReference(c)
		if !ok || kind == "" {
			return
		}
		if key := string(kind) + "/" + bpclient.TrimVersion(name); dropped[key] {
			found = bpclient.TrimVersion(name)
		}
	})
	return found
}

// RenderPlan writes the plan as text, for a run with no terminal UI.
func (p *Plan) RenderPlan(w io.Writer) {
	apply, skip, conflict := p.Counts()
	fmt.Fprintf(w, "Plan: %d to apply, %d skipped, %d conflict(s)\n\n", apply, skip, conflict)

	byKind := map[bpclient.Kind]map[Status]int{}
	for _, it := range p.Items {
		if byKind[it.Kind] == nil {
			byKind[it.Kind] = map[Status]int{}
		}
		byKind[it.Kind][it.Status]++
	}
	for _, kind := range bpclient.ResourceKinds {
		counts := byKind[kind]
		if len(counts) == 0 {
			continue
		}
		var parts []string
		for _, st := range []Status{StatusNew, StatusSame, StatusConflict, StatusUnknown} {
			if n := counts[st]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, st))
			}
		}
		fmt.Fprintf(w, "  %-18s %s\n", kind, strings.Join(parts, ", "))
	}

	// Name what would actually be written, so a dry run is reviewable.
	var applying []string
	for _, it := range p.Items {
		if !it.Skip {
			applying = append(applying, fmt.Sprintf("  + %-18s %s", it.Kind, it.Title()))
		}
	}
	if len(applying) > 0 {
		fmt.Fprintf(w, "\nWould apply %d resource(s):\n", len(applying))
		for _, l := range applying {
			fmt.Fprintln(w, l)
		}
	}

	// Conflicts are never applied, so say which and why.
	var conflicts []string
	for _, it := range p.Items {
		if it.Status == StatusConflict {
			conflicts = append(conflicts, fmt.Sprintf("  ! %-18s %s (differs in %s)",
				it.Kind, it.Title(), strings.Join(it.Diff, ", ")))
		}
	}
	if len(conflicts) > 0 {
		fmt.Fprintf(w, "\n%d resource(s) already exist in the destination and differ. They are left\n"+
			"alone; use the TUI to see the difference and decide:\n", len(conflicts))
		for _, l := range conflicts {
			fmt.Fprintln(w, l)
		}
	}

	if len(p.Warnings) > 0 {
		fmt.Fprintf(w, "\n%d warning(s):\n", len(p.Warnings))
		for _, l := range summarize(p.Warnings, 10) {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
}

// RenderCredentials lists the credentials a run filled with a placeholder,
// with the warning that goes with them.
func RenderCredentials(w io.Writer, filled []*Secret, value string) {
	if len(filled) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", strings.Repeat("=", 72))
	fmt.Fprintf(w, "%d credential(s) were filled with the placeholder %q.\n", len(filled), value)
	fmt.Fprint(w, "These resources exist in the destination but WILL NOT WORK until you\n"+
		"replace each value with the real one in the Bindplane UI. Do not roll a\n"+
		"configuration out to collectors before you do: it will authenticate with\n"+
		"the placeholder and fail.\n\n")
	for _, s := range filled {
		fmt.Fprintf(w, "  %s → %s\n", s.Where, s.Param)
	}
	fmt.Fprintf(w, "%s\n", strings.Repeat("=", 72))
}

// RenderSkipped lists resources left out because they still need a
// credential.
func RenderSkipped(w io.Writer, skipped []string) {
	if len(skipped) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%d resource(s) need a credential and were left out, along with anything\n"+
		"depending on them. Pass --placeholder-credentials to copy them with a\n"+
		"placeholder instead, or migrate them in the TUI where you can type the\n"+
		"real values:\n", len(skipped))
	for _, s := range skipped {
		fmt.Fprintf(w, "  - %s\n", s)
	}
}

// RenderResults writes what an apply did, grouping failures by reason.
func RenderResults(w io.Writer, results []Result) {
	for _, r := range results {
		if r.OK() {
			fmt.Fprintf(w, "OK      %-18s %d\n", r.Kind, r.Applied)
			continue
		}
		fmt.Fprintf(w, "PARTIAL %-18s %d of %d\n", r.Kind, r.Applied, r.Count)
	}

	var failures []Failure
	for _, r := range results {
		failures = append(failures, r.Failures...)
	}
	if len(failures) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%d resource(s) were refused:\n", len(failures))

	byReason := map[string][]string{}
	example := map[string]Failure{}
	for _, f := range failures {
		reason := f.Err.Error()
		byReason[reason] = append(byReason[reason], f.Title)
		example[reason] = f
	}
	reasons := make([]string, 0, len(byReason))
	for r := range byReason {
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool {
		return len(byReason[reasons[i]]) > len(byReason[reasons[j]])
	})
	for _, reason := range reasons {
		fmt.Fprintf(w, "\n  %s\n", strings.Join(byReason[reason], ", "))
		fmt.Fprintf(w, "    %s\n", oneLine(reason))
		if why := example[reason].Explain(); why != "" {
			fmt.Fprintf(w, "    %s\n", why)
		}
	}
}

// summarize caps a long list, saying how much it left out.
func summarize(all []string, limit int) []string {
	if len(all) <= limit {
		return all
	}
	out := append([]string{}, all[:limit]...)
	return append(out, fmt.Sprintf("... %d more", len(all)-limit))
}

// oneLine flattens a server error so it reads as a single entry.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.Join(strings.Fields(s), " ")
}
