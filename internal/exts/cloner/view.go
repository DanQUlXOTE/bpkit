package cloner

import (
	"fmt"
	"strings"

	"bpkit/internal/ui"
)

func (m Model) View() string {
	var b strings.Builder
	switch m.stage {
	case stConfigs:
		m.viewConfigs(&b)
	case stLoadingConfig:
		b.WriteString("\nLoading " + m.cfgName + "...\n")
	case stSources:
		m.viewSources(&b)
	case stCSV:
		m.viewCSV(&b)
	case stCreds:
		m.viewCreds(&b)
	case stPreview:
		m.viewPreview(&b)
	case stMode:
		m.viewMode(&b)
	case stConfirm:
		m.viewConfirm(&b)
	case stApplying:
		b.WriteString("\nApplying...\n")
	case stDone:
		b.WriteString("\n" + ui.Good.Render("Done") + "\n\n" + m.done + "\n\n")
		b.WriteString(ui.Keys("n start over", "q back to menu") + "\n")
	}
	if m.msg != "" {
		b.WriteString("\n" + ui.Bad.Render(ui.Wrap(m.msg, max(20, m.width-2))) + "\n")
	} else if m.note != "" {
		b.WriteString("\n" + ui.Good.Render(ui.Wrap(m.note, max(20, m.width-2))) + "\n")
	}
	return b.String()
}

