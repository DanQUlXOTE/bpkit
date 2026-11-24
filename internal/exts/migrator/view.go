package migrator

import (
	"encoding/json"
	"fmt"
	"strings"

	"bpkit/internal/migrate"
	"bpkit/internal/ui"
)

func (m Model) View() string {
	var b strings.Builder
	b.WriteString("\n" + m.route() + "\n\n")

	switch m.stage {
	case stDest:
		m.viewDest(&b)
	case stConfigs:
		m.viewConfigs(&b)
	case stBuilding:
		b.WriteString("Reading configurations and their dependencies...\n")
	case stSecrets:
		m.viewSecrets(&b)
	case stPlan:
		m.viewPlan(&b)
	case stDetail:
		m.viewDetail(&b)
	case stConfirm:
		m.viewConfirm(&b)
	case stApplying:
		b.WriteString("Applying...\n")
	case stDone:
		m.viewDone(&b)
	}

	if m.msg != "" {
		b.WriteString("\n" + ui.Bad.Render(m.msg) + "\n")
	} else if m.note != "" {
		b.WriteString("\n" + ui.Good.Render(m.note) + "\n")
	}
	return b.String()
}

// route is the from → to line kept at the top of every screen, because
// applying to the wrong project is the mistake worth preventing.
func (m Model) route() string {
	to := ui.Dim.Render("(choose a destination)")
	if m.dst != nil {
		to = ui.Warn.Render(m.dstName)
	}
	return fmt.Sprintf("  %s  %s  %s", ui.Good.Render(m.srcName), ui.Dim.Render("→"), to)
}

func (m Model) viewDest(b *strings.Builder) {
	b.WriteString("Copy into which profile?\n\n")
	if len(m.profs) == 0 {
		b.WriteString(ui.Dim.Render("No saved profiles. Press ctrl+p to add one.") + "\n")
	}
	for i, p := range m.profs {
		tag := ""
		if strings.EqualFold(p.Name, m.ctx.Active.Name) && p.Project == m.ctx.Active.Project {
			tag = ui.Dim.Render("  (source)")
		}
		raw := fmt.Sprintf("  %-22s %-32s %s", ui.Trunc(p.Name, 22), ui.Trunc(p.Server, 32), ui.Trunc(p.Project, 20))
		if i == m.profCur {
			raw = ui.Cur.Render(raw)
		}
		b.WriteString(raw + tag + "\n")
	}
	b.WriteString("\n" + ui.Keys("up/down move", "enter choose", "esc back to menu") + "\n")
	b.WriteString(ui.Dim.Render("Two profiles on the same server that differ only by project copy between projects.") + "\n")
}

func (m Model) viewConfigs(b *strings.Builder) {
	fmt.Fprintf(b, "Configurations to copy %s\n\n", ui.Dim.Render(fmt.Sprintf("(%d selected)", len(m.picked))))
	b.WriteString(m.flt.View() + "\n\n")
	if m.busy {
		b.WriteString("Loading...\n")
		return
	}
	end := min(len(m.view), m.top+m.listH())
	for i := m.top; i < end; i++ {
		c := m.view[i]
		raw := fmt.Sprintf("%s%-40s %s", ui.Mark(false, m.picked[c.Name()]), ui.Trunc(c.Title(), 40), c.Name())
		if i == m.cur {
			raw = ui.Cur.Render(raw)
		}
		b.WriteString(raw + "\n")
	}
	b.WriteString("\n" + ui.Keys("type to filter", "up/down move", "space select", "ctrl+a all", "enter build plan", "esc back") + "\n")
	b.WriteString(ui.Dim.Render("Sources, processors, destinations and extensions they reference come along automatically.") + "\n")
}

