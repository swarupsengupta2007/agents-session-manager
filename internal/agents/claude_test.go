package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

func TestEncodePath(t *testing.T) {
	cases := map[string]string{
		"/root":                        "-root",
		"/path/to/dir1":                "-path-to-dir1",
		"/home/u/my.project":           "-home-u-my-project",
		"/root/agents-session-manager": "-root-agents-session-manager",
		"/tmp/enc.probe_x":             "-tmp-enc-probe-x", // observed from a real claude run
	}
	for in, want := range cases {
		if got := EncodePath(in); got != want {
			t.Errorf("EncodePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func seedClaude(t *testing.T, home, cwd, id string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", EncodePath(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := strings.Join([]string{
		`{"type":"system","cwd":"` + cwd + `","timestamp":"2026-08-01T10:00:00.000Z"}`,
		`{"type":"user","cwd":"` + cwd + `","timestamp":"2026-08-01T10:00:01.000Z"}`,
		`{"type":"assistant","cwd":"` + cwd + `","model":"claude-opus-4-5","timestamp":"2026-08-01T10:00:02.000Z"}`,
		`{"type":"ai-title","aiTitle":"Fix the widget","sessionId":"` + id + `"}`,
	}, "\n") + "\n"
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaudeScanAndRemap(t *testing.T) {
	home := t.TempDir()
	oldCwd := filepath.Join(home, "proj-old") // will NOT exist -> orphan
	newCwd := filepath.Join(home, "proj-new")
	if err := os.MkdirAll(newCwd, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "0664494f-279a-4d7d-a398-2c83039d6885"
	seedClaude(t, home, oldCwd, id)

	c := NewClaude(home)
	if !c.Installed() {
		t.Fatal("expected claude installed")
	}
	ss, err := c.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 {
		t.Fatalf("got %d sessions", len(ss))
	}
	s := ss[0]
	if s.ID != id || s.Cwd != oldCwd || s.Title != "Fix the widget" || s.Messages != 2 || s.Model != "claude-opus-4-5" {
		t.Fatalf("unexpected session: %+v", s)
	}

	// Orphan flagging through ScanAll.
	all, errs := ScanAll(context.Background(), []Agent{c})
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if len(all) != 1 || !all[0].Orphan {
		t.Fatalf("expected single orphaned session, got %+v", all)
	}

	plan, err := c.RemapPlan(all, newCwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	rep, err := migrate.Apply(plan, filepath.Join(home, "backups"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.BackupDir == "" {
		t.Fatal("no backup dir reported")
	}

	// Session must now live under the encoded new path with rewritten cwd.
	moved := filepath.Join(home, ".claude", "projects", EncodePath(newCwd), id+".jsonl")
	b, err := os.ReadFile(moved)
	if err != nil {
		t.Fatalf("moved file missing: %v", err)
	}
	if !strings.Contains(string(b), `"cwd":"`+newCwd+`"`) || strings.Contains(string(b), `"cwd":"`+oldCwd+`"`) {
		t.Fatalf("cwd not rewritten: %s", b)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "projects", EncodePath(oldCwd))); !os.IsNotExist(err) {
		t.Fatal("old project dir should be gone")
	}

	// Re-scan: session found, no longer orphaned, under new cwd.
	all, _ = ScanAll(context.Background(), []Agent{c})
	if len(all) != 1 || all[0].Orphan || all[0].Cwd != newCwd {
		t.Fatalf("post-remap scan wrong: %+v", all)
	}
}

func TestClaudeRemapMovesProjectData(t *testing.T) {
	home := t.TempDir()
	oldCwd := filepath.Join(home, "proj-old")
	newCwd := filepath.Join(home, "proj-new")
	os.MkdirAll(newCwd, 0o755)
	id := "bbbbbbbb-1111-2222-3333-444444444444"
	seedClaude(t, home, oldCwd, id)

	// Sidecar dir with tool results + project memory, as Claude creates them.
	oldDir := filepath.Join(home, ".claude", "projects", EncodePath(oldCwd))
	writeFile := func(p, content string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(oldDir, id, "tool-results", "big.txt"), "tool output")
	writeFile(filepath.Join(oldDir, "memory", "MEMORY.md"), "# memory")

	c := NewClaude(home)
	ss, _ := c.Scan(context.Background())
	plan, err := c.RemapPlan(ss, newCwd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatalf("apply: %v", err)
	}

	newDir := filepath.Join(home, ".claude", "projects", EncodePath(newCwd))
	for _, p := range []string{
		filepath.Join(newDir, id+".jsonl"),
		filepath.Join(newDir, id, "tool-results", "big.txt"),
		filepath.Join(newDir, "memory", "MEMORY.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s after remap: %v", p, err)
		}
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("old project dir should be gone")
	}
}

func TestClaudeRemapRejectsConflict(t *testing.T) {
	home := t.TempDir()
	oldCwd := filepath.Join(home, "proj-old")
	newCwd := filepath.Join(home, "proj-new")
	os.MkdirAll(newCwd, 0o755)
	id := "cccccccc-1111-2222-3333-444444444444"
	seedClaude(t, home, oldCwd, id)

	oldDir := filepath.Join(home, ".claude", "projects", EncodePath(oldCwd))
	newDir := filepath.Join(home, ".claude", "projects", EncodePath(newCwd))
	for _, d := range []string{oldDir, newDir} {
		if err := os.MkdirAll(filepath.Join(d, "memory"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	c := NewClaude(home)
	ss, _ := c.Scan(context.Background())
	if _, err := c.RemapPlan(ss, newCwd); err == nil {
		t.Fatal("expected conflict error for existing memory dir")
	}
}

func TestClaudeDeletePlan(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "aaaaaaaa-1111-2222-3333-444444444444"
	seedClaude(t, home, cwd, id)

	c := NewClaude(home)
	ss, _ := c.Scan(context.Background())
	plan, err := c.DeletePlan(ss)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	ss, _ = c.Scan(context.Background())
	if len(ss) != 0 {
		t.Fatalf("session should be deleted, got %+v", ss)
	}
}

func TestClaudeRenamePlan(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "aaaaaaaa-1111-2222-3333-444444444444"
	seedClaude(t, home, cwd, id)

	c := NewClaude(home)
	ss, err := c.Scan(context.Background())
	if err != nil || len(ss) != 1 {
		t.Fatalf("scan: %v %+v", err, ss)
	}
	if _, err := c.RenamePlan(ss[0], "  "); err == nil {
		t.Fatal("empty title should fail")
	}
	if _, err := c.RenamePlan(ss[0], "Fix the widget"); err == nil {
		t.Fatal("unchanged title should fail")
	}
	plan, err := c.RenamePlan(ss[0], "Widget v2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	ss, _ = c.Scan(context.Background())
	if len(ss) != 1 || ss[0].Title != "Widget v2" || ss[0].ID != id {
		t.Fatalf("post-rename: %+v", ss)
	}

	// Transcript without an ai-title record: append one.
	id2 := "bbbbbbbb-1111-2222-3333-444444444444"
	dir := filepath.Join(home, ".claude", "projects", EncodePath(cwd))
	p := filepath.Join(dir, id2+".jsonl")
	if err := os.WriteFile(p, []byte(`{"type":"user","cwd":"`+cwd+`"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ss, _ = c.Scan(context.Background())
	var untitled model.Session
	for _, s := range ss {
		if s.ID == id2 {
			untitled = s
		}
	}
	if untitled.ID == "" {
		t.Fatal("untitled session missing")
	}
	plan, err = c.RenamePlan(untitled, "Named later")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	ss, _ = c.Scan(context.Background())
	var got string
	for _, s := range ss {
		if s.ID == id2 {
			got = s.Title
		}
	}
	if got != "Named later" {
		t.Fatalf("appended title = %q", got)
	}
}

func TestDiscoverExtraClaudeDir(t *testing.T) {
	t.Setenv("ASM_CONFIG", filepath.Join(t.TempDir(), "asm.json"))

	extra := t.TempDir()
	id := "dddddddd-1111-2222-3333-444444444444"
	content := `{"type":"user","cwd":"/gone","timestamp":"2026-08-01T10:00:00.000Z"}` + "\n"
	proj := filepath.Join(extra, "projects", EncodePath("/gone"))
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	as := DiscoverWith(DiscoverOptions{ExtraClaudeDirs: []string{extra}})
	var extraAgent Agent
	for _, a := range as {
		if a.Kind() == model.Claude && SamePath(a.Root(), extra) {
			extraAgent = a
		}
	}
	if extraAgent == nil {
		t.Fatal("extra Claude CONFIG_DIR was not discovered")
	}
	if AgentLabel(extraAgent) == "claude" {
		t.Fatalf("extra store should be labeled, got %q", AgentLabel(extraAgent))
	}
	ss, err := extraAgent.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].ID != id {
		t.Fatalf("extra scan %+v", ss)
	}
}

func TestDiscoverSkipsMissing(t *testing.T) {
	home := t.TempDir()
	c := NewClaude(home)
	if c.Installed() {
		t.Fatal("claude should not be installed in empty home")
	}
	_ = model.Claude
}