func (m Model) viewConfigs(b *strings.Builder) {
	b.WriteString("\nChoose a configuration:\n\n" + m.cfgFlt.View() + "\n\n")
	if m.busy {
		b.WriteString("Loading configurations...\n")
		return
	}
	if len(m.cfgView) == 0 {
		b.WriteString(ui.Dim.Render("no configurations match") + "\n")
	}
	end := min(len(m.cfgView), m.cfgTop+m.listH())
	for i := m.cfgTop; i < end; i++ {
		c := m.cfgView[i]
		line := fmt.Sprintf("  %-40s %s", ui.Trunc(c.Title(), 40), ui.Dim.Render(c.Name()))
		if i == m.cfgCur {
			line = ui.Cur.Render(fmt.Sprintf("  %-40s %s", ui.Trunc(c.Title(), 40), c.Name()))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + ui.Keys("type to filter", "up/down move", "enter choose", "esc back to menu") + "\n")
}

func (m Model) viewSources(b *strings.Builder) {
	fmt.Fprintf(b, "\nTemplate source in %s:\n\n", ui.Title.Render(m.cfgName))
	end := min(len(m.sources), m.srcTop+m.listH())
	for i := m.srcTop; i < end; i++ {
		s := m.sources[i]
		raw := fmt.Sprintf("  %2d. %-38s %s", i, ui.Trunc(s.label, 38), s.typ)
		var line string
		switch {
		case i == m.srcCur:
			line = ui.Cur.Render(raw)
		case !s.ok:
			line = ui.Dim.Render(raw)
		default:
			line = raw
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + ui.Dim.Render("Dimmed sources are not Windows Event sources and cannot be cloned.") + "\n")
	b.WriteString(ui.Keys("up/down move", "enter choose", "esc back") + "\n")
}

func (m Model) viewCSV(b *strings.Builder) {
	b.WriteString("\nCSV of hosts:\n\n  " + m.csvPath.View() + "\n\n")
	b.WriteString(ui.Dim.Render("A header row is required, with a hostname column. An optional name column\nbecomes the new source's display name. Every other column overrides the\nparameter of that name on the cloned source.") + "\n\n")
	b.WriteString(ui.Keys("enter read the file", "esc back") + "\n")
}

func (m Model) viewCreds(b *strings.Builder) {
	fmt.Fprintf(b, "\nWindows credentials, shared by all %d new source(s):\n\n", len(m.rows))
	mark := [credFields]string{"  ", "  ", "  "}
	mark[m.focus] = "> "
	if m.editing {
		mark[m.focus] = "* "
	}
	fmt.Fprintf(b, "%sUsername: %s\n", mark[0], m.user.View())
	fmt.Fprintf(b, "%sPassword: %s\n", mark[1], m.pass.View())
	fmt.Fprintf(b, "%sDomain:   %s\n\n", mark[2], m.domain.View())
	if m.editing {
		b.WriteString(ui.Dim.Render("INPUT  enter/esc done") + "\n")
	} else {
		b.WriteString(ui.Keys("up/down switch line", "enter edit", "v preview (nothing applied)", "esc back") + "\n")
	}
}

// previewLines is the body of the preview, one line per new source.
func (m Model) previewLines() []string {
	out := []string{
		fmt.Sprintf("Configuration: %s", m.cfgName),
		fmt.Sprintf("Template:      %s", m.result.TemplateName),
		fmt.Sprintf("Adding:        %d source(s)", len(m.result.Added)),
		"",
	}
	for _, u := range m.result.UnknownParams {
		out = append(out, ui.Warn.Render(fmt.Sprintf("warning: CSV column %q matches no parameter on the template", u)))
	}
	if len(m.result.UnknownParams) > 0 {
		out = append(out, "")
	}
	for i, name := range m.result.Added {
		host := ""
		if i < len(m.rows) {
			host = m.rows[i].Hostname
		}
		out = append(out, fmt.Sprintf("  + %-38s %s", ui.Trunc(name, 38), ui.Dim.Render(host)))
	}
	return out
}

func (m Model) viewPreview(b *strings.Builder) {
	b.WriteString("\n" + ui.Title.Render("Dry run") + " " + ui.Dim.Render("nothing has been sent to Bindplane") + "\n\n")
	lines := m.previewLines()
	h := max(3, m.height-12)
	top := min(m.prevTop, max(0, len(lines)-h))
	end := min(len(lines), top+h)
	for _, l := range lines[top:end] {
		b.WriteString(l + "\n")
	}
	if end < len(lines) {
		fmt.Fprintf(b, "%s\n", ui.Dim.Render(fmt.Sprintf("... %d more", len(lines)-end)))
	}
	b.WriteString("\n" + ui.Keys("up/down scroll", "enter continue", "esc start over", "q back to menu") + "\n")
}

func (m Model) viewMode(b *strings.Builder) {
	b.WriteString("\nWhat should happen to the modified configuration?\n\n")
	for md := modeDryRun; md <= modeUpdate; md++ {
		raw := fmt.Sprintf("  %-10s %s", md.String(), md.desc())
		if md == m.md {
			raw = ui.Cur.Render(raw)
		} else {
			raw = ui.Dim.Render(raw)
		}
		b.WriteString(raw + "\n")
	}
	b.WriteString("\n")
	switch m.md {
	case modeDryRun:
		b.WriteString("  Output file: " + m.outPath.View() + "\n")
	case modeVerify:
		b.WriteString("  New name:    " + m.newName.View() + "\n")
	case modeUpdate:
		b.WriteString("  " + ui.Bad.Render("This replaces "+m.cfgName+" on the server.") + "\n")
	}
	b.WriteString("\n")
	if m.editing {
		b.WriteString(ui.Dim.Render("INPUT  enter/esc done") + "\n")
	} else {
		b.WriteString(ui.Keys("up/down pick mode", "i edit the line", "enter go", "esc back") + "\n")
	}
}

func (m Model) viewConfirm(b *strings.Builder) {
	b.WriteString("\n" + ui.Bad.Render("Confirm") + "\n\n")
	switch m.md {
	case modeVerify:
		fmt.Fprintf(b, "  Apply %d new source(s) as a NEW configuration %q.\n\n", len(m.result.Added), strings.TrimSpace(m.newName.Value()))
	default:
		fmt.Fprintf(b, "  Replace configuration %q, adding %d new source(s).\n\n", m.cfgName, len(m.result.Added))
	}
	b.WriteString("  " + m.confirm.View() + "\n\n")
	b.WriteString(ui.Keys("type confirm then enter", "esc back") + "\n")
}
