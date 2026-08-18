package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

// Grok reads ~/.grok/sessions/<url-encoded-cwd>/<session-id>/ directories.
// Each session dir holds chat_history.jsonl, summary.json and friends; the
// session_search.sqlite index mirrors cwd/title per session id.
type Grok struct {
	root    string
	extra   bool
	unbound bool
}

func NewGrok(home string) *Grok {
	root := os.Getenv("GROK_HOME")
	if root == "" {
		root = filepath.Join(home, ".grok")
	}
	return newGrok(root, false)
}

func newGrok(root string, extra bool) *Grok {
	root = absClean(root)
	return &Grok{root: root, extra: extra, unbound: !extra}
}

func (g *Grok) Kind() model.Kind    { return model.Grok }
func (g *Grok) Root() string        { return g.root }
func (g *Grok) Label() string       { return extraLabel(model.Grok, g.extra, g.root) }
func (g *Grok) ClaimsUnbound() bool { return g.unbound }

func (g *Grok) Installed() bool {
	st, err := os.Stat(g.sessionsDir())
	return err == nil && st.IsDir()
}

func (g *Grok) sessionsDir() string { return filepath.Join(g.root, "sessions") }

func (g *Grok) sqlitePath() string { return filepath.Join(g.sessionsDir(), "session_search.sqlite") }

// encodeDir URL-encodes a cwd the way Grok names its per-project dirs
// (/root/configs -> %2Froot%2Fconfigs).
func encodeDir(p string) string { return url.PathEscape(p) }

func decodeDir(name string) string {
	if p, err := url.PathUnescape(name); err == nil {
		return p
	}
	return name
}

type grokActive struct {
	SessionID string `json:"session_id"`
	PID       int    `json:"pid"`
	Cwd       string `json:"cwd"`
}

