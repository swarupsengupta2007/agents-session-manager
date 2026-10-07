package ui

import (
	"os"
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
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
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

	if !strings.Contains(m.View(), "Remap project path") || !strings.Contains(m.View(), "agents-session-manager") {
		t.Fatalf("directory picker overlay missing:\n%s", m.View())
	}

	// "/" edits a path. Set the value directly, then confirm.
	m = send(t, m, key("/"))
	if !m.picker.editing {
		t.Fatal("expected path editing")
	}
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
	m = send(t, m, key("/"))
	m.input.SetValue("/gone/project")
	m = send(t, m, key("enter"))
	if m.mode != modeRemapInput || m.errMsg == "" {
		t.Fatalf("expected error for same path, mode=%v err=%q", m.mode, m.errMsg)
	}
	if !strings.Contains(m.View(), "same as the old one") {
		t.Fatalf("error not shown:\n%s", m.View())
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
	if !strings.Contains(m.View(), "Add extra claude home") {
		t.Fatalf("directory picker missing:\n%s", m.View())
	}
	m = send(t, m, key("/"))
	m.input.SetValue(dir)
	m = send(t, m, key("enter"))

	// Choosing only asks; nothing is saved before y.
	if m.mode != modeAddStore || m.picker.confirm != dir {
		t.Fatalf("expected confirm, mode=%v confirm=%q err=%q", m.mode, m.picker.confirm, m.errMsg)
	}
	view := m.View()
	if !strings.Contains(view, "as an extra claude home?") || !strings.Contains(view, "may not be a claude home") {
		t.Fatalf("confirm or warning missing:\n%s", view)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatalf("config written before confirm: %v", err)
	}
	m = send(t, m, key("esc"))
	if m.mode != modeAddStore || m.picker.confirm != "" {
		t.Fatalf("esc should go back to the picker, mode=%v confirm=%q", m.mode, m.picker.confirm)
	}

	m = send(t, m, key("/"))
	m.input.SetValue(dir)
	m = send(t, m, key("enter"))
	m = send(t, m, key("y"))
	if m.mode != modeList {
		t.Fatalf("mode=%v err=%q", m.mode, m.errMsg)
	}
	if !strings.Contains(m.status, "added") {
		t.Fatalf("status=%q", m.status)
	}
	b, err := os.ReadFile(cfg)
	if err != nil || !strings.Contains(string(b), dir) {
		t.Fatalf("config missing %s: %s %v", dir, b, err)
	}

	// The same dir again is refused before the confirm step.
	m.mode = modeList
	m = send(t, m, key("a"))
	m = send(t, m, key("/"))
	m.input.SetValue(dir)
	m = send(t, m, key("enter"))
	if m.picker.confirm != "" || !strings.Contains(m.errMsg, "already a claude home") {
		t.Fatalf("duplicate home not refused, confirm=%q err=%q", m.picker.confirm, m.errMsg)
	}
}

func TestAddStoreConfirmNoWarningForRealHome(t *testing.T) {
	t.Setenv("ASM_CONFIG", filepath.Join(t.TempDir(), "asm.json"))
	m := fixtureModel(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	m = send(t, m, key("tab")) // claude
	m = send(t, m, key("a"))
	m = send(t, m, key("/"))
	m.input.SetValue(dir)
	m = send(t, m, key("enter"))
	if m.picker.confirm != dir || m.picker.note != "" {
		t.Fatalf("confirm=%q note=%q err=%q", m.picker.confirm, m.picker.note, m.errMsg)
	}
	if !strings.Contains(m.footerKeys(), "y add") {
		t.Fatalf("footer=%q", m.footerKeys())
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

func TestDirPickerBrowseFilterAndSelect(t *testing.T) {
	root := t.TempDir()
	pick := filepath.Join(root, "pickme")
	skip := filepath.Join(root, "skipme")
	if err := os.Mkdir(pick, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(skip, 0o755); err != nil {
		t.Fatal(err)
	}

	m := fixtureModel(t)
	m = send(t, m, key("m"))
	view := m.View()
	if strings.Index(view, "agents-session-manager") < 0 || strings.Index(view, "agents-session-manager") > strings.Index(view, "Remap project path") {
		t.Fatalf("picker should overlay under the header:\n%s", view)
	}
	if !strings.Contains(view, "/gone/project") {
		t.Fatalf("remap source missing from picker:\n%s", view)
	}

	m.picker.cwd = root
	m.loadPickerDir()
	view = m.View()
	if !strings.Contains(view, "pickme") || !strings.Contains(view, "skipme") {
		t.Fatalf("directories missing:\n%s", view)
	}

	m = send(t, m, key("p"))
	m = send(t, m, key("i"))
	view = m.View()
	if !strings.Contains(view, "pickme") || strings.Contains(view, "skipme") {
		t.Fatalf("filter failed:\n%s", view)
	}
	if m.picker.cursor >= len(m.picker.entries) || m.picker.entries[m.picker.cursor] != "pickme" {
		t.Fatalf("cursor %+v entries %+v", m.picker.cursor, m.picker.entries)
	}

	// esc clears the filter before it cancels the picker.
	m = send(t, m, key("esc"))
	if m.mode != modeRemapInput || m.picker.filter != "" {
		t.Fatalf("filter clear: mode=%v filter=%q", m.mode, m.picker.filter)
	}
	if !strings.Contains(m.View(), "skipme") {
		t.Fatal("cleared filter should show every directory")
	}

	m = send(t, m, key("p"))
	m = send(t, m, key("i"))
	m = send(t, m, key(" "))
	if m.mode != modeRemapPreview {
		t.Fatalf("space select: mode=%v err=%q", m.mode, m.errMsg)
	}
	if m.remapPlan == nil || m.remapPlan.NewCwd != pick {
		t.Fatalf("plan %+v", m.remapPlan)
	}
}

func TestDirPickerOpenAndUseThisDir(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	m := fixtureModel(t)
	m = send(t, m, key("m"))
	m.picker.cwd = root
	m.loadPickerDir()

	moved := false
	for i := 0; i < len(m.picker.entries)+1; i++ {
		if m.picker.entries[m.picker.cursor] == "sub" {
			moved = true
			break
		}
		m = send(t, m, key("down"))
	}
	if !moved {
		t.Fatalf("sub not in %+v", m.picker.entries)
	}
	m = send(t, m, key("enter"))
	if m.picker.cwd != sub {
		t.Fatalf("enter should open sub, cwd=%s", m.picker.cwd)
	}
	m = send(t, m, key("left"))
	if m.picker.cwd != root {
		t.Fatalf("left should go up, cwd=%s", m.picker.cwd)
	}

	m.picker.cwd = sub
	m.loadPickerDir()
	if m.picker.entries[0] != "." {
		t.Fatalf("expected a . row first: %+v", m.picker.entries)
	}
	m = send(t, m, key("home"))
	m = send(t, m, key("enter"))
	if m.mode != modeRemapPreview || m.remapPlan == nil || m.remapPlan.NewCwd != sub {
		t.Fatalf(". row should use this dir, mode=%v plan=%+v err=%q", m.mode, m.remapPlan, m.errMsg)
	}
}

func TestDirPickerPathEditEscape(t *testing.T) {
	m := fixtureModel(t)
	m = send(t, m, key("m"))
	m = send(t, m, key("/"))
	if !m.picker.editing || !strings.Contains(m.View(), "path:") {
		t.Fatalf("path edit missing, editing=%v\n%s", m.picker.editing, m.View())
	}
	m = send(t, m, key("esc"))
	if m.mode != modeRemapInput || m.picker.editing {
		t.Fatalf("esc should return to the list, mode=%v editing=%v", m.mode, m.picker.editing)
	}
	m = send(t, m, key("esc"))
	if m.mode != modeList || m.remapGroup != nil {
		t.Fatalf("second esc should cancel, mode=%v", m.mode)
	}
}

func TestDirPickerFitsWidth(t *testing.T) {
	m := fixtureModel(t)
	m = send(t, m, tea.WindowSizeMsg{Width: 48, Height: 20})
	m = send(t, m, key("m"))
	m = send(t, m, key("/"))
	modal := m.renderDirPicker()
	for _, line := range strings.Split(modal, "\n") {
		if w := lipgloss.Width(line); w > 48 {
			t.Fatalf("picker line wider than terminal (%d): %q", w, line)
		}
	}
	if !strings.Contains(m.View(), "use path") || !strings.Contains(m.View(), "agents-session-manager") {
		t.Fatalf("narrow picker clipped the hint or header:\n%s", m.View())
	}
}

func TestPickerStartAndReadSubdirs(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linkdir")
	if err := os.Symlink(sub, link); err != nil {
		t.Fatal(err)
	}
	if pickerStart(sub) != root {
		t.Fatalf("pickerStart(existing) = %s", pickerStart(sub))
	}
	missing := filepath.Join(root, "nope", "child")
	if nearestExistingDir(missing) != root {
		t.Fatalf("nearest = %s", nearestExistingDir(missing))
	}
	names, err := readSubdirs(root)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names, ",")
	if got != "..,linkdir,sub" {
		t.Fatalf("names = %s", got)
	}

	// A path whose entire prefix is gone should not dump the user at `/`.
	orphan := pickerStart("/no/such/agents-session-manager-path")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if orphan != home {
		t.Fatalf("orphan start = %s, want home %s", orphan, home)
	}
}

func TestDirPickerLettersAlwaysFilter(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"my-project", "jk-tools", "other", "yarn"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := fixtureModel(t)
	m = send(t, m, key("m"))
	m.picker.cwd = root
	m.loadPickerDir()

	// Former shortcut letters start and extend a filter like any other.
	for _, tc := range []struct{ typed, want string }{
		{"ya", "yarn"},
		{"jk", "jk-tools"},
		{"my", "my-project"},
		{"h", "other"},
	} {
		for _, r := range tc.typed {
			m = send(t, m, key(string(r)))
		}
		if m.mode != modeRemapInput || m.picker.filter != tc.typed || m.picker.cwd != root {
			t.Fatalf("%q: mode=%v filter=%q cwd=%s", tc.typed, m.mode, m.picker.filter, m.picker.cwd)
		}
		if got := m.picker.entries[m.picker.cursor]; got != tc.want {
			t.Fatalf("%q: cursor on %q, want %q", tc.typed, got, tc.want)
		}
		m = send(t, m, key("esc"))
	}
	if m.mode != modeRemapInput {
		t.Fatalf("esc should only clear the filter, mode=%v", m.mode)
	}
}

func TestResolvePickerPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	for _, tc := range []struct{ raw, want string }{
		{"~", home},
		{"~/proj/x", filepath.Join(home, "proj", "x")},
		{"rel/dir", filepath.Join(base, "rel", "dir")},
		{"/abs/../abs/p", "/abs/p"},
	} {
		got, err := resolvePickerPath(tc.raw, base)
		if err != nil || got != tc.want {
			t.Fatalf("%q: got %q err=%v, want %q", tc.raw, got, err, tc.want)
		}
	}
	if _, err := resolvePickerPath("~bob/x", base); err == nil {
		t.Fatal("~user should be rejected")
	}
}

func TestDirPickerTildePathEdit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, "newproj")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	m := fixtureModel(t)
	m = send(t, m, key("m"))
	m = send(t, m, key("~"))
	if !m.picker.editing || m.input.Value() != "~" {
		t.Fatalf("~ should start a path, editing=%v value=%q", m.picker.editing, m.input.Value())
	}
	m.input.SetValue("~/newproj")
	m = send(t, m, key("enter"))
	if m.mode != modeRemapPreview || m.remapPlan == nil || m.remapPlan.NewCwd != target {
		t.Fatalf("~ not expanded, mode=%v plan=%+v err=%q", m.mode, m.remapPlan, m.errMsg)
	}

	m = fixtureModel(t)
	m = send(t, m, key("m"))
	m = send(t, m, key("~"))
	m.input.SetValue("~bob/x")
	m = send(t, m, key("enter"))
	if m.mode != modeRemapInput || !strings.Contains(m.View(), "~user") {
		t.Fatalf("~user should be refused in the picker, mode=%v\n%s", m.mode, m.View())
	}
}
