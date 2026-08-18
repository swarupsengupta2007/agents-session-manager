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
	"time"

	_ "modernc.org/sqlite"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

func seedAgy(t *testing.T, home, cwd, id, title string, withSummary bool) {
	t.Helper()
	cli := filepath.Join(home, ".gemini", "antigravity-cli")
	convDir := filepath.Join(cli, "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cli, "brain", id, ".user_uploaded"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cli, "brain", id, "scratch.txt"), []byte("scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(convDir, id+".db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE steps (idx INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO steps (idx) VALUES (0), (1), (2)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if withSummary {
		sumPath := filepath.Join(cli, "conversation_summaries.db")
		sdb, err := sql.Open("sqlite", sumPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sdb.Exec(`CREATE TABLE IF NOT EXISTS conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT "",
			preview TEXT NOT NULL DEFAULT "",
			step_count INTEGER NOT NULL DEFAULT 0,
			last_modified_time TEXT NOT NULL DEFAULT "",
			workspace_uris TEXT NOT NULL DEFAULT "[]"
		)`); err != nil {
			t.Fatal(err)
		}
		uris, _ := json.Marshal([]string{"file://" + cwd})
		if _, err := sdb.Exec(`INSERT INTO conversation_summaries
			(conversation_id, title, preview, step_count, last_modified_time, workspace_uris)
			VALUES (?, ?, ?, 3, ?, ?)`,
			id, title, title, "2026-08-01 10:00:00.000000000+00:00", string(uris)); err != nil {
			t.Fatal(err)
		}
		sdb.Close()
	}

	hist := filepath.Join(cli, "history.jsonl")
	f, err := os.OpenFile(hist, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{
		"display": title, "workspace": cwd, "conversationId": id, "timestamp": time.Now().UnixMilli(),
	})
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()

	proj := filepath.Join(home, ".gemini", "projects.json")
	if _, err := os.Stat(proj); os.IsNotExist(err) {
		doc := map[string]any{"projects": map[string]string{cwd: filepath.Base(cwd)}}
		b, _ := json.MarshalIndent(doc, "", "  ")
		if err := os.WriteFile(proj, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAgyScanAndRemap(t *testing.T) {
	home := t.TempDir()
	oldCwd := filepath.Join(home, "proj-old")
	newCwd := filepath.Join(home, "proj-new")
	if err := os.MkdirAll(newCwd, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "bca1d2fc-6baf-49f5-b367-87268590b64a"
	seedAgy(t, home, oldCwd, id, "Install OpenSSH Server", true)

	// Conversation without a summary row, cwd only in history.
	idNoSum := "7ae4bcb4-06b0-4845-ae7f-9aad00141ecb"
	seedAgy(t, home, oldCwd, idNoSum, "Diagnose tailscale", false)

	// Prefix neighbor must not be remapped.
	other := filepath.Join(home, "proj-old-extra")
	idOther := "aaaaaaaa-1111-2222-3333-444444444444"
	seedAgy(t, home, other, idOther, "Other project", true)

	a := NewAgy(home)
	if !a.Installed() {
		t.Fatal("expected agy installed")
	}
	all, errs := ScanAll(context.Background(), []Agent{a})
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	if len(all) != 3 {
		t.Fatalf("got %d sessions: %+v", len(all), all)
	}
	byID := sessionsByID(all)
	if s := byID[id]; s.Cwd != oldCwd || !s.Orphan || s.Title != "Install OpenSSH Server" || s.Messages != 3 {
		t.Fatalf("summary session: %+v", s)
	}
	if s := byID[idNoSum]; s.Cwd != oldCwd || !s.Orphan || s.Title != "Diagnose tailscale" {
		t.Fatalf("history-only session: %+v", s)
	}

	var group []model.Session
	for _, s := range all {
		if s.Cwd == oldCwd {
			group = append(group, s)
		}
	}
	plan, err := a.RemapPlan(group, newCwd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Summaries rewritten, prefix neighbor intact.
	db, err := sql.Open("sqlite", a.summariesPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var uris string
	if err := db.QueryRow(`SELECT workspace_uris FROM conversation_summaries WHERE conversation_id = ?`, id).Scan(&uris); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(uris, "file://"+newCwd) || strings.Contains(uris, "file://"+oldCwd+"\"") {
		t.Fatalf("workspace_uris = %s", uris)
	}
	if err := db.QueryRow(`SELECT workspace_uris FROM conversation_summaries WHERE conversation_id = ?`, idOther).Scan(&uris); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(uris, "file://"+other) {
		t.Fatalf("other workspace clobbered: %s", uris)
	}

	// history.jsonl
	hb, err := os.ReadFile(a.historyPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hb), `"workspace":"`+newCwd+`"`) {
		t.Fatalf("history not rewritten: %s", hb)
	}
	if !strings.Contains(string(hb), `"workspace":"`+other+`"`) {
		t.Fatalf("history prefix clobber: %s", hb)
	}

	// projects.json
	pb, err := os.ReadFile(a.projectsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pb), `"`+newCwd+`"`) || strings.Contains(string(pb), `"`+oldCwd+`"`) {
		t.Fatalf("projects.json: %s", pb)
	}

	// Conversation files stay put (UUID-keyed).
	if _, err := os.Stat(a.convDB(id)); err != nil {
		t.Fatalf("conversation db moved: %v", err)
	}

	all, _ = ScanAll(context.Background(), []Agent{a})
	byID = sessionsByID(all)
	if s := byID[id]; s.Orphan || s.Cwd != newCwd {
		t.Fatalf("post-remap summary session: %+v", s)
	}
	if s := byID[idNoSum]; s.Orphan || s.Cwd != newCwd {
		t.Fatalf("post-remap history session: %+v", s)
	}
	if s := byID[idOther]; s.Cwd != other {
		t.Fatalf("other session remapped: %+v", s)
	}
}

func TestAgyRenamePlan(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "fab8ccf6-2deb-450a-b8df-f72e9445e4b7"
	seedAgy(t, home, cwd, id, "Empty-ish", true)

	a := NewAgy(home)
	ss, err := a.Scan(context.Background())
	if err != nil || len(ss) != 1 {
		t.Fatalf("scan: %v %+v", err, ss)
	}
	plan, err := a.RenamePlan(ss[0], "SSH notes")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	ss, _ = a.Scan(context.Background())
	if len(ss) != 1 || ss[0].Title != "SSH notes" || ss[0].ID != id {
		t.Fatalf("post-rename: %+v", ss)
	}
}

func TestAgyRenameHistoryOnly(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "7ae4bcb4-06b0-4845-ae7f-9aad00141ecb"
	seedAgy(t, home, cwd, id, "Diagnose tailscale", false)

	a := NewAgy(home)
	ss, err := a.Scan(context.Background())
	if err != nil || len(ss) != 1 {
		t.Fatalf("scan: %v %+v", err, ss)
	}
	plan, err := a.RenamePlan(ss[0], "Tailscale fix")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	ss, _ = a.Scan(context.Background())
	if len(ss) != 1 || ss[0].Title != "Tailscale fix" {
		t.Fatalf("history-only rename: %+v", ss)
	}
}

func TestAgyDelete(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "fab8ccf6-2deb-450a-b8df-f72e9445e4b7"
	seedAgy(t, home, cwd, id, "Empty-ish", true)
	if err := os.MkdirAll(filepath.Join(home, ".gemini", "antigravity-cli", "presence"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "antigravity-cli", "presence", id+".lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAgy(home)
	ss, _ := a.Scan(context.Background())
	plan, err := a.DeletePlan(ss)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	ss, _ = a.Scan(context.Background())
	if len(ss) != 0 {
		t.Fatalf("expected no sessions, got %+v", ss)
	}
	if _, err := os.Stat(a.convDB(id)); !os.IsNotExist(err) {
		t.Fatal("conversation db should be archived")
	}
	if _, err := os.Stat(a.brainDir(id)); !os.IsNotExist(err) {
		t.Fatal("brain dir should be archived")
	}
}

func TestAgyRemapRefusesHeldLock(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "98e76761-f2c1-4b16-987e-ebc17cd407e5"
	seedAgy(t, home, cwd, id, "Live", true)
	lock := filepath.Join(home, ".gemini", "antigravity-cli", "presence", id+".lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("python3", "-c", `
import fcntl, sys, time
f = open(sys.argv[1], "r+")
fcntl.flock(f, fcntl.LOCK_EX)
sys.stdout.write("ready\n")
sys.stdout.flush()
time.sleep(30)
`, lock)
	cmd.Stdout = nil
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

	a := NewAgy(home)
	ss, _ := a.Scan(context.Background())
	if len(ss) != 1 || !ss[0].Active {
		t.Fatalf("expected active session, got %+v", ss)
	}
	if _, err := a.RemapPlan(ss, cwd+"-new"); err == nil {
		t.Fatal("expected remap to refuse a flocked conversation")
	}
	if _, err := a.DeletePlan(ss); err == nil {
		t.Fatal("expected delete to refuse a flocked conversation")
	}
	marks := a.ActiveMarkers()
	if len(marks) != 1 || marks[0].PID != cmd.Process.Pid {
		t.Fatalf("markers = %+v, holder pid %d", marks, cmd.Process.Pid)
	}
}

func TestAgyScanToleratesSummaryWithoutDB(t *testing.T) {
	home := t.TempDir()
	cli := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(filepath.Join(cli, "conversations"), 0o755); err != nil {
		t.Fatal(err)
	}
	sdb, err := sql.Open("sqlite", filepath.Join(cli, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.Exec(`CREATE TABLE conversation_summaries (
		conversation_id TEXT, title TEXT, preview TEXT, step_count INTEGER,
		last_modified_time TEXT, workspace_uris TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.Exec(`INSERT INTO conversation_summaries VALUES
		('only-summary', 'T', 'P', 1, '2026-08-01 00:00:00+00:00', '["file:///gone"]')`); err != nil {
		t.Fatal(err)
	}
	sdb.Close()

	a := NewAgy(home)
	ss, err := a.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].ID != "only-summary" || ss[0].Cwd != "/gone" || ss[0].Title != "T" {
		t.Fatalf("got %+v", ss)
	}
}

func sessionsByID(ss []model.Session) map[string]model.Session {
	out := make(map[string]model.Session, len(ss))
	for _, s := range ss {
		out[s.ID] = s
	}
	return out
}
