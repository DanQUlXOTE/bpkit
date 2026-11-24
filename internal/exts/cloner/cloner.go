// Package cloner is the bpkit extension that bulk-adds Windows Event sources
// to a configuration by cloning a template source already in it, one new
// source per CSV row.
package cloner

import (
	"context"
	"fmt"
	"os"
	"strings"

	"bpkit/internal/bpclient"
	"bpkit/internal/cloner"
	"bpkit/internal/ext"
	"bpkit/internal/ui"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"
)

// Ext registers the source cloner on the bpkit menu.
type Ext struct{}

func (Ext) Meta() ext.Meta {
	return ext.Meta{
		ID:          "clone",
		Title:       "Source cloner",
		Desc:        "Clone a template Windows Event source once per CSV row",
		Connections: 1,
	}
}

func (Ext) New(c ext.Context) (tea.Model, tea.Cmd) {
	m := New(c.Client)
	return m, m.Init()
}

type stage int

const (
	stConfigs stage = iota
	stLoadingConfig
	stSources
	stCSV
	stCreds
	stPreview
	stMode
	stConfirm
	stApplying
	stDone
)

// mode is what we do with the modified configuration.
type mode int

const (
	modeDryRun mode = iota // write YAML to a local file
	modeVerify             // apply as a new configuration under another name
	modeUpdate             // apply in place over the original
)

func (md mode) String() string {
	switch md {
	case modeVerify:
		return "verify"
	case modeUpdate:
		return "update"
	}
	return "dry-run"
}

func (md mode) desc() string {
	switch md {
	case modeVerify:
		return "apply as a NEW configuration you name, to eyeball in the UI first"
	case modeUpdate:
		return "apply in place, replacing the original configuration"
	}
	return "write the modified YAML to a local file, touching nothing on the server"
}

type configsMsg struct {
	configs []bpclient.Resource
	err     error
}
type configMsg struct {
	config bpclient.Resource
	err    error
}
type appliedMsg struct {
	out string
	err error
}

type Model struct {
	client *bpclient.Client
	stage  stage

	// configuration picking
	configs  []bpclient.Resource
	cfgView  []bpclient.Resource
	cfgFlt   textinput.Model
	cfgCur   int
	cfgTop   int
	cfgName  string
	cfg      bpclient.Resource // the full configuration being edited
	busy     bool
	srcCur   int
	srcTop   int
	sources  []srcEntry
	template string

	// inputs
	csvPath textinput.Model
	user    textinput.Model
	pass    textinput.Model
	domain  textinput.Model
	outPath textinput.Model
	newName textinput.Model
	confirm textinput.Model
	focus   int
	editing bool

	md      mode
	rows    []cloner.Row
	result  *cloner.Result
	prevTop int

	width, height int
	msg, note     string
	done          string
}

type srcEntry struct {
	label string
	typ   string
	ok    bool // a Windows Event source we can clone
}

func New(c *bpclient.Client) Model {
	m := Model{
		client:  c,
		cfgFlt:  ui.Input("filter configurations", ""),
		csvPath: ui.Input("path to CSV with a hostname column", ""),
		user:    ui.Input("Windows username, shared by every new source", ""),
		pass:    ui.Input("Windows password", ""),
		domain:  ui.Input("Windows domain (optional)", ""),
		outPath: ui.Input("(default: <config>-new.yaml)", ""),
		newName: ui.Input("name for the verification configuration", ""),
		confirm: ui.Input("type confirm", ""),
		width:   120,
		height:  30,
		busy:    true,
	}
	m.pass.EchoMode = textinput.EchoPassword
	m.pass.EchoCharacter = '*'
	return m
}

func (m Model) Init() tea.Cmd { return tea.Batch(textinput.Blink, m.loadConfigs()) }

func (m Model) loadConfigs() tea.Cmd {
	c := m.client
	return func() tea.Msg {
		rs, err := c.ListResources(context.Background(), bpclient.KindConfiguration)
		return configsMsg{rs, err}
	}
}

