package migrate

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRewriteCwdInFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
		old     string
		new     string
		want    string
	}{
		{
			name:    "compact jsonl",
			content: `{"type":"system","cwd":"/path/to/dir1","x":1}` + "\n" + `{"cwd":"/path/to/dir1"}` + "\n",
			old:     "/path/to/dir1",
			new:     "/path/to/dir2",
			want:    `{"type":"system","cwd":"/path/to/dir2","x":1}` + "\n" + `{"cwd":"/path/to/dir2"}` + "\n",
		},
		{
			name:    "pretty json with spaces",
			content: "{\n  \"info\": {\n    \"cwd\": \"/path/to/dir1\"\n  }\n}",
			old:     "/path/to/dir1",
			new:     "/path/to/dir2",
			want:    "{\n  \"info\": {\n    \"cwd\": \"/path/to/dir2\"\n  }\n}",
		},
		{
			name:    "path needing json escaping",
			content: `{"cwd":"/we\"ird/dir1"}`,
			old:     `/we"ird/dir1`,
			new:     `/we"ird/dir2`,
			want:    `{"cwd":"/we\"ird/dir2"}`,
		},
		{
			name:    "no match untouched",
			content: `{"cwd":"/somewhere/else"}`,
			old:     "/path/to/dir1",
			new:     "/path/to/dir2",
			want:    `{"cwd":"/somewhere/else"}`,
		},
		{
			name:    "prefix path not clobbered",
			content: `{"cwd":"/path/to/dir10"}`,
			old:     "/path/to/dir1",
			new:     "/path/to/dir2",
			want:    `{"cwd":"/path/to/dir10"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "f.jsonl")
			writeFile(t, p, tc.content)
			if err := RewriteCwdInFile(p, tc.old, tc.new); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRewriteCwdPreservesMtime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.jsonl")
	writeFile(t, p, `{"cwd":"/old"}`+"\n")
	past := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, past, past); err != nil {
		t.Fatal(err)
	}
	if err := RewriteCwdInFile(p, "/old", "/new"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(past) {
		t.Fatalf("mtime changed: %v != %v", info.ModTime(), past)
	}
}

func TestApplyMoveFileWithBackup(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "home", ".agent", "projects", "-old", "sess.jsonl")
	writeFile(t, src, `{"cwd":"/old"}`+"\n")
	dst := filepath.Join(tmp, "home", ".agent", "projects", "-new", "sess.jsonl")

	plan := &Plan{
		Agent:  "test",
		OldCwd: "/old",
		NewCwd: "/new",
	}
	plan.Add(Action{Kind: RewriteCwd, Src: src, Old: "/old", New: "/new", Desc: "rewrite"})
	plan.Add(Action{Kind: MoveFile, Src: src, Dst: dst, Desc: "move"})
	plan.Add(Action{Kind: RemoveEmptyDir, Src: filepath.Dir(src), Desc: "rmdir"})

	rep, err := Apply(plan, filepath.Join(tmp, "backups"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(rep.Done) != 3 {
		t.Fatalf("done = %v", rep.Done)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("dst missing: %v", err)
	}
	if !strings.Contains(string(got), `{"cwd":"/new"}`) {
		t.Fatalf("dst not rewritten: %s", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("src still exists")
	}
	if _, err := os.Stat(filepath.Dir(src)); !os.IsNotExist(err) {
		t.Fatal("old project dir still exists")
	}

	// Backup must contain the pristine original.
	var backupFile string
	filepath.WalkDir(rep.BackupDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) == "sess.jsonl" && backupFile == "" {
			backupFile = p
		}
		return nil
	})
	if backupFile == "" {
		t.Fatalf("no backup of sess.jsonl under %s", rep.BackupDir)
	}
	b, err := os.ReadFile(backupFile)
	if err != nil || !strings.Contains(string(b), `{"cwd":"/old"}`) {
		t.Fatalf("backup does not hold original: %v %s", err, b)
	}
}

func TestApplyArchive(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "sess-dir")
	writeFile(t, filepath.Join(src, "chat.jsonl"), "hello")

	plan := &Plan{Agent: "test"}
	plan.Add(Action{Kind: Archive, Src: src, Desc: "archive"})

	rep, err := Apply(plan, filepath.Join(tmp, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("archived source still exists")
	}
	if _, err := os.Stat(filepath.Join(rep.BackupDir, "archived", "sess-dir", "chat.jsonl")); err != nil {
		t.Fatalf("archived copy missing: %v", err)
	}
}

func TestRewriteProjectsJSON(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "projects.json")
	writeFile(t, p, "{\n  \"projects\": {\n    \"/old\": \"oldname\",\n    \"/other\": \"keep\"\n  }\n}\n")

	plan := &Plan{Agent: "agy", OldCwd: "/old", NewCwd: "/new"}
	plan.Add(Action{Kind: ProjectsJSONRemap, Src: p, Old: "/old", New: "/new", Desc: "projects"})
	if _, err := Apply(plan, filepath.Join(tmp, "backups")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, `"/new"`) || strings.Contains(s, `"/old"`) || !strings.Contains(s, `"keep"`) {
		t.Fatalf("projects.json rewrite wrong: %s", s)
	}
}

func TestRewriteProjectsJSONObjectValues(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "trust.json")
	writeFile(t, p, `{
  "schema_version": 1,
  "projects": {
    "/old": {"decision": "trusted"},
    "/other": {"decision": "trusted"}
  }
}
`)
	if err := rewriteProjectsJSON(p, "/old", "/new"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, `"/new"`) || strings.Contains(s, `"/old"`) || !strings.Contains(s, `"trusted"`) || !strings.Contains(s, `"schema_version"`) {
		t.Fatalf("trust.json rewrite wrong: %s", s)
	}
}

func TestSQLiteSetCwdCustomColumn(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (session_id TEXT, workspace_root TEXT, workspace_key TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions VALUES ('s1', '/old', '/old')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	plan := &Plan{Agent: "muse", OldCwd: "/old", NewCwd: "/new"}
	plan.Add(Action{Kind: SQLiteSetCwd, Src: dbPath, SessionID: "s1", New: "/new",
		Table: "sessions", Column: "session_id", SetColumn: "workspace_root", Desc: "root"})
	plan.Add(Action{Kind: SQLiteSetCwd, Src: dbPath, SessionID: "s1", New: "/new",
		Table: "sessions", Column: "session_id", SetColumn: "workspace_key", Desc: "key"})
	if _, err := Apply(plan, filepath.Join(tmp, "backups")); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var root, key string
	if err := db.QueryRow(`SELECT workspace_root, workspace_key FROM sessions WHERE session_id='s1'`).Scan(&root, &key); err != nil {
		t.Fatal(err)
	}
	if root != "/new" || key != "/new" {
		t.Fatalf("root=%q key=%q", root, key)
	}
}

func TestRewriteProjectsJSONConflict(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "projects.json")
	writeFile(t, p, `{"projects":{"/old":"a","/new":"b"}}`+"\n")
	if err := rewriteProjectsJSON(p, "/old", "/new"); err == nil {
		t.Fatal("expected conflict")
	}
}

func TestSQLiteSetWorkspace(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (
		conversation_id TEXT PRIMARY KEY, workspace_uris TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES
		('c1', '["file:///path/to/dir1"]'),
		('c2', '["file:///path/to/dir10"]')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	plan := &Plan{Agent: "agy", OldCwd: "/path/to/dir1", NewCwd: "/path/to/dir2"}
	plan.Add(Action{Kind: SQLiteSetWorkspace, Src: dbPath, SessionID: "c1",
		Old: "/path/to/dir1", New: "/path/to/dir2", Desc: "ws c1"})
	plan.Add(Action{Kind: SQLiteSetWorkspace, Src: dbPath, SessionID: "missing",
		Old: "/path/to/dir1", New: "/path/to/dir2", Desc: "ws missing"})
	if _, err := Apply(plan, filepath.Join(tmp, "backups")); err != nil {
		t.Fatal(err)
	}

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var u1, u2 string
	if err := db.QueryRow(`SELECT workspace_uris FROM conversation_summaries WHERE conversation_id='c1'`).Scan(&u1); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT workspace_uris FROM conversation_summaries WHERE conversation_id='c2'`).Scan(&u2); err != nil {
		t.Fatal(err)
	}
	if u1 != `["file:///path/to/dir2"]` {
		t.Fatalf("c1 = %q", u1)
	}
	if u2 != `["file:///path/to/dir10"]` {
		t.Fatalf("prefix clobber: c2 = %q", u2)
	}
}

