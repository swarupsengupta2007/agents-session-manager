package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

func seedMuse(t *testing.T, home, cwd, id, title string) string {
	t.Helper()
	dir := filepath.Join(home, ".local", "share", "muse", "sessions", "2026", "08", "18", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "session.jsonl")
	lines := []string{
		`{"payload_type":"runtime.session.metadata","recorded_at":1787034281722928,"payload":{"kind":"metadata","record":{"model_id":"muse-spark-1.2-contributor","workspace_root":"` + cwd + `"}}}`,
		`{"payload_type":"runtime.session.route_facts","recorded_at":1787034281722986,"payload":{"kind":"route_facts","record":{"cwd":"` + cwd + `","pid":1}}}`,
		`{"payload_type":"runtime.user_intent.accepted","recorded_at":1787034281814455,"payload":{"model_messages":[{"content":[{"kind":"text","text":"` + title + `"}]}]}}`,
	}
	if err := os.WriteFile(log, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".session.lock"), []byte("pid=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(home, ".local", "share", "muse", "session-index.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		session_id TEXT PRIMARY KEY,
		session_stream_id TEXT NOT NULL DEFAULT '',
		session_dir TEXT NOT NULL DEFAULT '',
		session_log_path TEXT NOT NULL DEFAULT '',
		layout TEXT NOT NULL DEFAULT '',
		workspace_root TEXT,
		workspace_key TEXT,
		provider_id TEXT,
		model_id TEXT,
		title TEXT NOT NULL DEFAULT '',
		session_name TEXT NOT NULL DEFAULT '',
		session_name_revision INTEGER NOT NULL DEFAULT 0,
		search_text TEXT NOT NULL DEFAULT '',
		created_at_us INTEGER,
		updated_at_us INTEGER,
		prompt_count INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'valid',
		status_rank INTEGER NOT NULL DEFAULT 0,
		indexed_at_us INTEGER NOT NULL DEFAULT 0,
		latest_segment_terminated INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO sessions
		(session_id, session_stream_id, session_dir, session_log_path, layout,
		 workspace_root, workspace_key, model_id, title, session_name, search_text, prompt_count, indexed_at_us)
		VALUES (?, ?, ?, ?, 'session_jsonl', ?, ?, 'muse-spark-1.2-contributor', ?, ?, ?, 1, 1)`,
		id, id, dir, log, cwd, cwd, title, title, id+"\x1f"+cwd); err != nil {
		t.Fatal(err)
	}
	db.Close()

	cfg := filepath.Join(home, ".config", "muse")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	trust := filepath.Join(cfg, "trust.json")
	if _, err := os.Stat(trust); os.IsNotExist(err) {
		doc := map[string]any{
			"schema_version": 1,
			"projects":       map[string]any{cwd: map[string]string{"decision": "trusted"}},
		}
		b, _ := json.MarshalIndent(doc, "", "  ")
		if err := os.WriteFile(trust, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestMuseScanAndRemap(t *testing.T) {
	home := t.TempDir()
	oldCwd := filepath.Join(home, "proj-old")
	newCwd := filepath.Join(home, "proj-new")
	if err := os.MkdirAll(newCwd, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "2c70a816-46a3-409a-8c67-fe442a290686"
	dir := seedMuse(t, home, oldCwd, id, "update")

	other := filepath.Join(home, "proj-old-extra")
	idOther := "aaaaaaaa-1111-2222-3333-444444444444"
	seedMuse(t, home, other, idOther, "other")

	m := NewMuse(home)
	if !m.Installed() {
		t.Fatal("expected muse installed")
	}
	all, errs := ScanAll(context.Background(), []Agent{m})
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if len(all) != 2 {
		t.Fatalf("got %d sessions: %+v", len(all), all)
	}
	byID := sessionsByID(all)
	s := byID[id]
	if s.Cwd != oldCwd || !s.Orphan || s.Title != "update" || s.Messages < 1 {
		t.Fatalf("unexpected session: %+v", s)
	}

	var group []model.Session
	for _, x := range all {
		if x.Cwd == oldCwd {
			group = append(group, x)
		}
	}
	plan, err := m.RemapPlan(group, newCwd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Session dir stays put; jsonl rewritten.
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("session dir moved: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"workspace_root":"`+newCwd+`"`) ||
		!strings.Contains(string(b), `"cwd":"`+newCwd+`"`) ||
		strings.Contains(string(b), `"cwd":"`+oldCwd+`"`) {
		t.Fatalf("jsonl not rewritten: %s", b)
	}

	db, err := sql.Open("sqlite", m.indexPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var root, key string
	if err := db.QueryRow(`SELECT workspace_root, workspace_key FROM sessions WHERE session_id=?`, id).Scan(&root, &key); err != nil {
		t.Fatal(err)
	}
	if root != newCwd || key != newCwd {
		t.Fatalf("index root=%q key=%q", root, key)
	}
	if err := db.QueryRow(`SELECT workspace_root FROM sessions WHERE session_id=?`, idOther).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if root != other {
		t.Fatalf("prefix neighbor clobbered: %q", root)
	}

	tb, err := os.ReadFile(m.trustPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tb), `"`+newCwd+`"`) || strings.Contains(string(tb), `"`+oldCwd+`"`) {
		t.Fatalf("trust.json: %s", tb)
	}

	all, _ = ScanAll(context.Background(), []Agent{m})
	byID = sessionsByID(all)
	if s := byID[id]; s.Orphan || s.Cwd != newCwd {
		t.Fatalf("post-remap: %+v", s)
	}
	if s := byID[idOther]; s.Cwd != other {
		t.Fatalf("other remapped: %+v", s)
	}
}