func (m Model) loadConfig(name string) tea.Cmd {
	c := m.client
	return func() tea.Msg {
		r, err := c.GetResource(context.Background(), bpclient.KindConfiguration, name)
		return configMsg{r, err}
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
	case configMsg:
		m.busy = false
		if msg.err != nil {
			m.stage, m.msg = stConfigs, "load configuration: "+msg.err.Error()
			return m, nil
		}
		m.cfg = msg.config
		m.sources = listSources(m.cfg)
		m.srcCur, m.srcTop = firstClonable(m.sources), 0
		if len(m.sources) == 0 {
			m.stage, m.msg = stConfigs, "configuration has no sources to clone"
			return m, nil
		}
		m.stage, m.msg = stSources, ""
	case appliedMsg:
		if msg.err != nil {
			m.stage, m.msg = stMode, msg.err.Error()
			return m, nil
		}
		m.stage, m.done, m.msg = stDone, msg.out, ""
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m Model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.stage {
	case stConfigs:
		return m.updateConfigs(k)
	case stSources:
		return m.updateSources(k)
	case stCSV:
		return m.updateCSV(k)
	case stCreds:
		return m.updateCreds(k)
	case stPreview:
		return m.updatePreview(k)
	case stMode:
		return m.updateMode(k)
	case stConfirm:
		return m.updateConfirm(k)
	case stDone:
		switch k.String() {
		case "q", "esc", "enter":
			return m, ext.Leave
		case "n":
			return New(m.client), m.loadConfigs()
		}
	}
	return m, nil
}

// -------------------------------------------------------- pick the config

func (m *Model) refilter() {
	f := strings.ToLower(strings.TrimSpace(m.cfgFlt.Value()))
	m.cfgView = nil
	for _, c := range m.configs {
		if f == "" || strings.Contains(strings.ToLower(c.Title()), f) || strings.Contains(strings.ToLower(c.Name()), f) {
			m.cfgView = append(m.cfgView, c)
		}
	}
	if m.cfgCur >= len(m.cfgView) {
		m.cfgCur = max(0, len(m.cfgView)-1)
	}
	m.cfgTop = ui.Scroll(m.cfgTop, m.cfgCur, m.listH())
}

func (m Model) listH() int { return max(3, m.height-14) }

func (m Model) updateConfigs(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.msg, m.note = "", ""
	switch k.String() {
	case "esc":
		return m, ext.Leave
	case "up", "ctrl+p":
		m.cfgCur = max(0, m.cfgCur-1)
		m.cfgTop = ui.Scroll(m.cfgTop, m.cfgCur, m.listH())
		return m, nil
	case "down", "ctrl+n":
		m.cfgCur = min(len(m.cfgView)-1, m.cfgCur+1)
		m.cfgTop = ui.Scroll(m.cfgTop, m.cfgCur, m.listH())
		return m, nil
	case "enter":
		if len(m.cfgView) == 0 {
			return m, nil
		}
		m.cfgName = m.cfgView[m.cfgCur].Name()
		m.stage, m.busy = stLoadingConfig, true
		return m, m.loadConfig(m.cfgName)
	}
	var cmd tea.Cmd
	m.cfgFlt.Focus()
	m.cfgFlt, cmd = m.cfgFlt.Update(k)
	m.refilter()
	return m, cmd
}

// ------------------------------------------------------ pick the template

// listSources describes the configuration's sources for the picker,
// marking the ones that are Windows Event sources.
func listSources(cfg bpclient.Resource) []srcEntry {
	spec := cfg.Spec()
	if spec == nil {
		return nil
	}
	list, _ := spec["sources"].([]any)
	out := make([]srcEntry, 0, len(list))
	for _, s := range list {
		sm, _ := s.(map[string]any)
		name, _ := sm["name"].(string)
		display, _ := sm["displayName"].(string)
		typ, _ := sm["type"].(string)
		label := display
		if label == "" {
			label = name
		}
		out = append(out, srcEntry{label: label, typ: typ, ok: cloner.IsWindowsEvent(typ)})
	}
	return out
}

func firstClonable(ss []srcEntry) int {
	for i, s := range ss {
		if s.ok {
			return i
		}
	}
	return 0
}

func (m Model) updateSources(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.msg, m.note = "", ""
	switch k.String() {
	case "esc", "q":
		m.stage = stConfigs
	case "up", "k":
		m.srcCur = max(0, m.srcCur-1)
		m.srcTop = ui.Scroll(m.srcTop, m.srcCur, m.listH())
	case "down", "j":
		m.srcCur = min(len(m.sources)-1, m.srcCur+1)
		m.srcTop = ui.Scroll(m.srcTop, m.srcCur, m.listH())
	case "enter":
		if len(m.sources) == 0 {
			return m, nil
		}
		s := m.sources[m.srcCur]
		if !s.ok {
			m.msg = fmt.Sprintf("%q is a %s source; only Windows Event sources can be cloned", s.label, s.typ)
			return m, nil
		}
		// The cloner matches by index, which is unambiguous even when two
		// sources share a display name.
		m.template = fmt.Sprintf("%d", m.srcCur)
		m.stage, m.focus = stCSV, 0
		m.csvPath.Focus()
		return m, textinput.Blink
	}
	return m, nil
}

// ------------------------------------------------------------- CSV + creds

func (m Model) updateCSV(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.csvPath.Blur()
		m.stage = stSources
		return m, nil
	case "enter":
		path := strings.TrimSpace(m.csvPath.Value())
		f, err := os.Open(expand(path))
		if err != nil {
			m.msg = err.Error()
			return m, nil
		}
		defer f.Close()
		rows, err := cloner.ReadCSV(f)
		if err != nil {
			m.msg = "parse csv: " + err.Error()
			return m, nil
		}
		if len(rows) == 0 {
			m.msg = "csv has no rows"
			return m, nil
		}
		m.rows, m.msg = rows, ""
		m.note = fmt.Sprintf("%d row(s) read", len(rows))
		m.csvPath.Blur()
		m.stage, m.focus, m.editing = stCreds, 0, false
		return m, nil
	}
	var cmd tea.Cmd
	m.csvPath, cmd = m.csvPath.Update(k)
	return m, cmd
}

