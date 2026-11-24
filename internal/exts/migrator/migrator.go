// Package migrator is the bpkit extension that copies configurations, and
// everything they depend on, from one Bindplane project to another.
package migrator

import (
	"context"
	"fmt"
	"strings"

	"bpkit/internal/bpclient"
	"bpkit/internal/ext"
	"bpkit/internal/migrate"
	"bpkit/internal/profiles"
	"bpkit/internal/ui"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// Ext registers the migrator on the bpkit menu.
type Ext struct{}

func (Ext) Meta() ext.Meta {
	return ext.Meta{
		ID:          "migrate",
		Title:       "Config migrator",
		Desc:        "Copy configurations and their dependencies to another project",
		Connections: 2,
	}
}

func (Ext) New(c ext.Context) (tea.Model, tea.Cmd) {
	m := New(c)
	return m, m.Init()
}

type stage int

const (
	stDest stage = iota // pick the destination profile
	stConfigs
	stBuilding
	stSecrets
	stPlan
	stDetail
	stConfirm
	stApplying
	stDone
)

type configsMsg struct {
	configs []bpclient.Resource
	err     error
}
type planMsg struct {
	plan *migrate.Plan
	err  error
}
type appliedMsg struct {
	results []migrate.Result
	err     error
}

type Model struct {
	ctx ext.Context

	src     *bpclient.Client
	srcName string
	dst     *bpclient.Client
	dstName string

	stage stage
	busy  bool

	// destination picker
	profs   []profiles.Profile
	profCur int

	// configuration picker
	configs  []bpclient.Resource
	view     []bpclient.Resource
	flt      textinput.Model
	cur, top int
	picked   map[string]bool

	// plan review
	plan     *migrate.Plan
	itemCur  int
	itemTop  int
	detTop   int
	secCur   int
	secInput textinput.Model
	editing  bool

	confirm textinput.Model
	results []migrate.Result

	width, height int
	msg, note     string
}

func New(c ext.Context) Model {
	m := Model{
		ctx:      c,
		src:      c.Client,
		srcName:  c.Active.Title(),
		flt:      ui.Input("filter configurations", ""),
		secInput: ui.Input("value for this parameter", ""),
		confirm:  ui.Input("type confirm", ""),
		picked:   map[string]bool{},
		width:    c.Width,
		height:   c.Height,
	}
	m.secInput.EchoMode = textinput.EchoPassword
	m.secInput.EchoCharacter = '*'
	if m.width == 0 {
		m.width, m.height = 120, 30
	}
	m.profs, _ = c.Store.List()
	return m
}

func (m Model) Init() tea.Cmd { return textinput.Blink }

func (m Model) listH() int { return max(3, m.height-14) }

func (m Model) loadConfigs() tea.Cmd {
	c := m.src
	return func() tea.Msg {
		rs, err := c.ListResources(context.Background(), bpclient.KindConfiguration)
		return configsMsg{rs, err}
	}
}

func (m Model) build() tea.Cmd {
	src, dst := m.src, m.dst
	var names []string
	for _, c := range m.configs {
		if m.picked[c.Name()] {
			names = append(names, c.Name())
		}
	}
	return func() tea.Msg {
		p, err := migrate.Build(context.Background(), src, dst, names)
		return planMsg{p, err}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case configsMsg:
		m.busy = false
		if msg.err != nil {
			m.msg = "list configurations: " + msg.err.Error()
			return m, nil
		}
		m.configs = msg.configs
		m.refilter()
	case planMsg:
		m.busy = false
		if msg.err != nil {
			m.stage, m.msg = stConfigs, "build plan: "+msg.err.Error()
			return m, nil
		}
		m.plan, m.msg = msg.plan, ""
		m.itemCur, m.itemTop, m.secCur = 0, 0, 0
		if len(m.plan.Pending()) > 0 {
			m.stage = stSecrets
			m.secInput.SetValue("")
		} else {
			m.stage = stPlan
		}
	case appliedMsg:
		m.busy = false
		m.results = msg.results
		if msg.err != nil {
			m.msg = msg.err.Error()
		}
		m.stage = stDone
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m Model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.stage {
	case stDest:
		return m.updateDest(k)
	case stConfigs:
		return m.updateConfigs(k)
	case stSecrets:
		return m.updateSecrets(k)
	case stPlan:
		return m.updatePlan(k)
	case stDetail:
		return m.updateDetail(k)
	case stConfirm:
		return m.updateConfirm(k)
	case stDone:
		switch k.String() {
		case "q", "esc", "enter":
			return m, ext.Leave
		}
	}
	return m, nil
}

// --------------------------------------------- pick the destination project

func (m Model) updateDest(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.msg, m.note = "", ""
	switch k.String() {
	case "esc", "q":
		return m, ext.Leave
	case "up", "k":
		m.profCur = max(0, m.profCur-1)
	case "down", "j":
		m.profCur = min(len(m.profs)-1, m.profCur+1)
	case "enter":
		if len(m.profs) == 0 {
			return m, nil
		}
		p := m.profs[m.profCur]
		if strings.EqualFold(p.Name, m.ctx.Active.Name) && p.Project == m.ctx.Active.Project {
			m.msg = "the destination must differ from the source"
			return m, nil
		}
		c, err := m.ctx.Dial(p)
		if err != nil {
			m.msg = err.Error()
			return m, nil
		}
		m.dst, m.dstName = c, p.Title()
		m.stage, m.busy = stConfigs, true
		return m, m.loadConfigs()
	}
	return m, nil
}

// ------------------------------------------------- pick the configurations

func (m *Model) refilter() {
	f := strings.ToLower(strings.TrimSpace(m.flt.Value()))
	m.view = nil
	for _, c := range m.configs {
		if f == "" || strings.Contains(strings.ToLower(c.Title()), f) || strings.Contains(strings.ToLower(c.Name()), f) {
			m.view = append(m.view, c)
		}
	}
	if m.cur >= len(m.view) {
		m.cur = max(0, len(m.view)-1)
	}
	m.top = ui.Scroll(m.top, m.cur, m.listH())
}

func (m Model) updateConfigs(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.msg, m.note = "", ""
	switch k.String() {
	case "esc":
		m.stage = stDest
		return m, nil
	case "up", "ctrl+p":
		m.cur = max(0, m.cur-1)
		m.top = ui.Scroll(m.top, m.cur, m.listH())
		return m, nil
	case "down", "ctrl+n":
		m.cur = min(len(m.view)-1, m.cur+1)
		m.top = ui.Scroll(m.top, m.cur, m.listH())
		return m, nil
	case " ":
		if len(m.view) > 0 {
			name := m.view[m.cur].Name()
			if m.picked[name] {
				delete(m.picked, name)
			} else {
				m.picked[name] = true
			}
		}
		return m, nil
	case "ctrl+a":
		for _, c := range m.view {
			m.picked[c.Name()] = true
		}
		return m, nil
	case "enter":
		if len(m.picked) == 0 {
			m.msg = "select at least one configuration with space"
			return m, nil
		}
		m.stage, m.busy = stBuilding, true
		return m, m.build()
	}
	var cmd tea.Cmd
	m.flt.Focus()
	m.flt, cmd = m.flt.Update(k)
	m.refilter()
	return m, cmd
}

// ------------------------------------------------------- fill the secrets

func (m Model) updateSecrets(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	secs := m.plan.Pending()
	if m.editing {
		switch k.String() {
		case "esc":
			m.editing = false
			m.secInput.Blur()
			return m, nil
		case "enter":
			if v := m.secInput.Value(); v != "" {
				secs[m.secCur].Set(v)
				m.note = "set " + secs[m.secCur].Param
			}
			m.editing = false
			m.secInput.Blur()
			m.secInput.SetValue("")
			// Move on to the next one still empty.
			for i := m.secCur + 1; i < len(secs); i++ {
				if !secs[i].Filled() {
					m.secCur = i
					break
				}
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.secInput, cmd = m.secInput.Update(k)
		return m, cmd
	}
	m.msg = ""
	switch k.String() {
	case "esc":
		m.stage = stConfigs
	case "up", "k":
		m.secCur = max(0, m.secCur-1)
	case "down", "j":
		m.secCur = min(len(secs)-1, m.secCur+1)
	case "enter", "i":
		if len(secs) > 0 {
			m.editing = true
			m.secInput.SetValue("")
			m.secInput.Focus()
			return m, textinput.Blink
		}
	case "c":
		// Continue, leaving the rest unset.
		m.stage, m.note = stPlan, ""
		if n := len(m.plan.Unfilled()); n > 0 {
			m.msg = fmt.Sprintf("%d credential(s) left unset — Bindplane refuses those resources outright, so exclude them or go back and fill them in", n)
		}
	}
	return m, nil
}

// --------------------------------------------------------- review the plan

func (m Model) updatePlan(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.msg, m.note = "", ""
	items := m.plan.Items
	switch k.String() {
	case "esc":
		if len(m.plan.Pending()) > 0 {
			m.stage = stSecrets
		} else {
			m.stage = stConfigs
		}
	case "q":
		return m, ext.Leave
	case "up", "k":
		m.itemCur = max(0, m.itemCur-1)
		m.itemTop = ui.Scroll(m.itemTop, m.itemCur, m.listH())
	case "down", "j":
		m.itemCur = min(len(items)-1, m.itemCur+1)
		m.itemTop = ui.Scroll(m.itemTop, m.itemCur, m.listH())
	case " ":
		if len(items) > 0 {
			items[m.itemCur].Skip = !items[m.itemCur].Skip
		}
	case "o":
		// Take every conflict, replacing the destination's versions.
		n := 0
		for i := range items {
			if items[i].Status == migrate.StatusConflict && items[i].Skip {
				items[i].Skip, n = false, n+1
			}
		}
		m.note = fmt.Sprintf("%d conflict(s) set to overwrite", n)
	case "d":
		if len(items) > 0 && items[m.itemCur].Status == migrate.StatusConflict {
			m.stage, m.detTop = stDetail, 0
		}
	case "enter":
		apply, _, _ := m.plan.Counts()
		if apply == 0 {
			m.msg = "nothing selected to apply"
			return m, nil
		}
		m.stage = stConfirm
		m.confirm.SetValue("")
		m.confirm.Focus()
		return m, textinput.Blink
	}
	return m, nil
}

func (m Model) updateDetail(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "q", "d":
		m.stage = stPlan
	case "up", "k":
		m.detTop = max(0, m.detTop-1)
	case "down", "j":
		m.detTop++
	}
	return m, nil
}

func (m Model) updateConfirm(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.confirm.Blur()
		m.stage, m.msg = stPlan, ""
		return m, nil
	case "enter":
		if strings.TrimSpace(m.confirm.Value()) != "confirm" {
			m.msg = "type confirm to apply, or esc to go back"
			return m, nil
		}
		m.confirm.Blur()
		m.stage, m.busy, m.msg = stApplying, true, ""
		p, dst := m.plan, m.dst
		return m, func() tea.Msg {
			res, err := p.Apply(context.Background(), dst)
			return appliedMsg{res, err}
		}
	}
	var cmd tea.Cmd
	m.confirm, cmd = m.confirm.Update(k)
	return m, cmd
}
