package agents

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"

	_ "modernc.org/sqlite"
)

// Muse reads Meta Muse Code sessions under $XDG_DATA_HOME/muse
// (default ~/.local/share/muse). Session dirs are date-based
// (sessions/YYYY/MM/DD/<uuid>/session.jsonl); the project path lives in
// workspace_root / cwd inside the log and in session-index.db.
type Muse struct {
	data    string
	config  string
	extra   bool
	unbound bool
}

func NewMuse(home string) *Muse {
	data := os.Getenv("MUSE_HOME")
	if data == "" {
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			data = filepath.Join(xdg, "muse")
		} else {
			data = filepath.Join(home, ".local", "share", "muse")
		}
	}
	cfg := os.Getenv("MUSE_CONFIG")
	if cfg == "" {
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			cfg = filepath.Join(xdg, "muse")
		} else {
			cfg = filepath.Join(home, ".config", "muse")
		}
	}
	m := newMuse(data, false)
	m.config = absClean(cfg)
	return m
}

func newMuse(data string, extra bool) *Muse {
	data = absClean(data)
	m := &Muse{
		data:    data,
		extra:   extra,
		unbound: !extra,
	}
	if extra {
		// Don't share the default trust.json with an isolated data dir.
		if _, err := os.Stat(filepath.Join(data, "trust.json")); err == nil {
			m.config = data
		} else {
			m.config = filepath.Join(data, "config")
		}
	}
	return m
}

func (m *Muse) Kind() model.Kind    { return model.Muse }
func (m *Muse) Root() string        { return m.data }
func (m *Muse) Label() string       { return extraLabel(model.Muse, m.extra, m.data) }
func (m *Muse) ClaimsUnbound() bool { return m.unbound }

func (m *Muse) Installed() bool {
	st, err := os.Stat(m.sessionsDir())
	return err == nil && st.IsDir()
}

func (m *Muse) sessionsDir() string { return filepath.Join(m.data, "sessions") }

func (m *Muse) indexPath() string { return filepath.Join(m.data, "session-index.db") }

func (m *Muse) trustPath() string { return filepath.Join(m.config, "trust.json") }

func (m *Muse) Scan(ctx context.Context) ([]model.Session, error) {
	idx := m.loadIndex()
	seen := map[string]bool{}
	var out []model.Session

	_ = filepath.WalkDir(m.sessionsDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == "subagent" {
			return fs.SkipDir
		}
		if d.IsDir() || d.Name() != "session.jsonl" {
			return nil
		}
		dir := filepath.Dir(p)
		id := filepath.Base(dir)
		seen[id] = true
		s := scanMuseJSONL(p, id)
		if row, ok := idx[id]; ok {
			applyMuseIndex(&s, row)
		}
		s.Active = museLockHeld(dir)
		if du, err := dirUsage(dir); err == nil {
			s.SizeBytes = du
		}
		out = append(out, s)
		return nil
	})

	for id, row := range idx {
		if seen[id] {
			continue
		}
		s := model.Session{Kind: model.Muse, ID: id, File: row.Dir}
		applyMuseIndex(&s, row)
		if s.File == "" {
			s.File = row.Log
		}
		s.Active = museLockHeld(row.Dir)
		out = append(out, s)
	}
	return out, nil
}

type museIndexRow struct {
	ID      string
	Dir     string
	Log     string
	Cwd     string
	Title   string
	Name    string
	Model   string
	Prompts int
	Created time.Time
	Updated time.Time
}

func (m *Muse) loadIndex() map[string]museIndexRow {
	out := map[string]museIndexRow{}
	db, err := sql.Open("sqlite", sqliteRO(m.indexPath()))
	if err != nil {
		return out
	}
	defer db.Close()
	rows, err := db.Query(`SELECT session_id, session_dir, session_log_path,
		workspace_root, title, session_name, model_id, prompt_count, created_at_us, updated_at_us
		FROM sessions`)
	if err != nil {
		// Older indexes have no session_name column.
		rows, err = db.Query(`SELECT session_id, session_dir, session_log_path,
			workspace_root, title, '' AS session_name, model_id, prompt_count, created_at_us, updated_at_us
			FROM sessions`)
	}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var r museIndexRow
		var created, updated sql.NullInt64
		var model, name sql.NullString
		if err := rows.Scan(&r.ID, &r.Dir, &r.Log, &r.Cwd, &r.Title, &name, &model, &r.Prompts, &created, &updated); err != nil {
			continue
		}
		r.Name = name.String
		r.Model = model.String
		r.Created = microsTime(created.Int64)
		r.Updated = microsTime(updated.Int64)
		if r.ID != "" {
			out[r.ID] = r
		}
	}
	return out
}