const credFields = 3

func (m *Model) credEdit(on bool) {
	m.editing = on
	m.user.Blur()
	m.pass.Blur()
	m.domain.Blur()
	if !on {
		return
	}
	switch m.focus {
	case 0:
		m.user.Focus()
	case 1:
		m.pass.Focus()
	case 2:
		m.domain.Focus()
	}
}

func (m Model) updateCreds(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.editing {
		switch k.String() {
		case "esc", "enter":
			m.credEdit(false)
			return m, nil
		}
		var cmd tea.Cmd
		switch m.focus {
		case 0:
			m.user, cmd = m.user.Update(k)
		case 1:
			m.pass, cmd = m.pass.Update(k)
		default:
			m.domain, cmd = m.domain.Update(k)
		}
		return m, cmd
	}
	switch k.String() {
	case "esc":
		m.stage = stCSV
		m.csvPath.Focus()
		return m, textinput.Blink
	case "down", "tab", "j":
		m.focus = (m.focus + 1) % credFields
	case "up", "shift+tab", "k":
		m.focus = (m.focus + credFields - 1) % credFields
	case "enter", "i":
		m.credEdit(true)
		return m, textinput.Blink
	case "v":
		return m.build()
	}
	return m, nil
}

// build runs the clone against a copy of the configuration and moves to the
// preview. Nothing has touched the server at this point.
func (m Model) build() (tea.Model, tea.Cmd) {
	if strings.TrimSpace(m.user.Value()) == "" || m.pass.Value() == "" {
		m.msg = "Windows username and password are required"
		return m, nil
	}
	// Work on a fresh copy so going back and forth never double-clones.
	fresh := deepCopy(m.cfg)
	res, err := cloner.Clone(fresh, m.template, m.rows, cloner.Creds{
		Username: m.user.Value(),
		Password: m.pass.Value(),
		Domain:   strings.TrimSpace(m.domain.Value()),
	})
	if err != nil {
		m.msg = err.Error()
		return m, nil
	}
	m.cfg, m.result = fresh, res
	m.stage, m.msg, m.prevTop = stPreview, "", 0
	return m, nil
}

func (m Model) updatePreview(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "b":
		// Re-fetch so the next build starts from the server's version.
		m.stage, m.busy = stLoadingConfig, true
		return m, m.loadConfig(m.cfgName)
	case "q":
		return m, ext.Leave
	case "up", "k":
		m.prevTop = max(0, m.prevTop-1)
	case "down", "j":
		m.prevTop++
	case "enter":
		m.stage, m.md = stMode, modeDryRun
		m.outPath.SetValue("")
	}
	return m, nil
}

