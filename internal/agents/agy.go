package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"

	_ "modernc.org/sqlite"
)

// Agy reads Google Antigravity CLI (agy) storage under ~/.gemini.
// Conversations live as one sqlite file each in antigravity-cli/conversations/;
// the project path used for resume listing is workspace_uris in
// conversation_summaries.db plus ~/.gemini/projects.json. Conversation
// files themselves are keyed by UUID, so a remap does not move them.
type Agy struct {
	gemini string
}

func NewAgy(home string) *Agy {
	root := os.Getenv("GEMINI_HOME")
	if root == "" {
		root = filepath.Join(home, ".gemini")
	}
	return &Agy{gemini: root}
}

func (a *Agy) Kind() model.Kind { return model.Agy }
func (a *Agy) Root() string     { return a.cliDir() }

func (a *Agy) cliDir() string { return filepath.Join(a.gemini, "antigravity-cli") }

func (a *Agy) conversationsDir() string {
	return filepath.Join(a.cliDir(), "conversations")
}

func (a *Agy) summariesPath() string {
	return filepath.Join(a.cliDir(), "conversation_summaries.db")
}

func (a *Agy) historyPath() string { return filepath.Join(a.cliDir(), "history.jsonl") }

func (a *Agy) projectsPath() string { return filepath.Join(a.gemini, "projects.json") }

func (a *Agy) presenceDir() string { return filepath.Join(a.cliDir(), "presence") }

func (a *Agy) brainDir(id string) string {
	return filepath.Join(a.cliDir(), "brain", id)
}

func (a *Agy) convDB(id string) string {
	return filepath.Join(a.conversationsDir(), id+".db")
}

func (a *Agy) presenceLock(id string) string {
	return filepath.Join(a.presenceDir(), id+".lock")
}

func (a *Agy) Installed() bool {
	if st, err := os.Stat(a.conversationsDir()); err == nil && st.IsDir() {
		return true
	}
	if _, err := os.Stat(a.summariesPath()); err == nil {
		return true
	}
	return false
}

type agySummary struct {
	Title        string
	Preview      string
	StepCount    int
	Modified     time.Time
	Workspace    string
	WorkspaceRaw string
}

type agyHist struct {
	Display string
	Cwd     string
	Updated time.Time
}

func (a *Agy) Scan(ctx context.Context) ([]model.Session, error) {
	summaries := a.loadSummaries()
	history := a.loadHistory()
	held := a.heldPresence()

	seen := map[string]bool{}
	var out []model.Session

	if ents, err := os.ReadDir(a.conversationsDir()); err == nil {
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".db") {
				continue
			}
			// WAL/SHM sidecars if they ever appear.
			if strings.Contains(name, "-wal") || strings.Contains(name, "-shm") {
				continue
			}
			id := strings.TrimSuffix(name, ".db")
			seen[id] = true
			out = append(out, a.buildSession(id, a.convDB(id), summaries[id], history[id], held[id]))
		}
	}

	// Summaries without a conversation db (tolerate both directions).
	for id, sum := range summaries {
		if seen[id] {
			continue
		}
		out = append(out, a.buildSession(id, a.summariesPath(), sum, history[id], held[id]))
	}
	return out, nil
}

func (a *Agy) buildSession(id, file string, sum agySummary, hist agyHist, active bool) model.Session {
	s := model.Session{Kind: model.Agy, ID: id, File: file, Active: active}
	if st, err := os.Stat(file); err == nil {
		s.SizeBytes = st.Size()
		s.UpdatedAt = st.ModTime()
	}
	s.Cwd = sum.Workspace
	if s.Cwd == "" {
		s.Cwd = hist.Cwd
	}
	s.Title = sum.Title
	if s.Title == "" {
		s.Title = sum.Preview
	}
	if s.Title == "" {
		s.Title = hist.Display
	}
	s.Messages = sum.StepCount
	if s.Messages == 0 {
		if n, err := countSteps(file); err == nil {
			s.Messages = n
		}
	}
	if !sum.Modified.IsZero() {
		s.UpdatedAt = sum.Modified
	} else if !hist.Updated.IsZero() && hist.Updated.After(s.UpdatedAt) {
		s.UpdatedAt = hist.Updated
	}
	if du, err := dirUsage(a.brainDir(id)); err == nil {
		s.SizeBytes += du
	}
	return s
}