func TestMuseRenamePlan(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "bbbbbbbb-1111-2222-3333-444444444444"
	seedMuse(t, home, cwd, id, "update")

	m := NewMuse(home)
	ss, err := m.Scan(context.Background())
	if err != nil || len(ss) != 1 {
		t.Fatalf("scan: %v %+v", err, ss)
	}
	plan, err := m.RenamePlan(ss[0], "Ship it")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	ss, _ = m.Scan(context.Background())
	if len(ss) != 1 || ss[0].Title != "Ship it" || ss[0].ID != id {
		t.Fatalf("post-rename: %+v", ss)
	}
	db, err := sql.Open("sqlite", m.indexPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var title, name string
	var rev int
	if err := db.QueryRow(`SELECT title, session_name, session_name_revision FROM sessions WHERE session_id=?`, id).
		Scan(&title, &name, &rev); err != nil {
		t.Fatal(err)
	}
	if title != "Ship it" || name != "Ship it" || rev != 1 {
		t.Fatalf("index title=%q name=%q rev=%d", title, name, rev)
	}
}

func TestMuseDelete(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "bbbbbbbb-1111-2222-3333-444444444444"
	dir := seedMuse(t, home, cwd, id, "bye")

	m := NewMuse(home)
	ss, _ := m.Scan(context.Background())
	plan, err := m.DeletePlan(ss)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("session dir should be archived")
	}
	ss, _ = m.Scan(context.Background())
	if len(ss) != 0 {
		t.Fatalf("expected no sessions, got %+v", ss)
	}
}

func TestMuseRemapRefusesHeldLock(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "cccccccc-1111-2222-3333-444444444444"
	dir := seedMuse(t, home, cwd, id, "live")
	lock := filepath.Join(dir, ".session.lock")

	cmd := exec.Command("python3", "-c", `
import fcntl, sys, time
f = open(sys.argv[1], "r+")
fcntl.flock(f, fcntl.LOCK_EX)
sys.stdout.write("ready\n")
sys.stdout.flush()
time.sleep(30)
`, lock)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	buf := make([]byte, 8)
	if _, err := stdout.Read(buf); err != nil {
		t.Fatal(err)
	}

	m := NewMuse(home)
	ss, _ := m.Scan(context.Background())
	if len(ss) != 1 || !ss[0].Active {
		t.Fatalf("expected active session, got %+v", ss)
	}
	if _, err := m.RemapPlan(ss, cwd+"-new"); err == nil {
		t.Fatal("expected remap to refuse a flocked session")
	}
	marks := m.ActiveMarkers()
	if len(marks) != 1 || marks[0].PID != cmd.Process.Pid {
		t.Fatalf("markers = %+v holder %d", marks, cmd.Process.Pid)
	}
}