// ------------------------------------------------------------ mode + apply

func (m Model) updateMode(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.editing {
		switch k.String() {
		case "esc", "enter":
			m.editing = false
			m.outPath.Blur()
			m.newName.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		if m.md == modeVerify {
			m.newName, cmd = m.newName.Update(k)
		} else {
			m.outPath, cmd = m.outPath.Update(k)
		}
		return m, cmd
	}
	switch k.String() {
	case "esc", "b":
		m.stage = stPreview
	case "left", "h", "up", "k":
		m.md = mode(max(0, int(m.md)-1))
	case "right", "l", "down", "j":
		m.md = mode(min(int(modeUpdate), int(m.md)+1))
	case "i":
		switch m.md {
		case modeDryRun:
			m.editing = true
			m.outPath.Focus()
			return m, textinput.Blink
		case modeVerify:
			m.editing = true
			m.newName.Focus()
			return m, textinput.Blink
		}
	case "enter":
		if m.md == modeVerify && strings.TrimSpace(m.newName.Value()) == "" {
			m.msg = "verify mode needs a name for the new configuration (press i)"
			return m, nil
		}
		if m.md == modeDryRun {
			// A local file write needs no typed confirmation.
			return m, m.apply()
		}
		m.stage, m.msg = stConfirm, ""
		m.confirm.SetValue("")
		m.confirm.Focus()
		return m, textinput.Blink
	}
	return m, nil
}

func (m Model) updateConfirm(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.confirm.Blur()
		m.stage, m.msg = stMode, ""
		return m, nil
	case "enter":
		if strings.TrimSpace(m.confirm.Value()) != "confirm" {
			m.msg = "type confirm to apply, or esc to go back"
			return m, nil
		}
		m.confirm.Blur()
		m.stage, m.msg = stApplying, ""
		return m, m.apply()
	}
	var cmd tea.Cmd
	m.confirm, cmd = m.confirm.Update(k)
	return m, cmd
}

func (m Model) apply() tea.Cmd {
	cfg, md, client := m.cfg, m.md, m.client
	out, name := strings.TrimSpace(m.outPath.Value()), strings.TrimSpace(m.newName.Value())
	cfgName, added := m.cfgName, len(m.result.Added)

	return func() tea.Msg {
		ctx := context.Background()
		switch md {
		case modeDryRun:
			if out == "" {
				out = cfgName + "-new.yaml"
			}
			data, err := yaml.Marshal(map[string]any(cfg))
			if err != nil {
				return appliedMsg{err: err}
			}
			if err := os.WriteFile(expand(out), data, 0o600); err != nil {
				return appliedMsg{err: err}
			}
			return appliedMsg{out: fmt.Sprintf(
				"Wrote %s with %d new source(s).\n\n%s",
				out, added,
				ui.Warn.Render("The file holds the Windows password in plaintext. It is mode 0600; delete it after use."))}

		case modeVerify:
			cp := deepCopy(cfg)
			meta, _ := cp["metadata"].(map[string]any)
			if meta == nil {
				meta = map[string]any{}
				cp["metadata"] = meta
			}
			meta["name"] = name
			delete(meta, "id")
			delete(meta, "version")
			if _, err := client.Apply(ctx, []bpclient.Resource{cp}); err != nil {
				return appliedMsg{err: err}
			}
			return appliedMsg{out: fmt.Sprintf(
				"Applied as a new configuration %q with %d new source(s).\n\nReview it at %s/configurations/%s",
				name, added, strings.TrimRight(client.BaseURL(), "/"), name)}

		default: // modeUpdate
			cp := deepCopy(cfg)
			if meta, ok := cp["metadata"].(map[string]any); ok {
				delete(meta, "id")
				delete(meta, "version")
			}
			if _, err := client.Apply(ctx, []bpclient.Resource{cp}); err != nil {
				return appliedMsg{err: err}
			}
			return appliedMsg{out: fmt.Sprintf(
				"Updated %q in place with %d new source(s).", cfgName, added)}
		}
	}
}

// expand resolves a leading ~ so pasted paths work.
func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

func deepCopy(r bpclient.Resource) bpclient.Resource {
	return bpclient.Resource(cloner.DeepCopyMap(r))
}