func applyMuseIndex(s *model.Session, r museIndexRow) {
	if s.Cwd == "" {
		s.Cwd = r.Cwd
	}
	if r.Name != "" {
		s.Title = r.Name
	} else if s.Title == "" || s.Title == "New session" && r.Title != "" {
		s.Title = r.Title
	}
	if s.Model == "" {
		s.Model = r.Model
	}
	if s.Messages == 0 {
		s.Messages = r.Prompts
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = r.Created
	}
	if r.Updated.After(s.UpdatedAt) {
		s.UpdatedAt = r.Updated
	}
	if s.File == "" {
		s.File = r.Dir
	}
}

func microsTime(us int64) time.Time {
	if us <= 0 {
		return time.Time{}
	}
	return time.UnixMicro(us)
}

func scanMuseJSONL(path, id string) model.Session {
	s := model.Session{Kind: model.Muse, ID: id, File: filepath.Dir(path)}
	if st, err := os.Stat(path); err == nil {
		s.SizeBytes = st.Size()
		s.UpdatedAt = st.ModTime()
	}
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"cwd"`)) &&
			!bytes.Contains(line, []byte(`"workspace_root"`)) &&
			!bytes.Contains(line, []byte(`"model_id"`)) &&
			!bytes.Contains(line, []byte(`user_intent`)) {
			continue
		}
		var rec struct {
			PayloadType string `json:"payload_type"`
			RecordedAt  int64  `json:"recorded_at"`
			Payload     struct {
				Kind   string `json:"kind"`
				Record struct {
					Cwd           string `json:"cwd"`
					WorkspaceRoot string `json:"workspace_root"`
					ModelID       string `json:"model_id"`
				} `json:"record"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Payload.Record.WorkspaceRoot != "" && s.Cwd == "" {
			s.Cwd = rec.Payload.Record.WorkspaceRoot
		}
		if rec.Payload.Record.Cwd != "" && s.Cwd == "" {
			s.Cwd = rec.Payload.Record.Cwd
		}
		if rec.Payload.Record.ModelID != "" && s.Model == "" {
			s.Model = rec.Payload.Record.ModelID
		}
		if strings.Contains(rec.PayloadType, "user_intent.accepted") {
			s.Messages++
			if s.Title == "" {
				s.Title = musePromptFromLine(line)
			}
		}
		if rec.RecordedAt > 0 {
			t := time.UnixMicro(rec.RecordedAt)
			if s.CreatedAt.IsZero() {
				s.CreatedAt = t
			}
			if t.After(s.UpdatedAt) {
				s.UpdatedAt = t
			}
		}
	}
	return s
}

