package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agents-session-manager/internal/migrate"
)

func seedCodex(t *testing.T, home, cwd, id string) string {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "08", "18")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "rollout-2026-08-18T10-00-00-"+id+".jsonl")
	content := strings.Join([]string{
		`{"timestamp":"2026-08-18T10:00:00.000Z","type":"session_meta","payload":{"id":"` + id + `","timestamp":"2026-08-18T10:00:00.000Z","cwd":"` + cwd + `","originator":"codex-tui"}}`,
		`{"timestamp":"2026-08-18T10:00:05.000Z","type":"event_msg","payload":{"type":"user_message","message":"Refactor the parser please"}}`,
		`{"timestamp":"2026-08-18T10:00:09.000Z","type":"event_msg","payload":{"type":"agent_message","message":"On it."}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCodexScanAndRemap(t *testing.T) {
	home := t.TempDir()
	oldCwd := filepath.Join(home, "work", "proj-old") // missing -> orphan
	newCwd := filepath.Join(home, "work", "proj-new")
	if err := os.MkdirAll(newCwd, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "019fd850-aa1f-7123-b405-a0c92ce72cbc"
	file := seedCodex(t, home, oldCwd, id)

	c := NewCodex(home)
	if !c.Installed() {
		t.Fatal("expected codex installed")
	}
	all, errs := ScanAll(context.Background(), []Agent{c})
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if len(all) != 1 {
		t.Fatalf("got %d sessions", len(all))
	}
	s := all[0]
	if s.ID != id || s.Cwd != oldCwd || !s.Orphan || s.Messages != 2 {
		t.Fatalf("unexpected session: %+v", s)
	}
	if s.Title != "Refactor the parser please" {
		t.Fatalf("title = %q", s.Title)
	}

	plan, err := c.RemapPlan(all, newCwd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"cwd":"`+newCwd+`"`) || strings.Contains(string(b), `"cwd":"`+oldCwd+`"`) {
		t.Fatalf("cwd not rewritten: %s", b)
	}

	all, _ = ScanAll(context.Background(), []Agent{c})
	if len(all) != 1 || all[0].Orphan || all[0].Cwd != newCwd {
		t.Fatalf("post-remap scan wrong: %+v", all)
	}
}

func TestCodexRenamePlan(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "019fd850-aa1f-7123-b405-a0c92ce72cbc"
	file := seedCodex(t, home, cwd, id)

	c := NewCodex(home)
	ss, err := c.Scan(context.Background())
	if err != nil || len(ss) != 1 {
		t.Fatalf("scan: %v %+v", err, ss)
	}
	plan, err := c.RenamePlan(ss[0], "Parser rewrite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file + ".title")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "Parser rewrite" {
		t.Fatalf("sidecar = %q", b)
	}
	ss, _ = c.Scan(context.Background())
	if len(ss) != 1 || ss[0].Title != "Parser rewrite" || ss[0].ID != id {
		t.Fatalf("post-rename: %+v", ss)
	}
}