func (a *Agy) loadSummaries() map[string]agySummary {
	out := map[string]agySummary{}
	db, err := sql.Open("sqlite", sqliteRO(a.summariesPath()))
	if err != nil {
		return out
	}
	defer db.Close()
	rows, err := db.Query(`SELECT conversation_id, title, preview, step_count, last_modified_time, workspace_uris
		FROM conversation_summaries`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, preview, modified, uris string
		var steps int
		if err := rows.Scan(&id, &title, &preview, &steps, &modified, &uris); err != nil {
			continue
		}
		if id == "" {
			continue
		}
		out[id] = agySummary{
			Title:        title,
			Preview:      preview,
			StepCount:    steps,
			Modified:     parseAgyTime(modified),
			Workspace:    firstWorkspace(uris),
			WorkspaceRaw: uris,
		}
	}
	return out
}

func (a *Agy) loadHistory() map[string]agyHist {
	out := map[string]agyHist{}
	data, err := os.ReadFile(a.historyPath())
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec struct {
			Display        string `json:"display"`
			Workspace      string `json:"workspace"`
			ConversationID string `json:"conversationId"`
			Timestamp      int64  `json:"timestamp"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || rec.ConversationID == "" {
			continue
		}
		h := out[rec.ConversationID]
		if rec.Workspace != "" {
			h.Cwd = rec.Workspace
		}
		if rec.Display != "" && !strings.HasPrefix(rec.Display, "/") {
			h.Display = rec.Display
		}
		if rec.Timestamp > 0 {
			// history timestamps are unix millis.
			t := time.UnixMilli(rec.Timestamp)
			if t.After(h.Updated) {
				h.Updated = t
			}
		}
		out[rec.ConversationID] = h
	}
	return out
}

func (a *Agy) heldPresence() map[string]bool {
	out := map[string]bool{}
	ents, err := os.ReadDir(a.presenceDir())
	if err != nil {
		return out
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".lock") {
			continue
		}
		if flockHeld(filepath.Join(a.presenceDir(), name)) {
			out[strings.TrimSuffix(name, ".lock")] = true
		}
	}
	return out
}

func firstWorkspace(urisJSON string) string {
	if urisJSON == "" {
		return ""
	}
	var uris []string
	if json.Unmarshal([]byte(urisJSON), &uris) != nil {
		return ""
	}
	for _, u := range uris {
		if p := fileURIPath(u); p != "" {
			return p
		}
	}
	return ""
}

func fileURIPath(u string) string {
	u = strings.TrimSpace(u)
	if !strings.HasPrefix(u, "file://") {
		return ""
	}
	rest := strings.TrimPrefix(u, "file://")
	if rest == "" || rest[0] != '/' {
		return ""
	}
	return rest
}

func parseAgyTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05Z07:00",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			if t.Year() < 2 {
				return time.Time{}
			}
			return t
		}
	}
	return time.Time{}
}

func sqliteRO(path string) string {
	return "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(2000)"
}

func countSteps(dbPath string) (int, error) {
	db, err := sql.Open("sqlite", sqliteRO(dbPath))
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM steps`).Scan(&n)
	return n, err
}

func (a *Agy) RemapPlan(sessions []model.Session, newCwd string) (*migrate.Plan, error) {
	if len(sessions) == 0 {
		return nil, fmt.Errorf("no sessions to remap")
	}
	if err := a.guardActive(sessions); err != nil {
		return nil, err
	}
	oldCwd := sessions[0].Cwd
	plan := &migrate.Plan{Agent: string(model.Agy), OldCwd: oldCwd, NewCwd: newCwd}

	if _, err := os.Stat(a.summariesPath()); err == nil {
		for _, s := range sessions {
			plan.Add(migrate.Action{
				Kind: migrate.SQLiteSetWorkspace, Src: a.summariesPath(),
				SessionID: s.ID, Old: oldCwd, New: newCwd,
				Desc: fmt.Sprintf("update workspace_uris for %s", s.ID),
			})
		}
	}
	if _, err := os.Stat(a.historyPath()); err == nil {
		plan.Add(migrate.Action{
			Kind: migrate.RewriteCwd, Src: a.historyPath(), Field: "workspace",
			Old: oldCwd, New: newCwd,
			Desc: "rewrite workspace in history.jsonl",
		})
	}
	if _, err := os.Stat(a.projectsPath()); err == nil {
		plan.Add(migrate.Action{
			Kind: migrate.ProjectsJSONRemap, Src: a.projectsPath(),
			Old: oldCwd, New: newCwd,
			Desc: fmt.Sprintf("remap projects.json %s → %s", oldCwd, newCwd),
		})
	}
	return plan, nil
}

