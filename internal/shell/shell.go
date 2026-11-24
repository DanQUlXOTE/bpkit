// Package shell is bpkit's outer program: it owns the connection and the
// saved profiles, lists the extensions, and runs the one you pick. An
// extension never has to think about credentials.
package shell

import (
	"fmt"
	"strings"

	"bpkit/internal/bpclient"
	"bpkit/internal/ext"
	"bpkit/internal/profiles"
	"bpkit/internal/ui"

	tea "github.com/charmbracelet/bubbletea"
)

type view int

const (
	vMenu view = iota
	vProfiles
	vProfileForm
	vProjects
	vExt
)

type Model struct {
	store  *profiles.Store
	active profiles.Profile
	client *bpclient.Client
	exts   []ext.Extension

	view view
	cur  int // menu cursor

	// profile manager
	profs       []profiles.Profile
	profCur     int
	profDelete  bool
	defaultName string

	// profile form and its project picker
	form     form
	projects []bpclient.Project
	projCur  int
	projBusy bool

	// hosted extension
	child     tea.Model
	childMeta ext.Meta

	width, height int
	msg, note     string
}

// New builds the shell. client may be nil, in which case bpkit opens on the
// profile manager so the first thing you do is connect.
func New(store *profiles.Store, active profiles.Profile, client *bpclient.Client, exts []ext.Extension) Model {
	m := Model{
		store:  store,
		active: active,
		client: client,
		exts:   exts,
		width:  120,
		height: 30,
	}
	m.reloadProfiles()
	if client == nil {
		m.view = vProfiles
	}
	return m
}

func (m Model) Init() tea.Cmd { return nil }

// context is what the shell hands an extension on entry.
func (m Model) context() ext.Context {
	return ext.Context{
		Client: m.client,
		Active: m.active,
		Store:  m.store,
		Dial:   dial,
		Width:  m.width,
		Height: m.height,
	}
}

func dial(p profiles.Profile) (*bpclient.Client, error) { return p.Connect() }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.child != nil {
			var cmd tea.Cmd
			m.child, cmd = m.child.Update(msg)
			return m, cmd
		}
		return m, nil

	case ext.Back:
		m.child, m.view, m.msg = nil, vMenu, ""
		return m, nil

	case ext.Status:
		if msg.Err {
			m.msg, m.note = msg.Text, ""
		} else {
			m.msg, m.note = "", msg.Text
		}
		return m, nil

	case projectsMsg:
		return m.gotProjects(msg)

	case pingMsg:
		if msg.err != nil {
			m.msg, m.note = "connection failed: "+msg.err.Error(), ""
		} else {
			m.msg, m.note = "", "connection OK"
		}
		return m, nil

	case tea.KeyMsg:
		return m.key(msg)
	}

	// Anything else (ticks, command results) belongs to the extension.
	if m.child != nil {
		var cmd tea.Cmd
		m.child, cmd = m.child.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m Model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Shell-wide keys win over the extension, so you can always get out.
	switch k.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "ctrl+b":
		if m.view == vExt {
			m.child, m.view, m.msg, m.note = nil, vMenu, "", ""
			return m, nil
		}
	case "ctrl+p":
		if m.view == vExt || m.view == vMenu {
			m.view, m.msg, m.note = vProfiles, "", ""
			m.reloadProfiles()
			return m, nil
		}
	}

	switch m.view {
	case vExt:
		var cmd tea.Cmd
		m.child, cmd = m.child.Update(k)
		return m, cmd
	case vProfiles:
		return m.updateProfiles(k)
	case vProfileForm:
		return m.updateForm(k)
	case vProjects:
		return m.updateProjects(k)
	}
	return m.updateMenu(k)
}