func (m Model) viewSecrets(b *strings.Builder) {
	secs := m.plan.Secrets
	left := len(m.plan.Unfilled())
	fmt.Fprintf(b, "%s  %s\n\n",
		ui.Title.Render("Credentials"),
		ui.Dim.Render(fmt.Sprintf("%d of %d still unset", left, len(secs))))
	b.WriteString(ui.Dim.Render("Bindplane never discloses these values, so they arrive as \"(sensitive)\".\nType the real value for each, or continue and fix them in the destination.") + "\n\n")

	end := min(len(secs), m.listH())
	top := max(0, min(m.secCur-end/2, len(secs)-end))
	for i := top; i < min(len(secs), top+end); i++ {
		s := secs[i]
		state := ui.Bad.Render("unset")
		if s.Filled() {
			state = ui.Good.Render("set  ")
		}
		raw := fmt.Sprintf("  %s  %-46s %s", state, ui.Trunc(s.Where, 46), s.Param)
		if i == m.secCur {
			raw = ui.Cur.Render(fmt.Sprintf("  %-5s  %-46s %s", stateWord(s), ui.Trunc(s.Where, 46), s.Param))
		}
		b.WriteString(raw + "\n")
	}
	b.WriteString("\n")
	if m.editing {
		b.WriteString("  " + secs[m.secCur].Param + ": " + m.secInput.View() + "\n\n")
		b.WriteString(ui.Dim.Render("INPUT  enter save  esc cancel") + "\n")
	} else {
		b.WriteString(ui.Keys("up/down move", "enter set value", "c continue", "esc back") + "\n")
	}
}

func stateWord(s *migrate.Secret) string {
	if s.Filled() {
		return "set"
	}
	return "unset"
}

func (m Model) viewPlan(b *strings.Builder) {
	apply, skip, conflict := m.plan.Counts()
	fmt.Fprintf(b, "%s  %s\n\n",
		ui.Title.Render("Plan"),
		ui.Dim.Render(fmt.Sprintf("%d to apply, %d skipped, %d conflict(s)", apply, skip, conflict)))

	for _, w := range m.plan.Warnings {
		b.WriteString(ui.Warn.Render("  "+w) + "\n")
	}
	if len(m.plan.Warnings) > 0 {
		b.WriteString("\n")
	}

	items := m.plan.Items
	end := min(len(items), m.itemTop+m.listH())
	for i := m.itemTop; i < end; i++ {
		it := items[i]
		action := ui.Good.Render("apply")
		if it.Skip {
			action = ui.Dim.Render("skip ")
		}
		status := it.Status.String()
		switch it.Status {
		case migrate.StatusConflict:
			status = ui.Warn.Render("conflict")
		case migrate.StatusSame:
			status = ui.Dim.Render("identical")
		case migrate.StatusUnknown:
			status = ui.Bad.Render("unreadable")
		}
		kind := strings.TrimSuffix(string(it.Kind), "s")
		root := " "
		if it.Root {
			root = "*"
		}
		raw := fmt.Sprintf(" %s %s  %-17s %-34s %s", root, action, kind, ui.Trunc(it.Title(), 34), status)
		if i == m.itemCur {
			raw = ui.Cur.Render(fmt.Sprintf(" %s %-5s  %-17s %-34s %s",
				root, actionWord(it), kind, ui.Trunc(it.Title(), 34), it.Status.String()))
		}
		b.WriteString(raw + "\n")
	}
	b.WriteString("\n")
	if len(items) > 0 {
		switch cur := items[m.itemCur]; {
		case cur.Status == migrate.StatusConflict:
			fmt.Fprintf(b, "%s\n", ui.Dim.Render("  differs in: "+strings.Join(cur.Diff, ", ")+"   (d for the detail)"))
		case cur.Note != "" && !cur.TypeDef: // a missing type is explained below
			fmt.Fprintf(b, "%s\n", ui.Dim.Render("  "+cur.Note))
		}
	}
	if n := m.missingTypes(); n > 0 {
		fmt.Fprintf(b, "%s\n", ui.Warn.Render(fmt.Sprintf(
			"  %d type definition(s) are missing in the destination. Bindplane seeds its built-in", n)))
		b.WriteString(ui.Warn.Render("  types itself and a new project can still be loading them — wait and re-check before") + "\n")
		b.WriteString(ui.Warn.Render("  copying them, since applying a built-in type over the API can fail validation.") + "\n")
	}
	b.WriteString(ui.Keys("space apply/skip", "o overwrite all conflicts", "d diff", "enter apply", "esc back", "q menu") + "\n")
	b.WriteString(ui.Dim.Render("* = a configuration you picked. Type definitions, conflicts and identical resources start skipped.") + "\n")
}

