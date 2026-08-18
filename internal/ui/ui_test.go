package ui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"agents-session-manager/internal/agents"
	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func send(t *testing.T, m tea.Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	mm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T", next)
	}
	return mm
}

func fixtureModel(t *testing.T) Model {
	t.Helper()
	home := t.TempDir()
	m := New([]agents.Agent{agents.NewClaude(home), agents.NewCodex(home), agents.NewGrok(home)}, t.TempDir())
	m = send(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})

	now := time.Now()
	m = send(t, m, sessionsLoadedMsg{sessions: []model.Session{
		{Kind: model.Claude, ID: "c-orphan", Cwd: "/gone/project", Title: "Orphaned claude", Orphan: true, UpdatedAt: now, Messages: 5},
		{Kind: model.Claude, ID: "c-orphan2", Cwd: "/gone/project", Title: "Orphaned claude 2", Orphan: true, UpdatedAt: now.Add(-time.Hour)},
		{Kind: model.Codex, ID: "x-ok", Cwd: home, Title: "Healthy codex", UpdatedAt: now, Messages: 3},
		{Kind: model.Grok, ID: "g-active", Cwd: home, Title: "Active grok", UpdatedAt: now, Active: true},
	}, errs: nil})
	if !m.loaded || len(m.filtered) != 4 {
		t.Fatalf("setup failed: loaded=%v filtered=%d", m.loaded, len(m.filtered))
	}
	return m
}

func TestOrphansFirstAndFilters(t *testing.T) {
	m := fixtureModel(t)

	// Orphans sort first.
	if m.filtered[0].ID != "c-orphan" {
		t.Fatalf("expected orphan first, got %+v", m.filtered[0])
	}

	// Orphans-only toggle.
	m = send(t, m, key("o"))
	if len(m.filtered) != 2 {
		t.Fatalf("orphansOnly: %d sessions", len(m.filtered))
	}
	m = send(t, m, key("o"))

	// Agent filter cycle: All -> claude -> codex -> grok -> All.
	m = send(t, m, key("tab"))
	if len(m.filtered) != 2 {
		t.Fatalf("claude filter: %d sessions", len(m.filtered))
	}
	m = send(t, m, key("tab"))
	if len(m.filtered) != 1 || m.filtered[0].ID != "x-ok" {
		t.Fatalf("codex filter wrong: %+v", m.filtered)
	}
	m = send(t, m, key("shift+tab"))
	if len(m.filtered) != 2 {
		t.Fatalf("shift+tab back to claude: %d sessions", len(m.filtered))
	}
	m = send(t, m, key("tab"))
	m = send(t, m, key("tab"))
	m = send(t, m, key("tab"))
	if len(m.filtered) != 4 {
		t.Fatalf("back to all: %d", len(m.filtered))
	}

	// Text query.
	m = send(t, m, key("/"))
	for _, r := range "act" {
		m = send(t, m, key(string(r)))
	}
	if len(m.filtered) != 1 || m.filtered[0].ID != "g-active" {
		t.Fatalf("query filter wrong: %+v", m.filtered)
	}
	m = send(t, m, key("enter")) // close search
	if m.searching {
		t.Fatal("search should be closed")
	}
}

func TestRemapFlow(t *testing.T) {
	m := fixtureModel(t)
	target := t.TempDir() // exists, so Validate passes

	// Cursor starts on first orphan. Press m to remap the whole project group.
	m = send(t, m, key("m"))
	if m.mode != modeRemapInput {
		t.Fatalf("mode = %v", m.mode)
	}
	if len(m.remapGroup) != 2 {
		t.Fatalf("remap group should cover both /gone/project sessions, got %d", len(m.remapGroup))
	}

	// Type one rune through the input wiring, then set the value directly.
	m = send(t, m, key("/"))
	m.input.SetValue(target)
	m = send(t, m, key("enter"))
	if m.mode != modeRemapPreview {
		t.Fatalf("expected preview, mode=%v errMsg=%q", m.mode, m.errMsg)
	}
	if m.remapPlan == nil || len(m.remapPlan.Actions) == 0 {
		t.Fatal("no plan built")
	}
	if m.remapPlan.OldCwd != "/gone/project" || m.remapPlan.NewCwd != target {
		t.Fatalf("plan cwd wrong: %+v", m.remapPlan)
	}

	// Preview renders the plan.
	view := m.View()
	if !strings.Contains(view, "Migration plan") || !strings.Contains(view, target) {
		t.Fatalf("preview missing from view:\n%s", view)
	}

	// Cancel works.
	m = send(t, m, key("esc"))
	if m.mode != modeList || m.remapPlan != nil {
		t.Fatalf("cancel failed: mode=%v plan=%v", m.mode, m.remapPlan)
	}
}