func (m Model) updateMenu(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.msg, m.note = "", ""
	switch k.String() {
	case "q", "esc":
		return m, tea.Quit
	case "up", "k":
		m.cur = max(0, m.cur-1)
	case "down", "j":
		m.cur = min(len(m.exts)-1, m.cur+1)
	case "p":
		m.view = vProfiles
		m.reloadProfiles()
	case "enter":
		return m.enter(m.cur)
	default:
		// Number keys jump straight to an extension.
		if n := int(k.String()[0]) - '1'; len(k.String()) == 1 && n >= 0 && n < len(m.exts) {
			m.cur = n
			return m.enter(n)
		}
	}
	return m, nil
}

// enter starts extension i, refusing when there is no connection yet.
func (m Model) enter(i int) (tea.Model, tea.Cmd) {
	if i < 0 || i >= len(m.exts) {
		return m, nil
	}
	e := m.exts[i]
	meta := e.Meta()
	if meta.Connections > 0 && m.client == nil {
		m.msg = "not connected: press p to pick a profile"
		m.view = vProfiles
		return m, nil
	}
	child, cmd := e.New(m.context())
	m.child, m.childMeta, m.view = child, meta, vExt
	m.msg, m.note = "", ""
	// Hand the extension the terminal size it was born into.
	if m.width > 0 && m.height > 0 {
		m.child, _ = m.child.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height - headerLines})
	}
	return m, tea.Batch(cmd, tea.ClearScreen)
}

// headerLines is the room the shell keeps for its own header above an
// extension, so the extension can size its lists correctly.
const headerLines = 2

func (m Model) View() string {
	var b strings.Builder

	switch m.view {
	case vExt:
		fmt.Fprintf(&b, "%s %s\n", ui.Title.Render("bpkit · "+m.childMeta.Title), ui.Dim.Render(m.connLabel()))
		b.WriteString(m.child.View())
		if m.msg != "" {
			b.WriteString("\n" + ui.Bad.Render(ui.Wrap(m.msg, m.wrapW())))
		} else if m.note != "" {
			b.WriteString("\n" + ui.Good.Render(ui.Wrap(m.note, m.wrapW())))
		}
		return b.String()
	case vProfiles:
		m.viewProfiles(&b)
	case vProfileForm:
		m.viewForm(&b)
	case vProjects:
		m.viewProjects(&b)
	default:
		m.viewMenu(&b)
	}

	if m.msg != "" {
		b.WriteString("\n" + ui.Bad.Render(ui.Wrap(m.msg, m.wrapW())) + "\n")
	} else if m.note != "" {
		b.WriteString("\n" + ui.Good.Render(ui.Wrap(m.note, m.wrapW())) + "\n")
	}
	return b.String()
}

// wrapW is the width long messages reflow to, leaving a small gutter.
func (m Model) wrapW() int {
	if m.width <= 0 {
		return 100
	}
	return max(20, m.width-2)
}

func (m Model) connLabel() string {
	if m.client == nil {
		return "not connected"
	}
	label := m.active.Name
	if label == "" {
		label = m.client.BaseURL()
	}
	if p := m.client.Project(); p != "" {
		label += " · " + p
	}
	return label
}

func (m Model) viewMenu(b *strings.Builder) {
	fmt.Fprintf(b, "%s\n", ui.Title.Render("bpkit"))
	fmt.Fprintf(b, "%s\n\n", ui.Dim.Render("Bindplane API toolkit · connected to "+m.connLabel()))

	for i, e := range m.exts {
		meta := e.Meta()
		line := fmt.Sprintf(" %d. %-22s %s", i+1, meta.Title, ui.Dim.Render(meta.Desc))
		if i == m.cur {
			line = ui.Cur.Render(fmt.Sprintf(" %d. %-22s %s", i+1, meta.Title, meta.Desc))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	b.WriteString(ui.Keys("up/down move", "enter or 1-9 open", "p profiles", "q quit") + "\n")
	b.WriteString(ui.Dim.Render("inside a tool: ctrl+b back to this menu, ctrl+p profiles") + "\n")
}