func TestSQLiteDeleteCustomColumn(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT, workspace_uris TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES ('c1', '[]'), ('c2', '[]')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	plan := &Plan{Agent: "agy"}
	plan.Add(Action{
		Kind: SQLiteDelete, Src: dbPath, SessionID: "c1",
		Table: "conversation_summaries", Column: "conversation_id", Desc: "del",
	})
	if _, err := Apply(plan, filepath.Join(tmp, "backups")); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_summaries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows = %d", n)
	}
}

func TestSetJSONStringField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	writeFile(t, p, "{\n  \"session_summary\": \"old\",\n  \"x\": 1\n}\n")
	if err := SetJSONStringField(p, "session_summary", "new title"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"session_summary": "new title"`) || !strings.Contains(string(b), `"x"`) {
		t.Fatalf("%s", b)
	}
}

func TestAppendJSONLAndWriteFile(t *testing.T) {
	tmp := t.TempDir()
	jsonl := filepath.Join(tmp, "hist.jsonl")
	writeFile(t, jsonl, `{"display":"old"}`+"\n")
	sidecar := filepath.Join(tmp, "rollout.jsonl.title")

	plan := &Plan{Agent: "test", NewTitle: "new"}
	plan.Add(Action{Kind: AppendJSONL, Src: jsonl, New: `{"display":"new"}`, Desc: "append"})
	plan.Add(Action{Kind: WriteFile, Src: sidecar, New: "new\n", Desc: "sidecar"})
	if _, err := Apply(plan, filepath.Join(tmp, "backups")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(jsonl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"display":"old"`) || !strings.Contains(string(b), `"display":"new"`) {
		t.Fatalf("jsonl = %s", b)
	}
	got, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new\n" {
		t.Fatalf("sidecar = %q", got)
	}
}