func TestRemapRejectsSamePath(t *testing.T) {
	m := fixtureModel(t)
	m = send(t, m, key("m"))
	m.input.SetValue("/gone/project")
	m = send(t, m, key("enter"))
	if m.mode != modeRemapInput || m.errMsg == "" {
		t.Fatalf("expected error for same path, mode=%v err=%q", m.mode, m.errMsg)
	}
}

func TestTransferFlow(t *testing.T) {
	m := fixtureModel(t)
	m = send(t, m, key("e"))
	if m.mode != modeTransferPick || m.xferSrc == nil {
		t.Fatalf("export not entered: mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "export") || !strings.Contains(m.View(), "migrate") {
		t.Fatalf("pick view missing options:\n%s", m.View())
	}
	m = send(t, m, key("c"))
	if m.mode != modeTransferTarget || m.xferMode != agents.TransferExport {
		t.Fatalf("target not entered: mode=%v modeStr=%s", m.mode, m.xferMode)
	}
	if len(m.xferTargets) == 0 {
		t.Fatal("no targets")
	}
	m = send(t, m, key("esc"))
	if m.mode != modeTransferPick {
		t.Fatalf("esc should return to pick, mode=%v", m.mode)
	}
	m = send(t, m, key("esc"))
	if m.mode != modeList || m.xferSrc != nil {
		t.Fatal("esc should cancel transfer")
	}
}

func TestRenameFlow(t *testing.T) {
	m := fixtureModel(t)
	m = send(t, m, key("n"))
	if m.mode != modeRenameInput || m.renameTarget == nil {
		t.Fatalf("rename not entered: mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "Rename") {
		t.Fatal("rename prompt missing from view")
	}
	m = send(t, m, key("esc"))
	if m.mode != modeList || m.renameTarget != nil {
		t.Fatal("rename should cancel")
	}
}

func TestRenameRefusedWhenLocked(t *testing.T) {
	m := fixtureModel(t)
	m.locks = []agents.Lock{{Kind: model.Claude, PIDs: []int{1234}, Sources: []string{"process claude pid 1234"}}}
	m = send(t, m, key("n"))
	if m.mode != modeList || m.renameTarget != nil {
		t.Fatalf("rename should be refused, mode=%v target=%v", m.mode, m.renameTarget)
	}
	if m.errMsg == "" || !strings.Contains(m.errMsg, "1234") {
		t.Fatalf("errMsg = %q", m.errMsg)
	}
}

func TestDeleteFlow(t *testing.T) {
	m := fixtureModel(t)
	m = send(t, m, key("x"))
	if m.mode != modeDeleteConfirm || m.deleteTarget == nil {
		t.Fatalf("delete confirm not entered: mode=%v", m.mode)
	}
	// Any key but y cancels.
	m = send(t, m, key("n"))
	if m.mode != modeList || m.deleteTarget != nil {
		t.Fatal("delete should be cancelled")
	}
	// y applies asynchronously.
	m = send(t, m, key("x"))
	m = send(t, m, key("y"))
	if !m.applying {
		t.Fatal("expected applying state")
	}
}

func TestRemapRefusedWhenLocked(t *testing.T) {
	m := fixtureModel(t)
	m.locks = []agents.Lock{{Kind: model.Claude, PIDs: []int{1234}, Sources: []string{"process claude pid 1234"}}}
	m = send(t, m, key("m"))
	if m.mode != modeList {
		t.Fatalf("remap should be refused, mode=%v", m.mode)
	}
	if m.errMsg == "" || !strings.Contains(m.errMsg, "claude") || !strings.Contains(m.errMsg, "1234") {
		t.Fatalf("errMsg = %q", m.errMsg)
	}
}

func TestDeleteRefusedWhenLocked(t *testing.T) {
	m := fixtureModel(t)
	m.locks = []agents.Lock{{Kind: model.Claude, PIDs: []int{9}}}
	m = send(t, m, key("x"))
	if m.mode != modeList || m.deleteTarget != nil {
		t.Fatalf("delete should be refused, mode=%v target=%v", m.mode, m.deleteTarget)
	}
	if m.errMsg == "" {
		t.Fatal("expected lock refusal message")
	}
}

func TestResumeAllowedWhenLocked(t *testing.T) {
	m := fixtureModel(t)
	// Healthy non-orphan is the 3rd row after orphans.
	for m.filtered[m.cursor].ID != "x-ok" {
		m = send(t, m, key("down"))
	}
	m.locks = []agents.Lock{{Kind: model.Codex, PIDs: []int{1}}}
	m = send(t, m, key("r"))
	// Resume is allowed; the only error we accept is missing binary.
	if m.errMsg != "" && !strings.Contains(m.errMsg, "not found") {
		t.Fatalf("resume should not be blocked by the lock: %q", m.errMsg)
	}
	if m.mode != modeList {
		t.Fatalf("mode = %v", m.mode)
	}
}

func TestLockPollTickRefreshes(t *testing.T) {
	m := fixtureModel(t)
	next, cmd := m.Update(lockPollTick{})
	mm := next.(Model)
	if cmd == nil {
		t.Fatal("lock poll should issue a detect command")
	}
	if !mm.detectingLocks {
		t.Fatal("detectingLocks should be set while a detect is in flight")
	}

	mm = send(t, mm, locksMsg{locks: []agents.Lock{{Kind: model.Claude, PIDs: []int{42}}}, arm: true})
	if len(mm.locks) != 1 || mm.locks[0].PIDs[0] != 42 {
		t.Fatalf("locks not applied: %+v", mm.locks)
	}
	if mm.detectingLocks {
		t.Fatal("detectingLocks should clear when the result arrives")
	}
	if !mm.pollArmed {
		t.Fatal("arm=true should schedule the next tick")
	}

	mm = send(t, mm, locksMsg{locks: nil, arm: false})
	if len(mm.locks) != 0 {
		t.Fatalf("locks should clear: %+v", mm.locks)
	}
	mm = send(t, mm, key("tab"))
	if strings.Contains(mm.View(), "LOCKED") {
		t.Fatalf("cleared lock still shown:\n%s", mm.View())
	}
}

func TestViewLockBanner(t *testing.T) {
	m := fixtureModel(t)
	m.locks = []agents.Lock{{Kind: model.Claude, PIDs: []int{1234}}, {Kind: model.Grok, PIDs: []int{99}}}

	// All tab: no long combined banner; locked agent names still appear as tabs.
	v := m.View()
	if strings.Contains(v, "LOCKED") {
		t.Fatalf("All tab should not show a lock banner:\n%s", v)
	}
	if !strings.Contains(v, "claude") || !strings.Contains(v, "grok") {
		t.Fatalf("tabs missing:\n%s", v)
	}

	// Selecting the locked claude tab shows only that tab's PIDs.
	m = send(t, m, key("tab"))
	v = m.View()
	if !strings.Contains(v, "LOCKED") || !strings.Contains(v, "1234") {
		t.Fatalf("claude banner missing:\n%s", v)
	}
	if strings.Contains(v, "99") {
		t.Fatalf("claude tab should not list grok PIDs:\n%s", v)
	}

	// Codex tab is unlocked: no banner.
	m = send(t, m, key("tab"))
	v = m.View()
	if strings.Contains(v, "LOCKED") {
		t.Fatalf("unlocked tab should not show a lock banner:\n%s", v)
	}

	// Grok tab: only grok PID.
	m = send(t, m, key("tab"))
	v = m.View()
	if !strings.Contains(v, "LOCKED") || !strings.Contains(v, "99") {
		t.Fatalf("grok banner missing:\n%s", v)
	}
	if strings.Contains(v, "1234") {
		t.Fatalf("grok tab should not list claude PIDs:\n%s", v)
	}
}

func TestAddStoreRequiresAgentTab(t *testing.T) {
	m := fixtureModel(t)
	m = send(t, m, key("a"))
	if m.mode != modeList || m.errMsg == "" {
		t.Fatalf("All tab should refuse add-dir: mode=%v err=%q", m.mode, m.errMsg)
	}
}

func TestAddClaudeDirFlow(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "asm.json")
	t.Setenv("ASM_CONFIG", cfg)
	m := fixtureModel(t)
	dir := t.TempDir()
	m = send(t, m, key("tab")) // claude
	m = send(t, m, key("a"))
	if m.mode != modeAddStore {
		t.Fatalf("mode=%v err=%q", m.mode, m.errMsg)
	}
	m.input.SetValue(dir)
	m = send(t, m, key("enter"))
	if m.mode != modeList {
		t.Fatalf("mode=%v err=%q", m.mode, m.errMsg)
	}
	if !strings.Contains(m.status, "added") {
		t.Fatalf("status=%q", m.status)
	}
}

func TestResumeRefusesOrphan(t *testing.T) {
	m := fixtureModel(t)
	m = send(t, m, key("r"))
	if m.errMsg == "" {
		t.Fatal("expected error resuming an orphaned session")
	}
}

func TestApplyResultRefreshes(t *testing.T) {
	m := fixtureModel(t)
	m.mode = modeRemapPreview
	m.applying = true
	plan := &migrate.Plan{Agent: "claude", OldCwd: "/gone/project", NewCwd: "/somewhere/new"}
	rep := &migrate.Report{BackupDir: "/tmp/backup"}
	m = send(t, m, applyResultMsg{plan: plan, report: rep})
	if m.mode != modeList || m.applying {
		t.Fatalf("state after apply: mode=%v applying=%v", m.mode, m.applying)
	}
	if !strings.Contains(m.status, "remapped") {
		t.Fatalf("status = %q", m.status)
	}

	m = fixtureModel(t)
	m.applying = true
	m = send(t, m, applyResultMsg{
		plan:   &migrate.Plan{Agent: "claude", NewTitle: "New name"},
		report: &migrate.Report{BackupDir: "/tmp/backup"},
	})
	if !strings.Contains(m.status, "renamed") || !strings.Contains(m.status, "New name") {
		t.Fatalf("rename status = %q", m.status)
	}

	m = fixtureModel(t)
	m.applying = true
	m = send(t, m, applyResultMsg{
		plan:   &migrate.Plan{Agent: "claude", NewTitle: "X"},
		report: &migrate.Report{BackupDir: "/tmp/backup", Warnings: []string{"potential corruption: foo"}},
	})
	if !strings.Contains(m.status, "WARNING") || !strings.Contains(m.status, "potential corruption") {
		t.Fatalf("warning status = %q", m.status)
	}
}

func TestFooterWrapsInsteadOfEllipsis(t *testing.T) {
	m := fixtureModel(t)
	m = send(t, m, tea.WindowSizeMsg{Width: 48, Height: 24})
	got := wrapGuide(m.footerKeys(), 48)
	if strings.Contains(got, "…") {
		t.Fatalf("footer truncated:\n%s", got)
	}
	if !strings.Contains(got, "q quit") || !strings.Contains(got, "R refresh") || !strings.Contains(got, "n rename") {
		t.Fatalf("footer missing keys:\n%s", got)
	}
	if !strings.Contains(got, "\n") {
		t.Fatalf("expected wrapped footer at width 48:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if lipgloss.Width(line) > 48 {
			t.Fatalf("footer line wider than terminal (%d): %q", lipgloss.Width(line), line)
		}
	}
	v := m.View()
	if !strings.Contains(v, "q quit") {
		t.Fatalf("full view dropped wrapped footer:\n%s", v)
	}
	if m.footerLineCount() < 2 {
		t.Fatalf("footerLineCount = %d", m.footerLineCount())
	}
}

func TestViewRendersAllModes(t *testing.T) {
	m := fixtureModel(t)
	for _, tc := range []struct {
		name string
		mut  func(Model) Model
	}{
		{"list", func(m Model) Model { return m }},
		{"detail", func(m Model) Model { m.detail = true; return m }},
		{"narrow", func(m Model) Model {
			mm := send(t, m, tea.WindowSizeMsg{Width: 60, Height: 20})
			return mm
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mm := tc.mut(m)
			v := mm.View()
			if v == "" || v == "…" {
				t.Fatalf("empty view")
			}
			if !strings.Contains(v, "agents-session-manager") {
				t.Fatalf("header missing:\n%s", v)
			}
		})
	}
}