func musePromptFromLine(line []byte) string {
	var rec struct {
		Payload struct {
			ModelMessages []struct {
				Content []struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"model_messages"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return ""
	}
	for _, msg := range rec.Payload.ModelMessages {
		for _, c := range msg.Content {
			if t := strings.TrimSpace(c.Text); t != "" {
				return truncate(t, 100)
			}
		}
	}
	return ""
}

func museLockPath(sessionDir string) string {
	return filepath.Join(sessionDir, ".session.lock")
}

func museLockHeld(sessionDir string) bool {
	p := museLockPath(sessionDir)
	if flockHeld(p) {
		return true
	}
	// Stale lock files keep a pid= line after the process exits.
	return false
}

func museLockPID(sessionDir string) int {
	b, err := os.ReadFile(museLockPath(sessionDir))
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(b))
	s = strings.TrimPrefix(s, "pid=")
	n, _ := strconv.Atoi(s)
	return n
}

func (m *Muse) RemapPlan(sessions []model.Session, newCwd string) (*migrate.Plan, error) {
	if len(sessions) == 0 {
		return nil, fmt.Errorf("no sessions to remap")
	}
	if err := m.guardActive(sessions); err != nil {
		return nil, err
	}
	oldCwd := sessions[0].Cwd
	plan := &migrate.Plan{Agent: string(model.Muse), OldCwd: oldCwd, NewCwd: newCwd}
	for _, s := range sessions {
		log := filepath.Join(s.File, "session.jsonl")
		if _, err := os.Stat(log); err != nil {
			continue
		}
		plan.Add(migrate.Action{
			Kind: migrate.RewriteCwd, Src: log, Field: "workspace_root",
			Old: oldCwd, New: newCwd,
			Desc: fmt.Sprintf("rewrite workspace_root in %s/session.jsonl", s.ID),
		})
		plan.Add(migrate.Action{
			Kind: migrate.RewriteCwd, Src: log, Field: "cwd",
			Old: oldCwd, New: newCwd,
			Desc: fmt.Sprintf("rewrite cwd in %s/session.jsonl", s.ID),
		})
	}
	if _, err := os.Stat(m.indexPath()); err == nil {
		for _, s := range sessions {
			plan.Add(migrate.Action{
				Kind: migrate.SQLiteSetCwd, Src: m.indexPath(), SessionID: s.ID, New: newCwd,
				Table: "sessions", Column: "session_id", SetColumn: "workspace_root",
				Desc: fmt.Sprintf("update session-index workspace_root for %s", s.ID),
			})
			plan.Add(migrate.Action{
				Kind: migrate.SQLiteSetCwd, Src: m.indexPath(), SessionID: s.ID, New: newCwd,
				Table: "sessions", Column: "session_id", SetColumn: "workspace_key",
				Desc: fmt.Sprintf("update session-index workspace_key for %s", s.ID),
			})
		}
	}
	if _, err := os.Stat(m.trustPath()); err == nil {
		plan.Add(migrate.Action{
			Kind: migrate.ProjectsJSONRemap, Src: m.trustPath(),
			Old: oldCwd, New: newCwd,
			Desc: fmt.Sprintf("remap trust.json %s → %s", oldCwd, newCwd),
		})
	}
	return plan, nil
}

func (m *Muse) RenamePlan(s model.Session, newTitle string) (*migrate.Plan, error) {
	if err := m.guardActive([]model.Session{s}); err != nil {
		return nil, err
	}
	plan, err := newRenamePlan(string(model.Muse), s, newTitle)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(m.indexPath()); err != nil {
		return nil, fmt.Errorf("session-index.db not found; cannot rename")
	}
	plan.Add(migrate.Action{
		Kind: migrate.SQLiteMuseRename, Src: m.indexPath(), SessionID: s.ID, New: plan.NewTitle,
		Table: "sessions", Column: "session_id",
		Desc: fmt.Sprintf("set title and session_name for %s", s.ID),
	})
	return plan, nil
}

func (m *Muse) DeletePlan(sessions []model.Session) (*migrate.Plan, error) {
	if err := m.guardActive(sessions); err != nil {
		return nil, err
	}
	plan := &migrate.Plan{Agent: string(model.Muse)}
	for _, s := range sessions {
		if s.File != "" {
			plan.Add(migrate.Action{
				Kind: migrate.Archive, Src: s.File,
				Desc: fmt.Sprintf("archive session %s", s.ID),
			})
		}
	}
	if _, err := os.Stat(m.indexPath()); err == nil {
		for _, s := range sessions {
			plan.Add(migrate.Action{
				Kind: migrate.SQLiteDelete, Src: m.indexPath(), SessionID: s.ID,
				Table: "sessions", Column: "session_id",
				Desc: fmt.Sprintf("remove %s from session-index", s.ID),
			})
		}
	}
	return plan, nil
}

func (m *Muse) ResumeCmd(s model.Session) ([]string, string) {
	return []string{"muse", "resume", s.ID}, s.Cwd
}

func (m *Muse) ResumeEnv() []string {
	env := []string{"MUSE_HOME=" + m.data}
	if m.config != "" {
		env = append(env, "MUSE_CONFIG="+m.config)
	}
	return env
}

func (m *Muse) ProcessNames() []string { return []string{"muse", "muse-bin-*"} }

func (m *Muse) OwnsProcess(pid int, comm, cmdline string) bool {
	if dir, ok := procEnvValue(pid, "MUSE_HOME"); ok && strings.TrimSpace(dir) != "" {
		return SamePath(dir, m.data)
	}
	if xdg, ok := procEnvValue(pid, "XDG_DATA_HOME"); ok && strings.TrimSpace(xdg) != "" {
		return SamePath(filepath.Join(xdg, "muse"), m.data)
	}
	if procTouchesDir(pid, m.data) {
		return true
	}
	return m.unbound
}

func (m *Muse) ActiveMarkers() []LiveActivity {
	var out []LiveActivity
	_ = filepath.WalkDir(m.sessionsDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == "subagent" {
			return fs.SkipDir
		}
		if d.IsDir() || d.Name() != ".session.lock" {
			return nil
		}
		dir := filepath.Dir(p)
		if filepath.Base(filepath.Dir(dir)) == "subagent" {
			return nil
		}
		if !flockHeld(p) {
			return nil
		}
		id := filepath.Base(dir)
		pid := 0
		if holders := flockHolderPIDs(p); len(holders) > 0 {
			pid = holders[0]
		} else {
			pid = museLockPID(dir)
		}
		desc := fmt.Sprintf("session lock %s held", id)
		if pid > 0 {
			desc += fmt.Sprintf(" by pid %d", pid)
		}
		out = append(out, LiveActivity{Desc: desc, PID: pid})
		return nil
	})
	return out
}

func (m *Muse) guardActive(sessions []model.Session) error {
	for _, s := range sessions {
		if museLockHeld(s.File) {
			return fmt.Errorf("session %s is currently live in a running muse process; exit it first", s.ID)
		}
	}
	return nil
}
