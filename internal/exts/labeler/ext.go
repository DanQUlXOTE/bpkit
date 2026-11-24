package labeler

import (
	"bpkit/internal/ext"

	tea "github.com/charmbracelet/bubbletea"
)

// Ext registers the labeler on the bpkit menu.
type Ext struct {
	// Query, Selector and Version prefill the search screen, so command-line
	// flags can drop you straight into a filtered list.
	Query, Selector, Version string
}

func (Ext) Meta() ext.Meta {
	return ext.Meta{
		ID:          "label",
		Title:       "Collector labeler",
		Desc:        "Bulk set/remove labels, assign a fleet, upgrade collectors",
		Connections: 1,
	}
}

func (e Ext) New(c ext.Context) (tea.Model, tea.Cmd) {
	m := New(c.Client, e.Query, e.Selector, e.Version)
	return m, m.Init()
}