func TestSQLiteMuseRename(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "session-index.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (
		session_id TEXT, title TEXT, session_name TEXT, session_name_revision INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions VALUES ('s1', 'old', 'old', 0)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	plan := &Plan{Agent: "muse", NewTitle: "new"}
	plan.Add(Action{Kind: SQLiteMuseRename, Src: dbPath, SessionID: "s1", New: "new",
		Table: "sessions", Column: "session_id", Desc: "rename"})
	if _, err := Apply(plan, filepath.Join(tmp, "backups")); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var title, name string
	var rev int
	if err := db.QueryRow(`SELECT title, session_name, session_name_revision FROM sessions`).
		Scan(&title, &name, &rev); err != nil {
		t.Fatal(err)
	}
	if title != "new" || name != "new" || rev != 1 {
		t.Fatalf("title=%q name=%q rev=%d", title, name, rev)
	}
}

func TestSQLiteMuseRenameFallsBackToTitle(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "session-index.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (session_id TEXT, title TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions VALUES ('s1', 'old')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	plan := &Plan{Agent: "muse", NewTitle: "new"}
	plan.Add(Action{Kind: SQLiteMuseRename, Src: dbPath, SessionID: "s1", New: "new",
		Table: "sessions", Column: "session_id", Desc: "rename"})
	if _, err := Apply(plan, filepath.Join(tmp, "backups")); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var title string
	if err := db.QueryRow(`SELECT title FROM sessions`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "new" {
		t.Fatalf("title=%q", title)
	}
}

func TestApplyProbeAbortsBeforeSwap(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "sess.jsonl")
	writeFile(t, src, `{"cwd":"/old"}`+"\n")

	n := 0
	probe := func() error {
		n++
		if n >= 3 {
			return fmt.Errorf("agent locked")
		}
		return nil
	}
	plan := &Plan{Agent: "test"}
	plan.Add(Action{Kind: RewriteCwd, Src: src, Old: "/old", New: "/new", Desc: "rewrite"})
	_, err := ApplyWith(plan, filepath.Join(tmp, "backups"), probe)
	if err == nil {
		t.Fatal("expected probe abort")
	}
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `{"cwd":"/old"}`) || strings.Contains(string(got), `{"cwd":"/new"}`) {
		t.Fatalf("original mutated despite abort: %s", got)
	}
}

func TestApplyDeltaMismatchWarns(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "sess.jsonl")
	writeFile(t, src, `{"cwd":"/old"}`+"\n")

	n := 0
	probe := func() error {
		n++
		if n == 4 {
			// Simulate a writer landing after our swap, before the verify read.
			return os.WriteFile(src, []byte("clobbered\n"), 0o644)
		}
		return nil
	}
	plan := &Plan{Agent: "test"}
	plan.Add(Action{Kind: RewriteCwd, Src: src, Old: "/old", New: "/new", Desc: "rewrite"})
	rep, err := ApplyWith(plan, filepath.Join(tmp, "backups"), probe)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(rep.Warnings) == 0 || !strings.Contains(rep.Warnings[0], "potential corruption") {
		t.Fatalf("warnings = %v", rep.Warnings)
	}
}

func TestApplyCopyModifyDoesNotLeaveWorkFile(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "sess.jsonl")
	writeFile(t, src, `{"cwd":"/old"}`+"\n")
	plan := &Plan{Agent: "test"}
	plan.Add(Action{Kind: RewriteCwd, Src: src, Old: "/old", New: "/new", Desc: "rewrite"})
	if _, err := Apply(plan, filepath.Join(tmp, "backups")); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".asm-") && strings.HasSuffix(e.Name(), ".work") {
			t.Fatalf("leftover work file %s", e.Name())
		}
	}
	got, _ := os.ReadFile(src)
	if !strings.Contains(string(got), `{"cwd":"/new"}`) {
		t.Fatalf("not rewritten: %s", got)
	}
}

func TestRemoveEmptyDirKeepsNonEmpty(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "d")
	writeFile(t, filepath.Join(dir, "x"), "y")
	if err := removeIfEmpty(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("non-empty dir was removed")
	}
}
