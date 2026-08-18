// Package ui implements the Bubble Tea front end.
package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"agents-session-manager/internal/agents"
	"agents-session-manager/internal/config"
	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

type mode int

const (
	modeList mode = iota
	modeRemapInput
	modeRemapPreview
	modeDeleteConfirm
	modeAddStore
	modeRenameInput
)

type sessionsLoadedMsg struct {
	sessions []model.Session
	errs     map[model.Kind]error
}

type applyResultMsg struct {
	plan   *migrate.Plan
	report *migrate.Report
	err    error
}

type resumeFinishedMsg struct {
	err error
}

type locksMsg struct {
	locks []agents.Lock
	arm   bool // schedule the next poll tick
}

type lockPollTick struct{}

const (
	lockPollInterval = time.Second
	lockStaleAfter   = 3 * time.Second
)

type sessionKey struct {
	kind  model.Kind
	id    string
	store string
}

// Model is the root Bubble Tea model.
type Model struct {
	agents     []agents.Agent
	backupRoot string
	extraDirs  map[model.Kind][]string
	addKind    model.Kind // kind for modeAddStore

	all      []model.Session
	filtered []model.Session
	scanErrs map[model.Kind]error
	scanning bool
	loaded   bool
	focusKey *sessionKey // session to re-select after a rescan

	cursor int
	offset int

	query     string
	input     textinput.Model
	searching bool

	agentFilter int // 0 = all, n = agents[n-1]
	orphansOnly bool
	detail      bool

	mode         mode
	remapGroup   []model.Session
	remapPlan    *migrate.Plan
	deleteTarget *model.Session
	renameTarget *model.Session

	applying bool
	spinner  spinner.Model

	status string
	errMsg string

	locks          []agents.Lock
	lastLocksAt    time.Time
	detectingLocks bool
	pollArmed      bool

	width, height int
}

// New builds the root model for the given discovered agents.
func New(as []agents.Agent, backupRoot string) Model {
	ti := textinput.New()
	ti.CharLimit = 512
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	return Model{
		agents:      as,
		backupRoot:  backupRoot,
		input:       ti,
		spinner:     sp,
		scanning:    true,
		lastLocksAt: time.Now(),
	}
}

// WithExtraDirs records --<agent>-dir flags so a TUI add can rediscover.
func (m Model) WithExtraDirs(dirs map[model.Kind][]string) Model {
	m.extraDirs = dirs
	return m
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.scanCmd(), m.spinner.Tick, detectLocksCmd(m.agents, true))
}

func detectLocksCmd(as []agents.Agent, arm bool) tea.Cmd {
	return func() tea.Msg {
		return locksMsg{locks: agents.DetectLocks(as), arm: arm}
	}
}

func pollLocksLater() tea.Cmd {
	return tea.Tick(lockPollInterval, func(time.Time) tea.Msg { return lockPollTick{} })
}

func (m Model) scanCmd() tea.Cmd {
	as := m.agents
	return func() tea.Msg {
		ss, errs := agents.ScanAll(context.Background(), as)
		return sessionsLoadedMsg{sessions: ss, errs: errs}
	}
}

func applyCmd(plan *migrate.Plan, backupRoot string, as []agents.Agent) tea.Cmd {
	var watch []agents.Agent
	for _, a := range as {
		if string(a.Kind()) == plan.Agent {
			watch = append(watch, a)
		}
	}
	if len(watch) == 0 {
		watch = as
	}
	return func() tea.Msg {
		rep, err := migrate.ApplyWith(plan, backupRoot, func() error {
			return agents.ProbeUnlocked(watch)
		})
		return applyResultMsg{plan: plan, report: rep, err: err}
	}
}

func (m *Model) selected() *model.Session {
	if len(m.filtered) == 0 || m.cursor < 0 || m.cursor >= len(m.filtered) {
		return nil
	}
	return &m.filtered[m.cursor]
}

