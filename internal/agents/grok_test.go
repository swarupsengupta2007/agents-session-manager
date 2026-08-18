package agents

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

func seedGrok(t *testing.T, home, cwd, id string) string {
	t.Helper()
	dir := filepath.Join(home, ".grok", "sessions", url.PathEscape(cwd), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	summary := `{
  "info": { "id": "` + id + `", "cwd": "` + cwd + `" },
  "session_summary": "Fix the frobnicator",
  "created_at": "2026-08-01T10:00:00.000Z",
  "updated_at": "2026-08-01T11:00:00.000Z",
  "last_active_at": "2026-08-01T11:30:00.000Z",
  "num_chat_messages": 12,
  "current_model_id": "grok-4.5"
}`
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte(summary), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chat_history.jsonl"), []byte(`{"type":"user","content":[]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(home, ".grok", "sessions", "session_search.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS session_docs (
		session_id TEXT, cwd TEXT, updated_at INTEGER, title TEXT,
		content TEXT, content_hash TEXT, last_indexed_offset INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session_docs (session_id, cwd, title) VALUES (?, ?, ?)`,
		id, cwd, "Fix the frobnicator"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestGrokScanAndRemap(t *testing.T) {
	home := t.TempDir()
	oldCwd := filepath.Join(home, "proj-old") // missing -> orphan
	newCwd := filepath.Join(home, "proj-new")
	if err := os.MkdirAll(newCwd, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "019f956e-123d-76a3-8c9d-e77b4cffabed"
	seedGrok(t, home, oldCwd, id)

	// Project-scoped file that must move with the project.
	histPath := filepath.Join(home, ".grok", "sessions", url.PathEscape(oldCwd), "prompt_history.jsonl")
	if err := os.WriteFile(histPath, []byte(`{"prompt":"hello"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := NewGrok(home)
	if !g.Installed() {
		t.Fatal("expected grok installed")
	}
	all, errs := ScanAll(context.Background(), []Agent{g})
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if len(all) != 1 {
		t.Fatalf("got %d sessions", len(all))
	}
	s := all[0]
	if s.ID != id || s.Cwd != oldCwd || !s.Orphan || s.Title != "Fix the frobnicator" || s.Messages != 12 || s.Model != "grok-4.5" {
		t.Fatalf("unexpected session: %+v", s)
	}

	plan, err := g.RemapPlan(all, newCwd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Session dir moved to the encoded new path.
	newDir := filepath.Join(home, ".grok", "sessions", url.PathEscape(newCwd), id)
	if _, err := os.Stat(filepath.Join(newDir, "chat_history.jsonl")); err != nil {
		t.Fatalf("session dir not moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".grok", "sessions", url.PathEscape(oldCwd))); !os.IsNotExist(err) {
		t.Fatal("old encoded dir should be removed")
	}

	// Project-scoped leftovers moved too.
	if _, err := os.Stat(filepath.Join(home, ".grok", "sessions", url.PathEscape(newCwd), "prompt_history.jsonl")); err != nil {
		t.Fatalf("prompt_history.jsonl not moved: %v", err)
	}

	// summary.json cwd rewritten.
	b, err := os.ReadFile(filepath.Join(newDir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"cwd": "`+newCwd+`"`) {
		t.Fatalf("summary cwd not rewritten: %s", b)
	}

	// sqlite row updated.
	db, err := sql.Open("sqlite", filepath.Join(home, ".grok", "sessions", "session_search.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT cwd FROM session_docs WHERE session_id = ?`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != newCwd {
		t.Fatalf("sqlite cwd = %q, want %q", got, newCwd)
	}

	// Re-scan: healthy under the new cwd.
	all, _ = ScanAll(context.Background(), []Agent{g})
	if len(all) != 1 || all[0].Orphan || all[0].Cwd != newCwd {
		t.Fatalf("post-remap scan wrong: %+v", all)
	}
}

func TestDiscoverExtraGrokDir(t *testing.T) {
	t.Setenv("ASM_CONFIG", filepath.Join(t.TempDir(), "asm.json"))
	home := t.TempDir()
	extra := filepath.Join(home, "alt-grok")
	id := "019f96b3-aaaa-7c10-960d-707eeffa7c72"
	seedGrok(t, extra, filepath.Join(home, "proj"), id)
	// seedGrok writes extra/.grok/sessions/...
	root := filepath.Join(extra, ".grok")
	as := DiscoverWith(DiscoverOptions{Extra: map[model.Kind][]string{model.Grok: {root}}})
	var found Agent
	for _, a := range as {
		if a.Kind() == model.Grok && SamePath(a.Root(), root) {
			found = a
		}
	}
	if found == nil {
		t.Fatal("extra grok home not discovered")
	}
	if AgentLabel(found) == "grok" {
		t.Fatalf("extra label %q", AgentLabel(found))
	}
}

func TestGrokIgnoresDeadActivePID(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "019f96b3-dead-7c10-960d-707eeffa7c72"
	seedGrok(t, home, cwd, id)

	if err := os.WriteFile(filepath.Join(home, ".grok", "active_sessions.json"),
		[]byte(`[{"session_id":"`+id+`","pid":999999999,"cwd":"`+cwd+`"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	g := NewGrok(home)
	ss, err := g.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].Active {
		t.Fatalf("dead pid must not mark session active: %+v", ss)
	}
	if _, err := g.RemapPlan(ss, cwd); err != nil {
		t.Fatalf("stale active_sessions entry should not block remap: %v", err)
	}
}

func TestGrokRenamePlan(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "019f96b3-49da-7c10-960d-707eeffa7c72"
	seedGrok(t, home, cwd, id)

	g := NewGrok(home)
	ss, err := g.Scan(context.Background())
	if err != nil || len(ss) != 1 {
		t.Fatalf("scan: %v %+v", err, ss)
	}
	plan, err := g.RenamePlan(ss[0], "Frobnicator v2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	ss, _ = g.Scan(context.Background())
	if len(ss) != 1 || ss[0].Title != "Frobnicator v2" || ss[0].ID != id {
		t.Fatalf("post-rename: %+v", ss)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, ".grok", "sessions", "session_search.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var title string
	if err := db.QueryRow(`SELECT title FROM session_docs WHERE session_id = ?`, id).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "Frobnicator v2" {
		t.Fatalf("sqlite title = %q", title)
	}
}

func TestGrokDelete(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "019f96b3-49da-7c10-960d-707eeffa7c72"
	seedGrok(t, home, cwd, id)

	g := NewGrok(home)
	ss, _ := g.Scan(context.Background())
	plan, err := g.DeletePlan(ss)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	ss, _ = g.Scan(context.Background())
	if len(ss) != 0 {
		t.Fatalf("expected no sessions, got %+v", ss)
	}
	db, _ := sql.Open("sqlite", filepath.Join(home, ".grok", "sessions", "session_search.sqlite"))
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_docs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("sqlite row not deleted: %d", n)
	}
}
