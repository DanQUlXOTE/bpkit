package shell

import (
	"path/filepath"
	"strings"
	"testing"

	"bpkit/internal/ext"
	"bpkit/internal/profiles"

	tea "github.com/charmbracelet/bubbletea"
)

func send(m Model, msgs ...tea.Msg) Model {
	for _, msg := range msgs {
		nm, cmd := m.Update(msg)
		m = nm.(Model)
		if cmd != nil {
			// Run commands that produce data messages; skip blink timers.
			if out := cmd(); out != nil {
				switch out.(type) {
				case projectsMsg, pingMsg:
					nm, _ = m.Update(out)
					m = nm.(Model)
				}
			}
		}
	}
	return m
}

func key(s string) tea.Msg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+p":
		return tea.KeyMsg{Type: tea.KeyCtrlP}
	case "ctrl+b":
		return tea.KeyMsg{Type: tea.KeyCtrlB}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func newShell(t *testing.T, exts ...ext.Extension) Model {
	t.Helper()
	store := profiles.NewStore(filepath.Join(t.TempDir(), "p.json"))
	return New(store, profiles.Profile{}, nil, exts)
}

func TestProfileManager(t *testing.T) {
	m := newShell(t)
	if m.view != vProfiles {
		t.Fatalf("should start on profiles without a client, got %v", m.view)
	}
	typeIn := func(m Model, s string) Model { // enter input mode, type, leave
		return send(m, key("enter"), key(s), key("enter"))
	}
	// add
	m = send(m, key("a"))
	m = typeIn(m, "acme")
	m = send(m, key("down"))
	m = typeIn(m, "https://acme.example.com")
	m = send(m, key("down"))
	m = typeIn(m, "secret-key-1234")
	m = send(m, key("s"))
	if m.view != vProfiles || len(m.profs) != 1 || m.profs[0].APIKey != "secret-key-1234" {
		t.Fatalf("add: view=%v profs=%+v msg=%q", m.view, m.profs, m.msg)
	}
	if v := m.View(); strings.Contains(v, "secret-key-1234") || !strings.Contains(v, "****1234") {
		t.Fatalf("key must be masked:\n%s", v)
	}
	// edit: rename, key left blank so it is kept
	m = send(m, key("e"))
	m.form.name.SetValue("acme-prod")
	m = send(m, key("s"))
	if m.profs[0].Name != "acme-prod" || m.profs[0].APIKey != "secret-key-1234" {
		t.Fatalf("edit: %+v msg=%q", m.profs, m.msg)
	}
	// use it: connects and lands on the menu
	m = send(m, key("enter"))
	if m.view != vMenu || m.client == nil || m.active.Name != "acme-prod" {
		t.Fatalf("use: view=%v active=%q", m.view, m.active.Name)
	}
	// back into the manager, delete with confirmation
	m = send(m, key("p"), key("d"), key("n"))
	if len(m.profs) != 1 {
		t.Fatal("delete must need y")
	}
	m = send(m, key("d"), key("y"))
	if len(m.profs) != 0 || m.client != nil {
		t.Fatalf("delete: profs=%v client=%v", m.profs, m.client)
	}
}

func TestProfileDefaultKey(t *testing.T) {
	m := newShell(t)
	m.store.Save("", profiles.Profile{Name: "a", Server: "s", APIKey: "k"})
	m.store.Save("", profiles.Profile{Name: "b", Server: "s", APIKey: "k"})
	m.reloadProfiles()
	m = send(m, key("down"), key("s"))
	if m.defaultName != "b" || !strings.Contains(m.View(), "(default)") {
		t.Fatalf("default=%q\n%s", m.defaultName, m.View())
	}
	m = send(m, key("s"))
	if m.defaultName != "" {
		t.Fatalf("toggle should clear, got %q", m.defaultName)
	}
}

// A profile's project must survive a save/edit round trip, since two
// profiles that differ only by project are how the migrator targets
// another project.
func TestProfileKeepsProject(t *testing.T) {
	m := newShell(t)
	m.store.Save("", profiles.Profile{Name: "a", Server: "s", APIKey: "k", Project: "proj-1"})
	m.reloadProfiles()
	m = send(m, key("e"))
	if got := m.form.project.Value(); got != "proj-1" {
		t.Fatalf("form project = %q", got)
	}
	m = send(m, key("s"))
	if m.profs[0].Project != "proj-1" {
		t.Fatalf("project lost on save: %+v", m.profs[0])
	}
	if v := m.View(); !strings.Contains(v, "proj-1") {
		t.Fatalf("project not listed:\n%s", v)
	}
}

// fakeExt records whether it was entered and what connection it got.
type fakeExt struct{ entered *int }

func (fakeExt) Meta() ext.Meta {
	return ext.Meta{ID: "fake", Title: "Fake tool", Desc: "test", Connections: 1}
}
func (f fakeExt) New(c ext.Context) (tea.Model, tea.Cmd) {
	*f.entered++
	return fakeModel{}, nil
}

type fakeModel struct{}

func (fakeModel) Init() tea.Cmd                         { return nil }
func (f fakeModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return f, nil }
func (fakeModel) View() string                          { return "FAKE BODY" }

func TestExtensionHosting(t *testing.T) {
	entered := 0
	m := newShell(t, fakeExt{entered: &entered})
	m.store.Save("", profiles.Profile{Name: "a", Server: "https://s.example.com", APIKey: "k"})
	m.reloadProfiles()
	m = send(m, key("enter")) // connect with the profile

	// The menu lists the extension, and 1 opens it.
	if !strings.Contains(m.View(), "Fake tool") {
		t.Fatalf("menu missing the extension:\n%s", m.View())
	}
	m = send(m, key("1"))
	if m.view != vExt || entered != 1 {
		t.Fatalf("view=%v entered=%d", m.view, entered)
	}
	if v := m.View(); !strings.Contains(v, "FAKE BODY") || !strings.Contains(v, "bpkit · Fake tool") {
		t.Fatalf("extension not hosted:\n%s", v)
	}
	// ctrl+b leaves the extension without quitting bpkit.
	m = send(m, key("ctrl+b"))
	if m.view != vMenu || m.child != nil {
		t.Fatalf("ctrl+b: view=%v child=%v", m.view, m.child)
	}
	// An extension's own Back message does the same.
	m = send(m, key("1"))
	m = send(m, ext.Back{})
	if m.view != vMenu {
		t.Fatalf("ext.Back: view=%v", m.view)
	}
	// ctrl+p reaches the profile manager from inside an extension.
	m = send(m, key("1"), key("ctrl+p"))
	if m.view != vProfiles {
		t.Fatalf("ctrl+p: view=%v", m.view)
	}
}

// Without a connection, opening an extension must send you to the profile
// manager rather than handing the extension a nil client.
func TestExtensionNeedsConnection(t *testing.T) {
	entered := 0
	m := newShell(t, fakeExt{entered: &entered})
	m.view = vMenu
	m = send(m, key("enter"))
	if entered != 0 || m.view != vProfiles {
		t.Fatalf("entered=%d view=%v", entered, m.view)
	}
}
