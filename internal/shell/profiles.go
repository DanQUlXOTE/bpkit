package shell

import (
	"context"
	"fmt"
	"strings"
	"time"

	"bpkit/internal/bpclient"
	"bpkit/internal/profiles"
	"bpkit/internal/ui"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type pingMsg struct{ err error }

type projectsMsg struct {
	projects []bpclient.Project
	err      error
}

// DefaultServer prefills the server field of a new profile.
const DefaultServer = "https://app.bindplane.com"

func (m *Model) reloadProfiles() {
	ps, err := m.store.List()
	if err != nil {
		m.msg = "profiles: " + err.Error()
	}
	m.profs = ps
	m.defaultName, _ = m.store.Default()
	if m.profCur >= len(ps) {
		m.profCur = max(0, len(ps)-1)
	}
}

// useProfile connects with p and returns to the menu.
func (m Model) useProfile(p profiles.Profile) (tea.Model, tea.Cmd) {
	c, err := dial(p)
	if err != nil {
		m.msg = err.Error()
		return m, nil
	}
	m.client, m.active = c, p
	// The running extension's data belongs to the old connection.
	m.child, m.view = nil, vMenu
	m.msg, m.note = "", "connected as "+p.Title()
	return m, tea.ClearScreen
}

func (m Model) updateProfiles(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.profDelete {
		m.profDelete = false
		if k.String() == "y" && len(m.profs) > 0 {
			name := m.profs[m.profCur].Name
			if err := m.store.Delete(name); err != nil {
				m.msg = err.Error()
			} else {
				m.note = fmt.Sprintf("deleted %q", name)
				if strings.EqualFold(name, m.active.Name) {
					m.client, m.active = nil, profiles.Profile{}
				}
				m.reloadProfiles()
			}
		} else {
			m.note = "delete cancelled"
		}
		return m, nil
	}
	m.msg, m.note = "", ""
	switch k.String() {
	case "esc", "q":
		if m.client == nil {
			return m, tea.Quit
		}
		m.view = vMenu
	case "up", "k":
		m.profCur = max(0, m.profCur-1)
	case "down", "j":
		m.profCur = min(len(m.profs)-1, m.profCur+1)
	case "enter":
		if len(m.profs) > 0 {
			return m.useProfile(m.profs[m.profCur])
		}
	case "a":
		return m.openForm(-1)
	case "e":
		if len(m.profs) > 0 {
			return m.openForm(m.profCur)
		}
	case "d":
		if len(m.profs) > 0 {
			m.profDelete = true
		}
	case "s":
		if len(m.profs) > 0 {
			name := m.profs[m.profCur].Name
			if strings.EqualFold(name, m.defaultName) {
				name = ""
			}
			if err := m.store.SetDefault(name); err != nil {
				m.msg = err.Error()
			} else if name == "" {
				m.note = "default profile cleared"
			} else {
				m.note = fmt.Sprintf("%q is now the default (used on startup)", name)
			}
			m.reloadProfiles()
		}
	}
	return m, nil
}

func (m Model) viewProfiles(b *strings.Builder) {
	fmt.Fprintf(b, "%s %s\n\n", ui.Title.Render("Profiles"), ui.Dim.Render("("+m.store.Path()+")"))
	if len(m.profs) == 0 {
		b.WriteString(ui.Dim.Render("No profiles yet. Press a to add one.") + "\n")
	}
	for i, p := range m.profs {
		star := "  "
		if strings.EqualFold(p.Name, m.active.Name) {
			star = "* "
		}
		def := ""
		if strings.EqualFold(p.Name, m.defaultName) {
			def = "  (default)"
		}
		line := fmt.Sprintf("%s%-20s %-34s %-18s %s%s",
			star, ui.Trunc(p.Name, 20), ui.Trunc(p.Server, 34),
			ui.Trunc(p.Project, 18), profiles.Mask(p.APIKey), def)
		if i == m.profCur {
			line = ui.Cur.Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	if m.profDelete && len(m.profs) > 0 {
		b.WriteString(ui.Bad.Render(fmt.Sprintf("Delete profile %q? press y to confirm, any other key cancels", m.profs[m.profCur].Name)) + "\n")
		return
	}
	b.WriteString(ui.Keys("up/down move", "enter use", "s set/clear default", "a add", "e edit", "d delete", "esc back") + "\n")
	b.WriteString(ui.Dim.Render("* = connected. Two profiles that differ only by project let the migrator copy between projects.") + "\n")
}

// ------------------------------------------------------------- the form

// form is the add/edit profile form. Field order matches the display.
type form struct {
	name, server, key, project textinput.Model
	skipTLS                    bool
	focus                      int
	idx                        int // index being edited, -1 when adding
	editing                    bool
}

const formFields = 5 // name, server, key, project, skipTLS

func newForm() form {
	mk := func(ph string) textinput.Model {
		t := textinput.New()
		t.Placeholder = ph
		t.Width = 60
		return t
	}
	f := form{
		name:    mk("e.g. acme-prod"),
		server:  mk(DefaultServer),
		key:     mk("Bindplane API key"),
		project: mk("optional; required for a key scoped to several projects"),
		idx:     -1,
	}
	f.server.SetValue(DefaultServer)
	f.key.EchoMode = textinput.EchoPassword
	f.key.EchoCharacter = '*'
	return f
}

// openForm starts the add (idx -1) or edit form.
func (m Model) openForm(idx int) (tea.Model, tea.Cmd) {
	m.form = newForm()
	m.form.idx = idx
	if idx >= 0 {
		p := m.profs[idx]
		m.form.name.SetValue(p.Name)
		m.form.server.SetValue(p.Server)
		m.form.project.SetValue(p.Project)
		m.form.key.Placeholder = "(unchanged: " + profiles.Mask(p.APIKey) + ")"
		m.form.skipTLS = p.SkipTLS
	}
	m.view, m.msg, m.note = vProfileForm, "", ""
	return m, nil
}

func (m *Model) formEdit(on bool) {
	m.form.editing = on
	m.form.name.Blur()
	m.form.server.Blur()
	m.form.key.Blur()
	m.form.project.Blur()
	if !on {
		return
	}
	switch m.form.focus {
	case 0:
		m.form.name.Focus()
	case 1:
		m.form.server.Focus()
	case 2:
		m.form.key.Focus()
	case 3:
		m.form.project.Focus()
	}
}

// formProfile builds the profile being edited, keeping the stored key when
// the key line was left empty.
func (m Model) formProfile() (old string, p profiles.Profile) {
	p = profiles.Profile{
		Name:    m.form.name.Value(),
		Server:  m.form.server.Value(),
		APIKey:  m.form.key.Value(),
		Project: m.form.project.Value(),
		SkipTLS: m.form.skipTLS,
	}
	if m.form.idx >= 0 {
		old = m.profs[m.form.idx].Name
		if strings.TrimSpace(p.APIKey) == "" {
			p.APIKey = m.profs[m.form.idx].APIKey
		}
	}
	return
}

func (m Model) updateForm(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.form.editing {
		switch k.String() {
		case "esc", "enter":
			m.formEdit(false)
			return m, nil
		}
		var cmd tea.Cmd
		switch m.form.focus {
		case 0:
			m.form.name, cmd = m.form.name.Update(k)
		case 1:
			m.form.server, cmd = m.form.server.Update(k)
		case 2:
			m.form.key, cmd = m.form.key.Update(k)
		default:
			m.form.project, cmd = m.form.project.Update(k)
		}
		return m, cmd
	}
	switch k.String() {
	case "esc":
		m.view, m.msg, m.note = vProfiles, "", ""
	case "down", "tab", "j":
		m.form.focus = (m.form.focus + 1) % formFields
	case "up", "shift+tab", "k":
		m.form.focus = (m.form.focus + formFields - 1) % formFields
	case "enter", "i":
		if m.form.focus == formFields-1 {
			m.form.skipTLS = !m.form.skipTLS
			return m, nil
		}
		m.formEdit(true)
		return m, textinput.Blink
	case "l":
		// List the projects this key can reach and pick one.
		return m.loadProjects()
	case "s":
		old, p := m.formProfile()
		if err := m.store.Save(old, p); err != nil {
			m.msg, m.note = err.Error(), ""
			return m, nil
		}
		m.reloadProfiles()
		for i, e := range m.profs {
			if strings.EqualFold(e.Name, p.Name) {
				m.profCur = i
			}
		}
		if old != "" && strings.EqualFold(old, m.active.Name) {
			m.active = p // renamed or re-keyed; reconnect on next select
		}
		m.view, m.msg, m.note = vProfiles, "", fmt.Sprintf("saved %q (enter to use it)", p.Name)
	case "t":
		_, p := m.formProfile()
		c, err := formClient(p)
		if err != nil {
			m.msg, m.note = err.Error(), ""
			return m, nil
		}
		m.msg, m.note = "", "testing connection..."
		return m, func() tea.Msg { return pingMsg{c.Ping(context.Background())} }
	}
	return m, nil
}

func formClient(p profiles.Profile) (*bpclient.Client, error) {
	return bpclient.New(bpclient.Options{
		BaseURL: p.Server, APIKey: p.APIKey, Project: p.Project,
		SkipTLSVerify: p.SkipTLS, Timeout: 15 * time.Second,
	})
}

func (m Model) viewForm(b *strings.Builder) {
	verb := "Add profile"
	if m.form.idx >= 0 {
		verb = "Edit profile"
	}
	b.WriteString(ui.Title.Render(verb) + "\n\n")
	mark := [formFields]string{}
	for i := range mark {
		mark[i] = "  "
	}
	mark[m.form.focus] = "> "
	if m.form.editing {
		mark[m.form.focus] = "* "
	}
	tls := "no"
	if m.form.skipTLS {
		tls = "yes (lab only)"
	}
	fmt.Fprintf(b, "%sName:        %s\n", mark[0], m.form.name.View())
	fmt.Fprintf(b, "%sServer:      %s\n", mark[1], m.form.server.View())
	fmt.Fprintf(b, "%sAPI key:     %s\n", mark[2], m.form.key.View())
	fmt.Fprintf(b, "%sProject:     %s\n", mark[3], m.form.project.View())
	fmt.Fprintf(b, "%sSkip TLS:    %s\n\n", mark[4], tls)
	b.WriteString(ui.Dim.Render("Project is sent as X-Bindplane-Account-ID. Leave it empty for a key that already\ncarries its own project; set it for a key scoped to several.") + "\n\n")
	if m.form.editing {
		b.WriteString(ui.Dim.Render("INPUT  enter/esc done") + "\n")
	} else {
		b.WriteString(ui.Keys("up/down switch line", "enter edit/toggle", "l list projects", "t test", "s save", "esc cancel") + "\n")
	}
}

// ---------------------------------------------------- the project picker

// loadProjects asks the server which projects the key in the form can reach.
func (m Model) loadProjects() (tea.Model, tea.Cmd) {
	_, p := m.formProfile()
	// Listing projects is an org-level call, so ask without a project scope.
	p.Project = ""
	c, err := formClient(p)
	if err != nil {
		m.msg, m.note = err.Error(), ""
		return m, nil
	}
	m.projBusy, m.msg, m.note = true, "", "listing projects..."
	return m, func() tea.Msg {
		ps, err := c.ListProjects(context.Background())
		return projectsMsg{ps, err}
	}
}

func (m Model) gotProjects(msg projectsMsg) (tea.Model, tea.Cmd) {
	m.projBusy = false
	if msg.err != nil {
		m.msg, m.note = "list projects: "+msg.err.Error(), ""
		return m, nil
	}
	if len(msg.projects) == 0 {
		m.msg, m.note = "", "no projects returned; type the project id by hand"
		return m, nil
	}
	m.projects, m.projCur = msg.projects, 0
	for i, p := range msg.projects {
		if p.ID == strings.TrimSpace(m.form.project.Value()) {
			m.projCur = i
		}
	}
	m.view, m.msg, m.note = vProjects, "", ""
	return m, nil
}

func (m Model) updateProjects(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "q":
		m.view = vProfileForm
	case "up", "k":
		m.projCur = max(0, m.projCur-1)
	case "down", "j":
		m.projCur = min(len(m.projects)-1, m.projCur+1)
	case "enter":
		if len(m.projects) > 0 {
			p := m.projects[m.projCur]
			m.form.project.SetValue(p.ID)
			// A fresh profile with no name takes the project's.
			if strings.TrimSpace(m.form.name.Value()) == "" {
				m.form.name.SetValue(p.Title())
			}
			m.view, m.note = vProfileForm, "project set to "+p.Title()
		}
	}
	return m, nil
}

func (m Model) viewProjects(b *strings.Builder) {
	b.WriteString(ui.Title.Render("Projects") + "\n\n")
	for i, p := range m.projects {
		line := fmt.Sprintf("  %-34s %s", ui.Trunc(p.Title(), 34), ui.Dim.Render(p.ID))
		if i == m.projCur {
			line = ui.Cur.Render(fmt.Sprintf("  %-34s %s", ui.Trunc(p.Title(), 34), p.ID))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + ui.Keys("up/down move", "enter select", "esc back") + "\n")
}