func (m *Model) applyFilters() {
	var wantKind model.Kind
	var wantStore string
	if m.agentFilter > 0 && m.agentFilter <= len(m.agents) {
		a := m.agents[m.agentFilter-1]
		wantKind = a.Kind()
		wantStore = a.Root()
	}
	q := strings.ToLower(strings.TrimSpace(m.query))
	out := m.filtered[:0]
	for _, s := range m.all {
		if wantKind != "" {
			if s.Kind != wantKind {
				continue
			}
			if wantStore != "" && s.Store != "" && !agents.SamePath(s.Store, wantStore) {
				continue
			}
		}
		if m.orphansOnly && !s.Orphan {
			continue
		}
		if q != "" {
			hay := strings.ToLower(s.Title + " " + s.ID + " " + s.Cwd + " " + s.Model + " " + s.Store)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		out = append(out, s)
	}
	m.filtered = out
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.offset = 0
	m.ensureVisible()
}

func (m Model) groupFor(s model.Session) []model.Session {
	var g []model.Session
	for _, x := range m.all {
		if x.Kind == s.Kind && x.Cwd == s.Cwd && (s.Store == "" || x.Store == "" || agents.SamePath(x.Store, s.Store)) {
			g = append(g, x)
		}
	}
	return g
}

func (m *Model) move(delta int) {
	if len(m.filtered) == 0 {
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	m.ensureVisible()
}

// ensureVisible keeps the cursor row inside the scroll window. Call it from
// Update (View receives the model by value, so scrolling state must not be
// computed there).
func (m *Model) ensureVisible() {
	page := m.pageSize()
	if page <= 0 {
		return
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+page {
		m.offset = m.cursor - page + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

// Update handles all messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	mm, ok := next.(Model)
	if !ok {
		return next, cmd
	}
	var extra tea.Cmd
	mm, extra = mm.ensureLockPoll()
	return mm, tea.Batch(cmd, extra)
}

func (m Model) ensureLockPoll() (Model, tea.Cmd) {
	if m.pollArmed || m.detectingLocks {
		return m, nil
	}
	if !m.lastLocksAt.IsZero() && time.Since(m.lastLocksAt) < lockStaleAfter {
		return m, nil
	}
	m.pollArmed = true
	return m, detectLocksCmd(m.agents, true)
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case sessionsLoadedMsg:
		m.scanning = false
		m.loaded = true
		m.all = msg.sessions
		m.scanErrs = msg.errs
		m.applyFilters()
		if m.focusKey != nil {
			for i := range m.filtered {
				if m.filtered[i].Kind == m.focusKey.kind && m.filtered[i].ID == m.focusKey.id &&
					(m.focusKey.store == "" || m.filtered[i].Store == m.focusKey.store) {
					m.cursor = i
					break
				}
			}
			m.focusKey = nil
			m.ensureVisible()
		}
		return m, detectLocksCmd(m.agents, false)

	case applyResultMsg:
		m.applying = false
		if msg.err != nil {
			m.errMsg = msg.err.Error()
			return m, detectLocksCmd(m.agents, false)
		}
		if msg.plan.NewTitle != "" {
			m.status = fmt.Sprintf("✓ renamed to %q · backup: %s", msg.plan.NewTitle, msg.report.BackupDir)
		} else if msg.plan.NewCwd != "" {
			n := 0
			for _, s := range m.all {
				if s.Kind == model.Kind(msg.plan.Agent) && s.Cwd == msg.plan.OldCwd {
					n++
				}
			}
			if n == 0 {
				n = len(m.remapGroup)
			}
			m.status = fmt.Sprintf("✓ remapped %d session(s) %s → %s · backup: %s",
				n, msg.plan.OldCwd, msg.plan.NewCwd, msg.report.BackupDir)
		} else {
			m.status = "✓ session deleted · archived to " + msg.report.BackupDir
		}
		if msg.report != nil && len(msg.report.Warnings) > 0 {
			m.status += " · WARNING: " + strings.Join(msg.report.Warnings, "; ")
		}
		m.mode = modeList
		m.remapPlan = nil
		m.remapGroup = nil
		m.deleteTarget = nil
		m.renameTarget = nil
		if s := m.selected(); s != nil {
			m.focusKey = &sessionKey{s.Kind, s.ID, s.Store}
		}
		return m, tea.Batch(m.scanCmd(), detectLocksCmd(m.agents, false))

	case resumeFinishedMsg:
		// Returned from a resumed agent process.
		if msg.err != nil {
			m.errMsg = "resume exited with error: " + msg.err.Error()
			return m, detectLocksCmd(m.agents, false)
		}
		m.status = ""
		return m, tea.Batch(m.scanCmd(), detectLocksCmd(m.agents, false))

	case locksMsg:
		m.locks = msg.locks
		m.lastLocksAt = time.Now()
		m.detectingLocks = false
		if msg.arm {
			m.pollArmed = true
			return m, pollLocksLater()
		}
		return m, nil

	case lockPollTick:
		m.pollArmed = false
		if m.detectingLocks {
			m.pollArmed = true
			return m, pollLocksLater()
		}
		m.detectingLocks = true
		return m, detectLocksCmd(m.agents, true)

	case spinner.TickMsg:
		if m.applying {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	if m.applying {
		return m, nil
	}

	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

	switch m.mode {
	case modeRemapInput:
		return m.updateRemapInput(key)
	case modeRemapPreview:
		return m.updateRemapPreview(key)
	case modeDeleteConfirm:
		return m.updateDeleteConfirm(key)
	case modeAddStore:
		return m.updateAddStore(key)
	case modeRenameInput:
		return m.updateRenameInput(key)
	default:
		return m.updateList(key)
	}
}

func (m Model) updateList(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.searching {
		switch key.String() {
		case "enter":
			m.searching = false
			m.input.Blur()
			return m, nil
		case "esc":
			m.searching = false
			m.input.Blur()
			m.query = ""
			m.input.SetValue("")
			m.applyFilters()
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(key)
		m.query = m.input.Value()
		m.applyFilters()
		m.cursor = 0
		return m, cmd
	}

	switch key.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "esc":
		if m.detail {
			m.detail = false
			return m, nil
		}
		if m.errMsg != "" || m.status != "" {
			m.errMsg, m.status = "", ""
			return m, nil
		}
		return m, tea.Quit
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup":
		m.move(-(m.pageSize() - 1))
	case "pgdown":
		m.move(m.pageSize() - 1)
	case "g", "home":
		m.cursor = 0
		m.ensureVisible()
	case "G", "end":
		if len(m.filtered) > 0 {
			m.cursor = len(m.filtered) - 1
		}
		m.ensureVisible()
	case "/":
		m.searching = true
		m.input.SetValue(m.query)
		m.input.Focus()
		return m, m.input.Cursor.BlinkCmd()
	case "tab":
		m.agentFilter = (m.agentFilter + 1) % (len(m.agents) + 1)
		m.applyFilters()
		m.cursor = 0
	case "shift+tab":
		n := len(m.agents) + 1
		m.agentFilter = (m.agentFilter - 1 + n) % n
		m.applyFilters()
		m.cursor = 0
	case "o":
		m.orphansOnly = !m.orphansOnly
		m.applyFilters()
		m.cursor = 0
	case "enter", "d":
		if m.selected() != nil {
			m.detail = !m.detail
		}
	case "m":
		return m.startRemap()
	case "x":
		return m.startDelete()
	case "n":
		return m.startRename()
	case "r":
		return m.startResume()
	case "a":
		return m.startAddStore()
	case "L":
		return m, detectLocksCmd(m.agents, false)
	case "R":
		m.scanning = true
		m.status, m.errMsg = "", ""
		if s := m.selected(); s != nil {
			m.focusKey = &sessionKey{s.Kind, s.ID, s.Store}
		}
		return m, tea.Batch(m.scanCmd(), detectLocksCmd(m.agents, false))
	case "?":
		m.detail = !m.detail
	}
	return m, nil
}

func (m Model) startRemap() (tea.Model, tea.Cmd) {
	s := m.selected()
	if s == nil {
		return m, nil
	}
	if msg := m.lockRefuse(*s, "remap"); msg != "" {
		m.errMsg = msg
		return m, nil
	}
	group := m.groupFor(*s)
	if len(group) == 0 {
		return m, nil
	}
	m.mode = modeRemapInput
	m.remapGroup = group
	m.errMsg = ""
	m.input.SetValue("")
	m.input.Placeholder = "/new/path/to/project"
	m.input.Focus()
	return m, m.input.Cursor.BlinkCmd()
}

func (m Model) updateRemapInput(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc":
		m.mode = modeList
		m.remapGroup = nil
		m.input.Blur()
		return m, nil
	case "enter":
		raw := strings.TrimSpace(m.input.Value())
		if raw == "" {
			m.mode = modeList
			m.remapGroup = nil
			m.input.Blur()
			return m, nil
		}
		abs, err := filepath.Abs(raw)
		if err != nil {
			m.errMsg = "invalid path: " + err.Error()
			return m, nil
		}
		abs = filepath.Clean(abs)
		oldCwd := m.remapGroup[0].Cwd
		if abs == filepath.Clean(oldCwd) {
			m.errMsg = "new path is the same as the old one"
			return m, nil
		}
		agent, ok := m.agentFor(m.remapGroup[0])
		if !ok {
			m.errMsg = "no agent adapter for " + string(m.remapGroup[0].Kind)
			return m, nil
		}
		plan, err := agent.RemapPlan(m.remapGroup, abs)
		if err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		if err := plan.Validate(); err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.remapPlan = plan
		m.mode = modeRemapPreview
		m.input.Blur()
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(key)
	return m, cmd
}

func (m Model) updateRemapPreview(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "n", "q":
		m.mode = modeList
		m.remapPlan = nil
		m.remapGroup = nil
		return m, nil
	case "y", "enter":
		m.applying = true
		m.errMsg = ""
		plan := m.remapPlan
		return m, tea.Batch(applyCmd(plan, m.backupRoot, m.agents), m.spinner.Tick)
	}
	return m, nil
}

func (m Model) startDelete() (tea.Model, tea.Cmd) {
	s := m.selected()
	if s == nil {
		return m, nil
	}
	if msg := m.lockRefuse(*s, "delete"); msg != "" {
		m.errMsg = msg
		return m, nil
	}
	cp := *s
	m.deleteTarget = &cp
	m.mode = modeDeleteConfirm
	m.errMsg = ""
	return m, nil
}

func (m Model) updateDeleteConfirm(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "y":
		s := *m.deleteTarget
		agent, ok := m.agentFor(s)
		if !ok {
			m.errMsg = "no agent adapter for " + string(s.Kind)
			m.mode = modeList
			return m, nil
		}
		plan, err := agent.DeletePlan([]model.Session{s})
		if err != nil {
			m.errMsg = err.Error()
			m.mode = modeList
			return m, nil
		}
		m.applying = true
		return m, tea.Batch(applyCmd(plan, m.backupRoot, m.agents), m.spinner.Tick)
	default:
		m.mode = modeList
		m.deleteTarget = nil
		return m, nil
	}
}

func (m Model) startResume() (tea.Model, tea.Cmd) {
	s := m.selected()
	if s == nil {
		return m, nil
	}
	if s.Orphan {
		m.errMsg = "session is orphaned — remap it first (m)"
		return m, nil
	}
	agent, ok := m.agentFor(*s)
	if !ok {
		m.errMsg = "no agent adapter for " + string(s.Kind)
		return m, nil
	}
	argv, dir := agent.ResumeCmd(*s)
	if _, err := exec.LookPath(argv[0]); err != nil {
		m.errMsg = fmt.Sprintf("%s binary not found in PATH", argv[0])
		return m, nil
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	if e, ok := agent.(interface{ ResumeEnv() []string }); ok {
		cmd.Env = append(cmd.Env, e.ResumeEnv()...)
	}
	m.status = ""
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return resumeFinishedMsg{err: err}
	})
}

func (m Model) agentFor(s model.Session) (agents.Agent, bool) {
	var kindMatch agents.Agent
	n := 0
	for _, a := range m.agents {
		if a.Kind() != s.Kind {
			continue
		}
		n++
		kindMatch = a
		if s.Store != "" && agents.SamePath(a.Root(), s.Store) {
			return a, true
		}
	}
	if n == 1 {
		return kindMatch, true
	}
	return nil, false
}

func (m Model) sessionLocked(s model.Session) bool {
	_, ok := m.lockOfSession(s)
	return ok
}

func (m Model) lockForAgent(a agents.Agent) (agents.Lock, bool) {
	for _, l := range m.locks {
		if l.Kind != a.Kind() {
			continue
		}
		if l.Root != "" && a.Root() != "" && !agents.SamePath(l.Root, a.Root()) {
			continue
		}
		return l, true
	}
	return agents.Lock{}, false
}

// tabLocked reports whether filter-bar index i (0 = All) is a locked agent.
func (m Model) tabLocked(i int) bool {
	if i <= 0 || i > len(m.agents) {
		return false
	}
	_, ok := m.lockForAgent(m.agents[i-1])
	return ok
}

func (m Model) selectedTabLock() (agents.Lock, bool) {
	if m.agentFilter <= 0 || m.agentFilter > len(m.agents) {
		return agents.Lock{}, false
	}
	return m.lockForAgent(m.agents[m.agentFilter-1])
}

func (m Model) lockOfSession(s model.Session) (agents.Lock, bool) {
	for _, l := range m.locks {
		if l.Kind != s.Kind {
			continue
		}
		if s.Store != "" && l.Root != "" && !agents.SamePath(l.Root, s.Store) {
			continue
		}
		return l, true
	}
	return agents.Lock{}, false
}

func (m Model) lockRefuse(s model.Session, verb string) string {
	l, ok := m.lockOfSession(s)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s; cannot %s its files while it can write them",
		agents.FormatLocks([]agents.Lock{l}), verb)
}

func (m Model) startRename() (tea.Model, tea.Cmd) {
	s := m.selected()
	if s == nil {
		return m, nil
	}
	if msg := m.lockRefuse(*s, "rename"); msg != "" {
		m.errMsg = msg
		return m, nil
	}
	cp := *s
	m.renameTarget = &cp
	m.mode = modeRenameInput
	m.errMsg = ""
	m.input.SetValue(s.Title)
	m.input.Placeholder = "new session name"
	m.input.Focus()
	return m, m.input.Cursor.BlinkCmd()
}

func (m Model) updateRenameInput(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc":
		m.mode = modeList
		m.renameTarget = nil
		m.input.Blur()
		return m, nil
	case "enter":
		raw := strings.TrimSpace(m.input.Value())
		s := *m.renameTarget
		m.input.Blur()
		agent, ok := m.agentFor(s)
		if !ok {
			m.errMsg = "no agent adapter for " + string(s.Kind)
			m.mode = modeList
			m.renameTarget = nil
			return m, nil
		}
		plan, err := agent.RenamePlan(s, raw)
		if err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.mode = modeList
		m.applying = true
		return m, tea.Batch(applyCmd(plan, m.backupRoot, m.agents), m.spinner.Tick)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(key)
	return m, cmd
}

func (m Model) startAddStore() (tea.Model, tea.Cmd) {
	if m.agentFilter <= 0 || m.agentFilter > len(m.agents) {
		m.errMsg = "select an agent tab first — extra homes are per agent"
		return m, nil
	}
	k := m.agents[m.agentFilter-1].Kind()
	if !agents.SupportsExtra(k) {
		m.errMsg = string(k) + " has no relocatable home"
		return m, nil
	}
	m.addKind = k
	m.mode = modeAddStore
	m.errMsg = ""
	m.input.SetValue("")
	m.input.Placeholder = "/path/to/custom/" + string(k) + "-home"
	m.input.Focus()
	return m, m.input.Cursor.BlinkCmd()
}

func (m Model) updateAddStore(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc":
		m.mode = modeList
		m.input.Blur()
		return m, nil
	case "enter":
		raw := strings.TrimSpace(m.input.Value())
		m.input.Blur()
		m.mode = modeList
		if raw == "" {
			return m, nil
		}
		if _, err := config.AddDir(string(m.addKind), raw); err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.agents = agents.DiscoverWith(agents.DiscoverOptions{Extra: m.extraDirs})
		m.status = fmt.Sprintf("added extra %s home %s", m.addKind, raw)
		m.scanning = true
		return m, m.scanCmd()
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(key)
	return m, cmd
}