func (a *Agy) RenamePlan(s model.Session, newTitle string) (*migrate.Plan, error) {
	if err := a.guardActive([]model.Session{s}); err != nil {
		return nil, err
	}
	plan, err := newRenamePlan(string(model.Agy), s, newTitle)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(a.summariesPath()); err == nil {
		plan.Add(migrate.Action{
			Kind: migrate.SQLiteSetCwd, Src: a.summariesPath(), SessionID: s.ID, New: plan.NewTitle,
			Table: "conversation_summaries", Column: "conversation_id", SetColumn: "title",
			Desc: fmt.Sprintf("set title for conversation %s", s.ID),
		})
	}
	if _, err := os.Stat(a.historyPath()); err == nil {
		line, _ := json.Marshal(map[string]any{
			"display": plan.NewTitle, "workspace": s.Cwd,
			"conversationId": s.ID, "timestamp": time.Now().UnixMilli(),
		})
		plan.Add(migrate.Action{
			Kind: migrate.AppendJSONL, Src: a.historyPath(), New: string(line),
			Desc: fmt.Sprintf("append history display for %s", s.ID),
		})
	}
	if plan.Empty() {
		return nil, fmt.Errorf("conversation_summaries.db and history.jsonl not found; cannot rename")
	}
	return plan, nil
}

func (a *Agy) DeletePlan(sessions []model.Session) (*migrate.Plan, error) {
	if err := a.guardActive(sessions); err != nil {
		return nil, err
	}
	plan := &migrate.Plan{Agent: string(model.Agy)}
	for _, s := range sessions {
		db := a.convDB(s.ID)
		if _, err := os.Stat(db); err == nil {
			plan.Add(migrate.Action{
				Kind: migrate.Archive, Src: db,
				Desc: fmt.Sprintf("archive conversation %s", s.ID),
			})
		}
		brain := a.brainDir(s.ID)
		if st, err := os.Stat(brain); err == nil && st.IsDir() {
			plan.Add(migrate.Action{
				Kind: migrate.Archive, Src: brain,
				Desc: fmt.Sprintf("archive brain/%s", s.ID),
			})
		}
		lock := a.presenceLock(s.ID)
		if _, err := os.Stat(lock); err == nil {
			plan.Add(migrate.Action{
				Kind: migrate.Archive, Src: lock,
				Desc: fmt.Sprintf("archive presence lock %s", s.ID),
			})
		}
	}
	if _, err := os.Stat(a.summariesPath()); err == nil {
		for _, s := range sessions {
			plan.Add(migrate.Action{
				Kind: migrate.SQLiteDelete, Src: a.summariesPath(), SessionID: s.ID,
				Table: "conversation_summaries", Column: "conversation_id",
				Desc: fmt.Sprintf("remove %s from conversation_summaries", s.ID),
			})
		}
	}
	return plan, nil
}

func (a *Agy) ResumeCmd(s model.Session) ([]string, string) {
	return []string{"agy", "--conversation", s.ID}, s.Cwd
}

func (a *Agy) ProcessNames() []string { return []string{"agy"} }

func (a *Agy) ActiveMarkers() []LiveActivity {
	var out []LiveActivity
	ents, err := os.ReadDir(a.presenceDir())
	if err != nil {
		return out
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".lock") {
			continue
		}
		path := filepath.Join(a.presenceDir(), name)
		if !flockHeld(path) {
			continue
		}
		id := strings.TrimSuffix(name, ".lock")
		pids := flockHolderPIDs(path)
		pid := 0
		if len(pids) > 0 {
			pid = pids[0]
		}
		desc := fmt.Sprintf("presence lock %s held", id)
		if pid > 0 {
			desc += fmt.Sprintf(" by pid %d", pid)
		}
		out = append(out, LiveActivity{Desc: desc, PID: pid})
	}
	return out
}

func (a *Agy) guardActive(sessions []model.Session) error {
	held := a.heldPresence()
	for _, s := range sessions {
		if held[s.ID] {
			return fmt.Errorf("conversation %s is currently live in a running agy process; exit it first", s.ID)
		}
	}
	return nil
}
