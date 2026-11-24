// Package ext defines what an extension is. bpkit's shell owns the things
// every tool needs — profiles, credentials, connections — and hands them to
// an extension through a Context. An extension is then just a bubbletea
// model that does one job against the Bindplane API.
package ext

import (
	"bpkit/internal/bpclient"
	"bpkit/internal/profiles"

	tea "github.com/charmbracelet/bubbletea"
)

// Meta describes an extension on the bpkit menu.
type Meta struct {
	// ID names the extension on the command line, e.g. "label".
	ID string
	// Title and Desc are the menu entry.
	Title string
	Desc  string
	// Connections is how many Bindplane connections the extension works
	// with: 1 for most, 2 for the migrator (source and destination).
	Connections int
}

// Context is what the shell gives an extension on entry.
type Context struct {
	// Client is the active connection. Extensions that need a second one
	// ask the user for a profile and call Dial.
	Client *bpclient.Client
	// Active is the profile Client was built from.
	Active profiles.Profile
	// Store lists the saved profiles, for extensions that pick a second
	// connection.
	Store *profiles.Store
	// Dial opens a connection for a profile.
	Dial func(profiles.Profile) (*bpclient.Client, error)

	Width, Height int
}

// Extension is one tool in bpkit.
type Extension interface {
	Meta() Meta
	// New returns the extension's root model. It is called each time the
	// user enters the extension, so an extension always starts clean.
	New(Context) (tea.Model, tea.Cmd)
}

// Back tells the shell to leave the current extension and return to the
// menu. An extension sends it instead of tea.Quit, so quitting a tool never
// quits bpkit.
type Back struct{}

// Leave is the command an extension returns when the user backs out of it.
func Leave() tea.Msg { return Back{} }

// Status asks the shell to show a line under the extension, for notices that
// outlive a single screen.
type Status struct {
	Text string
	Err  bool
}

// Say returns a command that posts a status line.
func Say(text string) tea.Cmd {
	return func() tea.Msg { return Status{Text: text} }
}

// Fail returns a command that posts an error line.
func Fail(err error) tea.Cmd {
	return func() tea.Msg { return Status{Text: err.Error(), Err: true} }
}
