// Package ui holds the styles and small widgets every bpkit extension shares,
// so the tools look and drive the same way.
package ui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
)

var (
	Title = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	Dim   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	Warn  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	Bad   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	Good  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	Cur   = lipgloss.NewStyle().Reverse(true)
)

// Input builds the one-line text input used throughout bpkit.
func Input(placeholder, value string) textinput.Model {
	t := textinput.New()
	t.Placeholder = placeholder
	t.SetValue(value)
	t.Width = 60
	t.Prompt = ""
	return t
}

// Trunc shortens s to n columns, ending in an ellipsis when it had to cut.
func Trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// Mark renders the cursor/selection gutter for a list row.
func Mark(cursor, selected bool) string {
	c, s := "  ", "[ ] "
	if cursor {
		c = "> "
	}
	if selected {
		s = "[x] "
	}
	return c + s
}

// Scroll clamps top so that cursor stays visible in a window of height rows,
// and returns the new top.
func Scroll(top, cursor, height int) int {
	if height <= 0 {
		return 0
	}
	if cursor < top {
		return cursor
	}
	if cursor >= top+height {
		return cursor - height + 1
	}
	return top
}

// Keys renders a help line, dimmed.
func Keys(parts ...string) string {
	return Dim.Render(strings.Join(parts, "  •  "))
}
