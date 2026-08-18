package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agents-session-manager/internal/migrate"
)

func seedQwen(t *testing.T, home, cwd, id, title string) string {
	t.Helper()
	dir := filepath.Join(home, ".qwen", "projects", EncodePath(cwd), "chats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := strings.Join([]string{
		`{"type":"system","cwd":"` + cwd + `","timestamp":"2026-08-01T10:00:00.000Z"}`,
		`{"type":"user","cwd":"` + cwd + `","timestamp":"2026-08-01T10:00:01.000Z","message":{"role":"user","parts":[{"text":"` + title + `"}]}}`,
		`{"type":"assistant","cwd":"` + cwd + `","model":"qwen3.8-max-preview","timestamp":"2026-08-01T10:00:02.000Z"}`,
	}, "\n") + "\n"
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, _ := json.Marshal(map[string]any{
		"schema_version": 1, "pid": 999999999, "session_id": id, "work_dir": cwd,
	})
	if err := os.WriteFile(filepath.Join(dir, id+".runtime.json"), rt, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestQwenScanAndRemap(t *testing.T) {
	home := t.TempDir()
	oldCwd := filepath.Join(home, "proj-old")
	newCwd := filepath.Join(home, "proj-new")
	if err := os.MkdirAll(newCwd, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "6d5dd3d9-01b8-4df0-a6a5-47391860d6f3"
	seedQwen(t, home, oldCwd, id, "hi from qwen")
	mem := filepath.Join(home, ".qwen", "projects", EncodePath(oldCwd), "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(mem), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mem, []byte("# mem"), 0o644); err != nil {
		t.Fatal(err)
	}

	q := NewQwen(home)
	if !q.Installed() {
		t.Fatal("expected qwen installed")
	}
	all, errs := ScanAll(context.Background(), []Agent{q})
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if len(all) != 1 {
		t.Fatalf("got %d sessions", len(all))
	}
	s := all[0]
	if s.ID != id || s.Cwd != oldCwd || !s.Orphan || s.Title != "hi from qwen" || s.Messages != 2 || s.Model != "qwen3.8-max-preview" {
		t.Fatalf("unexpected session: %+v", s)
	}
	if s.Active {
		t.Fatal("dead runtime pid should not mark the session active")
	}

	plan, err := q.RemapPlan(all, newCwd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatalf("apply: %v", err)
	}

	moved := filepath.Join(home, ".qwen", "projects", EncodePath(newCwd), "chats", id+".jsonl")
	b, err := os.ReadFile(moved)
	if err != nil {
		t.Fatalf("moved jsonl missing: %v", err)
	}
	if !strings.Contains(string(b), `"cwd":"`+newCwd+`"`) || strings.Contains(string(b), `"cwd":"`+oldCwd+`"`) {
		t.Fatalf("cwd not rewritten: %s", b)
	}
	rt, err := os.ReadFile(filepath.Join(home, ".qwen", "projects", EncodePath(newCwd), "chats", id+".runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rt), `"work_dir":"`+newCwd+`"`) {
		t.Fatalf("work_dir not rewritten: %s", rt)
	}
	if _, err := os.Stat(filepath.Join(home, ".qwen", "projects", EncodePath(newCwd), "memory", "MEMORY.md")); err != nil {
		t.Fatalf("memory not moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".qwen", "projects", EncodePath(oldCwd))); !os.IsNotExist(err) {
		t.Fatal("old project dir should be gone")
	}

	all, _ = ScanAll(context.Background(), []Agent{q})
	if len(all) != 1 || all[0].Orphan || all[0].Cwd != newCwd {
		t.Fatalf("post-remap scan wrong: %+v", all)
	}
}

func TestQwenRuntimeOnlyAndDelete(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "aaaaaaaa-1111-2222-3333-444444444444"
	dir := filepath.Join(home, ".qwen", "projects", EncodePath(cwd), "chats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rt, _ := json.Marshal(map[string]any{"pid": 999999999, "session_id": id, "work_dir": cwd})
	if err := os.WriteFile(filepath.Join(dir, id+".runtime.json"), rt, 0o644); err != nil {
		t.Fatal(err)
	}

	q := NewQwen(home)
	ss, err := q.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].ID != id || ss[0].Cwd != cwd {
		t.Fatalf("runtime-only session: %+v", ss)
	}
	plan, err := q.DeletePlan(ss)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	ss, _ = q.Scan(context.Background())
	if len(ss) != 0 {
		t.Fatalf("expected deleted, got %+v", ss)
	}
}

func TestQwenRenamePlan(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "6d5dd3d9-01b8-4df0-a6a5-47391860d6f3"
	seedQwen(t, home, cwd, id, "hi from qwen")

	q := NewQwen(home)
	ss, err := q.Scan(context.Background())
	if err != nil || len(ss) != 1 {
		t.Fatalf("scan: %v %+v", err, ss)
	}
	plan, err := q.RenamePlan(ss[0], "Qwen notes")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	ss, _ = q.Scan(context.Background())
	if len(ss) != 1 || ss[0].Title != "Qwen notes" || ss[0].ID != id {
		t.Fatalf("post-rename: %+v", ss)
	}
}

func TestQwenGuardLiveRuntime(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "bbbbbbbb-1111-2222-3333-444444444444"
	seedQwen(t, home, cwd, id, "live")

	// Point runtime at this test process so PidAlive is true.
	rt := filepath.Join(home, ".qwen", "projects", EncodePath(cwd), "chats", id+".runtime.json")
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "session_id": id, "work_dir": cwd})
	if err := os.WriteFile(rt, b, 0o644); err != nil {
		t.Fatal(err)
	}

	q := NewQwen(home)
	ss, _ := q.Scan(context.Background())
	if len(ss) != 1 || !ss[0].Active {
		t.Fatalf("expected active: %+v", ss)
	}
	if _, err := q.RemapPlan(ss, cwd+"-new"); err == nil {
		t.Fatal("expected live runtime to block remap")
	}
}