// missingTypes counts type definitions the destination does not have.
func (m Model) missingTypes() int {
	n := 0
	for _, it := range m.plan.Items {
		if it.TypeDef && it.Status == migrate.StatusNew {
			n++
		}
	}
	return n
}

func actionWord(it migrate.Item) string {
	if it.Skip {
		return "skip"
	}
	return "apply"
}

func (m Model) viewDetail(b *strings.Builder) {
	it := m.plan.Items[m.itemCur]
	fmt.Fprintf(b, "%s  %s\n\n", ui.Title.Render(it.Title()), ui.Dim.Render(string(it.Kind)))
	lines := []string{
		ui.Dim.Render("Fields that differ: ") + strings.Join(it.Diff, ", "),
		"",
	}
	for _, f := range it.Diff {
		lines = append(lines,
			ui.Warn.Render("  "+f),
			"    "+ui.Dim.Render("destination: ")+ui.Trunc(render(it.Existing.Spec()[f]), max(20, m.width-20)),
			"    "+ui.Dim.Render("source:      ")+ui.Trunc(render(it.Resource.Spec()[f]), max(20, m.width-20)),
			"")
	}
	h := max(3, m.height-12)
	top := min(m.detTop, max(0, len(lines)-h))
	for _, l := range lines[top:min(len(lines), top+h)] {
		b.WriteString(l + "\n")
	}
	b.WriteString("\n" + ui.Keys("up/down scroll", "esc back") + "\n")
}

// render shows a spec value compactly, for side-by-side comparison.
func render(v any) string {
	if v == nil {
		return "(absent)"
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(data)
}

func (m Model) viewConfirm(b *strings.Builder) {
	apply, _, conflict := m.plan.Counts()
	b.WriteString(ui.Bad.Render("Confirm") + "\n\n")
	fmt.Fprintf(b, "  Apply %d resource(s) to %s.\n", apply, ui.Warn.Render(m.dstName))
	if conflict > 0 {
		overwrite := 0
		for _, it := range m.plan.Items {
			if it.Status == migrate.StatusConflict && !it.Skip {
				overwrite++
			}
		}
		if overwrite > 0 {
			fmt.Fprintf(b, "  %s\n", ui.Bad.Render(fmt.Sprintf("%d of them replace a resource that already exists there.", overwrite)))
		}
	}
	if n := len(m.plan.Unfilled()); n > 0 {
		fmt.Fprintf(b, "  %s\n", ui.Warn.Render(fmt.Sprintf("%d credential(s) are still unset.", n)))
	}
	b.WriteString("\n  " + m.confirm.View() + "\n\n")
	b.WriteString(ui.Keys("type confirm then enter", "esc back") + "\n")
}

func (m Model) viewDone(b *strings.Builder) {
	for _, r := range m.results {
		if r.Err != nil {
			fmt.Fprintf(b, "%s %d %s: %v\n", ui.Bad.Render("FAILED"), r.Count, r.Kind, r.Err)
			continue
		}
		fmt.Fprintf(b, "%s %d %s\n", ui.Good.Render("OK    "), r.Count, r.Kind)
	}
	if len(m.results) == 0 && m.msg == "" {
		b.WriteString(ui.Dim.Render("nothing applied") + "\n")
	}
	b.WriteString("\n" + ui.Keys("q back to menu") + "\n")
}