func (g *Grok) liveActiveSessions() []grokActive {
	data, err := os.ReadFile(filepath.Join(g.root, "active_sessions.json"))
	if err != nil {
		return nil
	}
	var entries []grokActive
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}
	var out []grokActive
	for _, e := range entries {
		if e.SessionID == "" || !PidAlive(e.PID) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// activeSessionIDs reads ~/.grok/active_sessions.json and keeps only
// entries whose pid is still alive. Those must never be remapped/deleted.
func (g *Grok) activeSessionIDs() map[string]bool {
	out := map[string]bool{}
	for _, e := range g.liveActiveSessions() {
		out[e.SessionID] = true
	}
	return out
}

func (g *Grok) guardActive(sessions []model.Session) error {
	active := g.activeSessionIDs() // re-read fresh: activity can change after a scan
	for _, s := range sessions {
		if active[s.ID] {
			return fmt.Errorf("session %s is currently active in a running grok process; exit it first", s.ID)
		}
	}
	return nil
}

type grokSummary struct {
	Info struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	} `json:"info"`
	SessionSummary  string `json:"session_summary"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	LastActiveAt    string `json:"last_active_at"`
	NumMessages     int    `json:"num_messages"`
	NumChatMessages int    `json:"num_chat_messages"`
	CurrentModelID  string `json:"current_model_id"`
}

func (g *Grok) Scan(ctx context.Context) ([]model.Session, error) {
	entries, err := os.ReadDir(g.sessionsDir())
	if err != nil {
		return nil, err
	}
	active := g.activeSessionIDs()
	var out []model.Session
	for _, e := range entries {
		if !e.IsDir() {
			continue // session_search.sqlite etc.
		}
		cwd := decodeDir(e.Name())
		projDir := filepath.Join(g.sessionsDir(), e.Name())
		sessEntries, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}
		for _, se := range sessEntries {
			if !se.IsDir() {
				continue
			}
			s := model.Session{
				Kind:   model.Grok,
				ID:     se.Name(),
				Cwd:    cwd,
				File:   filepath.Join(projDir, se.Name()),
				Active: active[se.Name()],
			}
			if st, err := se.Info(); err == nil {
				s.UpdatedAt = st.ModTime()
			}
			if data, err := os.ReadFile(filepath.Join(s.File, "summary.json")); err == nil {
				var sum grokSummary
				if err := json.Unmarshal(data, &sum); err == nil {
					if sum.Info.Cwd != "" {
						s.Cwd = sum.Info.Cwd
					}
					s.Title = sum.SessionSummary
					s.Model = sum.CurrentModelID
					s.Messages = sum.NumChatMessages
					if t, err := time.Parse(time.RFC3339Nano, sum.CreatedAt); err == nil {
						s.CreatedAt = t
					}
					latest := sum.LastActiveAt
					if latest == "" {
						latest = sum.UpdatedAt
					}
					if t, err := time.Parse(time.RFC3339Nano, latest); err == nil && t.After(s.UpdatedAt) {
						s.UpdatedAt = t
					}
				}
			}
			if du, err := dirUsage(s.File); err == nil {
				s.SizeBytes = du
			}
			out = append(out, s)
		}
	}
	return out, nil
}

func dirUsage(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}

func (g *Grok) RemapPlan(sessions []model.Session, newCwd string) (*migrate.Plan, error) {
	if len(sessions) == 0 {
		return nil, fmt.Errorf("no sessions to remap")
	}
	if err := g.guardActive(sessions); err != nil {
		return nil, err
	}
	oldCwd := sessions[0].Cwd
	plan := &migrate.Plan{Agent: string(model.Grok), OldCwd: oldCwd, NewCwd: newCwd}
	oldEnc, newEnc := encodeDir(oldCwd), encodeDir(newCwd)
	newProjDir := filepath.Join(g.sessionsDir(), newEnc)
	for _, s := range sessions {
		dst := filepath.Join(newProjDir, s.ID)
		plan.Add(migrate.Action{
			Kind: migrate.MoveDir, Src: s.File, Dst: dst,
			Desc: fmt.Sprintf("move session %s to %s/", s.ID, newEnc),
		})
		if _, err := os.Stat(filepath.Join(s.File, "summary.json")); err == nil {
			plan.Add(migrate.Action{
				Kind: migrate.RewriteCwd, Src: filepath.Join(dst, "summary.json"), Old: oldCwd, New: newCwd,
				Desc: fmt.Sprintf("rewrite cwd in %s/summary.json", s.ID),
			})
		}
	}
	if _, err := os.Stat(g.sqlitePath()); err == nil {
		for _, s := range sessions {
			plan.Add(migrate.Action{
				Kind: migrate.SQLiteSetCwd, Src: g.sqlitePath(), SessionID: s.ID, New: newCwd,
				Desc: fmt.Sprintf("update search index for %s", s.ID),
			})
		}
	}

	// Project-scoped leftovers (prompt_history.jsonl etc.) move with the
	// project, same as Claude's memory/ dir.
	oldProjDir := filepath.Join(g.sessionsDir(), oldEnc)
	if entries, err := os.ReadDir(oldProjDir); err == nil {
		existing := map[string]bool{}
		if destEntries, err := os.ReadDir(newProjDir); err == nil {
			for _, e := range destEntries {
				existing[e.Name()] = true
			}
		}
		inGroup := map[string]bool{}
		for _, s := range sessions {
			inGroup[s.ID] = true
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() && inGroup[name] {
				continue // session dir already planned
			}
			if existing[name] {
				return nil, fmt.Errorf("conflict: %s already exists in target project dir %s", name, newEnc)
			}
			src := filepath.Join(oldProjDir, name)
			dst := filepath.Join(newProjDir, name)
			kind, desc := migrate.MoveFile, fmt.Sprintf("move project data %s", name)
			if e.IsDir() {
				kind, desc = migrate.MoveDir, fmt.Sprintf("move project data %s/", name)
			}
			plan.Add(migrate.Action{Kind: kind, Src: src, Dst: dst, Desc: desc})
		}
	}

	if oldEnc != newEnc {
		plan.Add(migrate.Action{
			Kind: migrate.RemoveEmptyDir, Src: oldProjDir,
			Desc: fmt.Sprintf("remove old project dir %s", oldEnc),
		})
	}
	return plan, nil
}

func (g *Grok) RenamePlan(s model.Session, newTitle string) (*migrate.Plan, error) {
	if err := g.guardActive([]model.Session{s}); err != nil {
		return nil, err
	}
	plan, err := newRenamePlan(string(model.Grok), s, newTitle)
	if err != nil {
		return nil, err
	}
	sum := filepath.Join(s.File, "summary.json")
	if _, err := os.Stat(sum); err == nil {
		plan.Add(migrate.Action{
			Kind: migrate.SetJSONKey, Src: sum, Field: "session_summary", New: plan.NewTitle,
			Desc: fmt.Sprintf("set session_summary in %s/summary.json", s.ID),
		})
		plan.Add(migrate.Action{
			Kind: migrate.SetJSONKey, Src: sum, Field: "generated_title", New: plan.NewTitle,
			Desc: fmt.Sprintf("set generated_title in %s/summary.json", s.ID),
		})
	}
	if _, err := os.Stat(g.sqlitePath()); err == nil {
		plan.Add(migrate.Action{
			Kind: migrate.SQLiteSetCwd, Src: g.sqlitePath(), SessionID: s.ID, New: plan.NewTitle,
			SetColumn: "title",
			Desc:      fmt.Sprintf("update search index title for %s", s.ID),
		})
	}
	if plan.Empty() {
		return nil, fmt.Errorf("no title fields found to update")
	}
	return plan, nil
}

func (g *Grok) DeletePlan(sessions []model.Session) (*migrate.Plan, error) {
	if err := g.guardActive(sessions); err != nil {
		return nil, err
	}
	plan := &migrate.Plan{Agent: string(model.Grok)}
	for _, s := range sessions {
		plan.Add(migrate.Action{
			Kind: migrate.Archive, Src: s.File,
			Desc: fmt.Sprintf("archive session %s", s.ID),
		})
	}
	if _, err := os.Stat(g.sqlitePath()); err == nil {
		for _, s := range sessions {
			plan.Add(migrate.Action{
				Kind: migrate.SQLiteDelete, Src: g.sqlitePath(), SessionID: s.ID,
				Desc: fmt.Sprintf("remove %s from search index", s.ID),
			})
		}
	}
	return plan, nil
}

func (g *Grok) ResumeCmd(s model.Session) ([]string, string) {
	bin := filepath.Join(g.root, "bin", "grok")
	if _, err := os.Stat(bin); err != nil {
		bin = "grok"
	}
	return []string{bin, "--resume", s.ID}, s.Cwd
}

func (g *Grok) ResumeEnv() []string { return []string{"GROK_HOME=" + g.root} }

func (g *Grok) ProcessNames() []string { return []string{"grok"} }

func (g *Grok) OwnsProcess(pid int, comm, cmdline string) bool {
	return ownsByEnvOrFD(pid, "GROK_HOME", g.root, g.unbound)
}

func (g *Grok) ActiveMarkers() []LiveActivity {
	var out []LiveActivity
	for _, e := range g.liveActiveSessions() {
		out = append(out, LiveActivity{
			Desc: fmt.Sprintf("active session %s pid %d", e.SessionID, e.PID),
			PID:  e.PID,
		})
	}
	return out
}
