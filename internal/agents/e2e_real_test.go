package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

// TestEndToEndRealDataCopy copies this machine's real agent data into a
// sandbox, simulates the project folder moving (transcripts left pointing at
// a path that no longer exists), then remaps every agent through the same
// code paths the TUI uses and verifies the sessions become healthy again.
//
// Skips when the machine has no real agent data to copy.
func TestEndToEndRealDataCopy(t *testing.T) {
	home := homeDir()
	if _, err := os.Stat(filepath.Join(home, ".claude", "projects")); err != nil {
		t.Skip("no real claude data on this machine")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "sessions")); err != nil {
		t.Skip("no real codex data on this machine")
	}
	if _, err := os.Stat(filepath.Join(home, ".grok", "sessions")); err != nil {
		t.Skip("no real grok data on this machine")
	}

	sandbox := t.TempDir()
	oldCwd := filepath.Join(sandbox, "moved-from") // never created -> orphan
	newCwd := filepath.Join(sandbox, "moved-to")
	if err := os.MkdirAll(newCwd, 0o755); err != nil {
		t.Fatal(err)
	}

	claudeRoot := filepath.Join(sandbox, "claude")
	codexRoot := filepath.Join(sandbox, "codex")
	grokRoot := filepath.Join(sandbox, "grok")
	for _, c := range [][]string{
		{filepath.Join(home, ".claude"), claudeRoot},
		{filepath.Join(home, ".codex"), codexRoot},
		{filepath.Join(home, ".grok"), grokRoot},
	} {
		out, err := exec.Command("cp", "-a", c[0], c[1]).CombinedOutput()
		if err != nil {
			t.Fatalf("copy %s: %v\n%s", c[0], err, out)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", claudeRoot)
	t.Setenv("ASM_CONFIG", filepath.Join(sandbox, "asm-config.json"))
	t.Setenv("CODEX_HOME", codexRoot)
	t.Setenv("GROK_HOME", grokRoot)

	// Always isolate GEMINI_HOME so Discover never touches the real ~/.gemini
	// store (agy may have it open). Copy if present so agy is exercised too.
	geminiRoot := filepath.Join(sandbox, "gemini")
	if _, err := os.Stat(filepath.Join(home, ".gemini", "antigravity-cli")); err == nil {
		if out, err := exec.Command("cp", "-a", filepath.Join(home, ".gemini"), geminiRoot).CombinedOutput(); err != nil {
			t.Fatalf("copy gemini: %v\n%s", err, out)
		}
	} else if err := os.MkdirAll(geminiRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GEMINI_HOME", geminiRoot)

	qwenRoot := filepath.Join(sandbox, "qwen")
	if _, err := os.Stat(filepath.Join(home, ".qwen", "projects")); err == nil {
		if out, err := exec.Command("cp", "-a", filepath.Join(home, ".qwen"), qwenRoot).CombinedOutput(); err != nil {
			t.Fatalf("copy qwen: %v\n%s", err, out)
		}
	} else if err := os.MkdirAll(qwenRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QWEN_HOME", qwenRoot)

	museRoot := filepath.Join(sandbox, "muse")
	museCfg := filepath.Join(sandbox, "muse-config")
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "muse", "sessions")); err == nil {
		if out, err := exec.Command("cp", "-a", filepath.Join(home, ".local", "share", "muse"), museRoot).CombinedOutput(); err != nil {
			t.Fatalf("copy muse: %v\n%s", err, out)
		}
	} else if err := os.MkdirAll(museRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "muse")); err == nil {
		if out, err := exec.Command("cp", "-a", filepath.Join(home, ".config", "muse"), museCfg).CombinedOutput(); err != nil {
			t.Fatalf("copy muse config: %v\n%s", err, out)
		}
	} else if err := os.MkdirAll(museCfg, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSE_HOME", museRoot)
	t.Setenv("MUSE_CONFIG", museCfg)

	// --- Phase 1: simulate the folder move /root -> oldCwd ---
	// Claude: rewrite cwd inside transcripts (dir names intentionally stay,
	// mirroring what happens when a project dir is moved behind claude's back).
	claudeProjects := filepath.Join(claudeRoot, "projects")
	dirs, err := os.ReadDir(claudeProjects)
	if err != nil {
		t.Fatal(err)
	}
	claudeSessions := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(claudeProjects, d.Name()))
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".jsonl") {
				p := filepath.Join(claudeProjects, d.Name(), f.Name())
				if err := migrate.RewriteCwdInFile(p, "/root", oldCwd); err != nil {
					t.Fatal(err)
				}
				claudeSessions++
			}
		}
	}
	if claudeSessions == 0 {
		t.Fatal("sandbox has no claude sessions")
	}

	// Codex: rewrite cwd in rollout files.
	codexFiles := 0
	filepath.WalkDir(filepath.Join(codexRoot, "sessions"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") {
			if err := migrate.RewriteCwdInFile(p, "/root", oldCwd); err != nil {
				t.Fatal(err)
			}
			codexFiles++
		}
		return nil
	})
	if codexFiles == 0 {
		t.Fatal("sandbox has no codex rollouts")
	}

	// Grok: rename the encoded project dir, rewrite summary.json, update sqlite.
	grokSessions := filepath.Join(grokRoot, "sessions")
	if err := os.Rename(filepath.Join(grokSessions, url.PathEscape("/root")),
		filepath.Join(grokSessions, url.PathEscape(oldCwd))); err != nil {
		t.Fatalf("grok dir rename: %v", err)
	}
	filepath.WalkDir(grokSessions, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "summary.json" {
			if err := migrate.RewriteCwdInFile(p, "/root", oldCwd); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	db, err := sql.Open("sqlite", filepath.Join(grokSessions, "session_search.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE session_docs SET cwd = ? WHERE cwd = ?`, oldCwd, "/root"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Agy: rewrite summaries + history + projects.json the same way a
	// folder move leaves the on-disk records pointing at a missing path.
	if NewAgy("").Installed() {
		simulateAgyFolderMove(t, oldCwd)
	}
	if NewQwen("").Installed() {
		simulateQwenFolderMove(t, oldCwd)
	}
	if NewMuse("").Installed() {
		simulateMuseFolderMove(t, oldCwd)
	}

	// --- Phase 2: scan the sandbox and verify everything is orphaned ---
	as := Discover()
	if len(as) < 3 {
		t.Fatalf("expected at least 3 agents discovered, got %d", len(as))
	}
	ss, errs := ScanAll(context.Background(), as)
	if len(errs) != 0 {
		t.Fatalf("scan errors: %v", errs)
	}
	byKind := map[model.Kind][]model.Session{}
	for _, s := range ss {
		byKind[s.Kind] = append(byKind[s.Kind], s)
	}
	wantKinds := []model.Kind{model.Claude, model.Codex, model.Grok}
	for _, extra := range []struct {
		ok   bool
		kind model.Kind
	}{
		{NewAgy("").Installed(), model.Agy},
		{NewQwen("").Installed(), model.Qwen},
		{NewMuse("").Installed(), model.Muse},
	} {
		if extra.ok {
			wantKinds = append(wantKinds, extra.kind)
		}
	}
	for _, kind := range wantKinds {
		group := byKind[kind]
		if len(group) == 0 {
			t.Fatalf("no %s sessions in sandbox", kind)
		}
		for _, s := range group {
			if s.Cwd != oldCwd {
				continue // e.g. grok sessions from /root/configs stay untouched
			}
			if !s.Orphan {
				t.Fatalf("%s session %s should be orphaned: %+v", kind, s.ID, s)
			}
		}
	}

	// --- Phase 3: remap every agent oldCwd -> newCwd ---
	backupRoot := filepath.Join(sandbox, "backups")
	for _, a := range as {
		var group []model.Session
		for _, s := range byKind[a.Kind()] {
			if s.Cwd == oldCwd {
				group = append(group, s)
			}
		}
		if len(group) == 0 {
			continue
		}
		plan, err := a.RemapPlan(group, newCwd)
		if err != nil {
			t.Fatalf("%s RemapPlan: %v", a.Kind(), err)
		}
		if err := plan.Validate(); err != nil {
			t.Fatalf("%s Validate: %v", a.Kind(), err)
		}
		if _, err := migrate.Apply(plan, backupRoot); err != nil {
			t.Fatalf("%s Apply: %v", a.Kind(), err)
		}
	}

	// --- Phase 4: rescan and verify health ---
	ss, errs = ScanAll(context.Background(), as)
	if len(errs) != 0 {
		t.Fatalf("post-remap scan errors: %v", errs)
	}
	moved := 0
	for _, s := range ss {
		if s.Cwd == oldCwd {
			t.Fatalf("%s session %s still points at old cwd", s.Kind, s.ID)
		}
		if s.Cwd != newCwd {
			continue
		}
		moved++
		if s.Orphan {
			t.Fatalf("%s session %s orphaned after remap", s.Kind, s.ID)
		}
		if s.Active {
			t.Fatalf("%s session %s unexpectedly active in sandbox", s.Kind, s.ID)
		}
	}
	if moved == 0 {
		t.Fatal("no sessions point at the new cwd after remap")
	}

	// Claude: transcripts must live under the encoded new path.
	encNew := EncodePath(newCwd)
	entries, err := os.ReadDir(filepath.Join(claudeProjects, encNew))
	if err != nil {
		t.Fatalf("claude target dir missing: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("claude target dir is empty")
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue // sidecar dirs and project data are checked below
		}
		b, err := os.ReadFile(filepath.Join(claudeProjects, encNew, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"cwd":"`+newCwd+`"`) {
			t.Fatalf("claude transcript %s not rewritten", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(claudeProjects, "-root")); !os.IsNotExist(err) {
		t.Fatal("old claude project dir should have been removed")
	}
	// Project-scoped data (memory/, session sidecar dirs) must have moved too.
	if entries, err := os.ReadDir(filepath.Join(claudeProjects, encNew)); err == nil {
		hasSideData := false
		for _, e := range entries {
			if e.Name() == "memory" || e.IsDir() {
				hasSideData = true
			}
		}
		if !hasSideData {
			t.Fatal("expected memory/ or session sidecar dirs to move with the project")
		}
	}

	// Grok: encoded dir exists, sqlite rows moved.
	if _, err := os.Stat(filepath.Join(grokSessions, url.PathEscape(newCwd))); err != nil {
		t.Fatalf("grok target dir missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(grokSessions, url.PathEscape(oldCwd))); !os.IsNotExist(err) {
		t.Fatal("old grok encoded dir should be gone")
	}
	db, err = sql.Open("sqlite", filepath.Join(grokSessions, "session_search.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var nOld, nNew int
	db.QueryRow(`SELECT COUNT(*) FROM session_docs WHERE cwd = ?`, oldCwd).Scan(&nOld)
	db.QueryRow(`SELECT COUNT(*) FROM session_docs WHERE cwd = ?`, newCwd).Scan(&nNew)
	if nOld != 0 || nNew == 0 {
		t.Fatalf("sqlite rows: old=%d new=%d", nOld, nNew)
	}

	agyRemapped := false
	for _, s := range byKind[model.Agy] {
		if s.Cwd == oldCwd {
			agyRemapped = true
			break
		}
	}
	if NewAgy("").Installed() && agyRemapped {
		agy := NewAgy("")
		sdb, err := sql.Open("sqlite", agy.summariesPath())
		if err != nil {
			t.Fatal(err)
		}
		var nAgyOld, nAgyNew int
		sdb.QueryRow(`SELECT COUNT(*) FROM conversation_summaries WHERE workspace_uris LIKE ?`,
			`%"file://`+oldCwd+`"%`).Scan(&nAgyOld)
		sdb.QueryRow(`SELECT COUNT(*) FROM conversation_summaries WHERE workspace_uris LIKE ?`,
			`%"file://`+newCwd+`"%`).Scan(&nAgyNew)
		sdb.Close()
		if nAgyOld != 0 || nAgyNew == 0 {
			t.Fatalf("agy workspace_uris: old=%d new=%d", nAgyOld, nAgyNew)
		}
		if b, err := os.ReadFile(agy.projectsPath()); err == nil {
			if strings.Contains(string(b), `"`+oldCwd+`"`) || !strings.Contains(string(b), `"`+newCwd+`"`) {
				t.Fatalf("agy projects.json not remapped: %s", b)
			}
		}
	}

	if NewQwen("").Installed() {
		verifyQwenRemap(t, oldCwd, newCwd)
	}
	if NewMuse("").Installed() {
		verifyMuseRemap(t, oldCwd, newCwd)
	}

	// Backups must exist for every remapped agent.
	backupAgents := []string{"claude", "codex", "grok"}
	if agyRemapped {
		backupAgents = append(backupAgents, "agy")
	}
	for _, kind := range []model.Kind{model.Qwen, model.Muse} {
		for _, s := range byKind[kind] {
			if s.Cwd == oldCwd {
				backupAgents = append(backupAgents, string(kind))
				break
			}
		}
	}
	for _, name := range backupAgents {
		matches, _ := filepath.Glob(filepath.Join(backupRoot, "*-"+name))
		if len(matches) == 0 {
			t.Fatalf("no backup dir for %s under %s", name, backupRoot)
		}
	}
}

// simulateAgyFolderMove rewrites sandbox agy records that pointed at /root
// so they now point at oldCwd (which does not exist), matching a real move.
func simulateAgyFolderMove(t *testing.T, oldCwd string) {
	t.Helper()
	agy := NewAgy("")
	sdb, err := sql.Open("sqlite", agy.summariesPath())
	if err == nil {
		rows, qerr := sdb.Query(`SELECT conversation_id, workspace_uris FROM conversation_summaries`)
		if qerr == nil {
			var ids []string
			for rows.Next() {
				var id, uris string
				if rows.Scan(&id, &uris) != nil {
					continue
				}
				if firstWorkspace(uris) == "/root" {
					ids = append(ids, id)
				}
			}
			rows.Close()
			sdb.Close()
			if len(ids) > 0 {
				plan := &migrate.Plan{Agent: "agy-sim"}
				for _, id := range ids {
					plan.Add(migrate.Action{
						Kind: migrate.SQLiteSetWorkspace, Src: agy.summariesPath(),
						SessionID: id, Old: "/root", New: oldCwd, Desc: "sim " + id,
					})
				}
				if _, err := migrate.Apply(plan, t.TempDir()); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			sdb.Close()
		}
	}
	if err := migrate.RewriteFieldInFile(agy.historyPath(), "workspace", "/root", oldCwd); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	b, err := os.ReadFile(agy.projectsPath())
	if err != nil {
		return
	}
	var doc struct {
		Projects map[string]string `json:"projects"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return
	}
	name, ok := doc.Projects["/root"]
	if !ok {
		return
	}
	delete(doc.Projects, "/root")
	doc.Projects[oldCwd] = name
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agy.projectsPath(), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func simulateQwenFolderMove(t *testing.T, oldCwd string) {
	t.Helper()
	q := NewQwen("")
	_ = filepath.WalkDir(q.projectsDir(), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		switch {
		case strings.HasSuffix(d.Name(), ".jsonl"):
			if err := migrate.RewriteCwdInFile(p, "/root", oldCwd); err != nil {
				t.Fatal(err)
			}
		case strings.HasSuffix(d.Name(), ".runtime.json"):
			if err := migrate.RewriteFieldInFile(p, "work_dir", "/root", oldCwd); err != nil {
				t.Fatal(err)
			}
			// Copied runtime files may still name live host pids.
			if b, err := os.ReadFile(p); err == nil {
				var rt map[string]any
				if json.Unmarshal(b, &rt) == nil {
					rt["pid"] = 0
					if out, err := json.Marshal(rt); err == nil {
						_ = os.WriteFile(p, out, 0o644)
					}
				}
			}
		}
		return nil
	})
}

func simulateMuseFolderMove(t *testing.T, oldCwd string) {
	t.Helper()
	m := NewMuse("")
	_ = filepath.WalkDir(m.sessionsDir(), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "session.jsonl" {
			return nil
		}
		if err := migrate.RewriteFieldInFile(p, "workspace_root", "/root", oldCwd); err != nil {
			t.Fatal(err)
		}
		if err := migrate.RewriteCwdInFile(p, "/root", oldCwd); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	db, err := sql.Open("sqlite", m.indexPath())
	if err == nil {
		_, _ = db.Exec(`UPDATE sessions SET workspace_root = ?, workspace_key = ? WHERE workspace_root = ?`,
			oldCwd, oldCwd, "/root")
		db.Close()
	}
	b, err := os.ReadFile(m.trustPath())
	if err != nil {
		return
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil {
		return
	}
	var projects map[string]json.RawMessage
	if json.Unmarshal(doc["projects"], &projects) != nil {
		return
	}
	val, ok := projects["/root"]
	if !ok {
		return
	}
	delete(projects, "/root")
	projects[oldCwd] = val
	nb, err := json.Marshal(projects)
	if err != nil {
		t.Fatal(err)
	}
	doc["projects"] = nb
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.trustPath(), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func verifyQwenRemap(t *testing.T, oldCwd, newCwd string) {
	t.Helper()
	q := NewQwen("")
	hadOld := false
	ss, _ := q.Scan(context.Background())
	for _, s := range ss {
		if s.Cwd == oldCwd {
			t.Fatalf("qwen session %s still at old cwd", s.ID)
		}
		if s.Cwd == newCwd {
			hadOld = true
			if s.Orphan {
				t.Fatalf("qwen session %s orphaned after remap", s.ID)
			}
		}
	}
	encNew := EncodePath(newCwd)
	if hadOld {
		if _, err := os.Stat(filepath.Join(q.projectsDir(), encNew)); err != nil {
			t.Fatalf("qwen target dir missing: %v", err)
		}
	}
}

func verifyMuseRemap(t *testing.T, oldCwd, newCwd string) {
	t.Helper()
	m := NewMuse("")
	db, err := sql.Open("sqlite", m.indexPath())
	if err != nil {
		return
	}
	defer db.Close()
	var nOld, nNew int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE workspace_root = ?`, oldCwd).Scan(&nOld)
	_ = db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE workspace_root = ?`, newCwd).Scan(&nNew)
	if nOld != 0 {
		t.Fatalf("muse index still has old cwd: %d", nOld)
	}
	// nNew may be 0 if this machine had no /root muse sessions.
	_ = nNew
}
