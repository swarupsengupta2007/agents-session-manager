package agents

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

// Qwen reads ~/.qwen/projects/<sanitized-cwd>/chats/<uuid>.jsonl transcripts.
// Path encoding is the same as Claude Code (every non-alphanumeric → "-").
// Per-session .runtime.json sidecars record a pid used as the live-session marker.
type Qwen struct {
	root    string
	extra   bool
	unbound bool
}

func NewQwen(home string) *Qwen {
	root := os.Getenv("QWEN_HOME")
	if root == "" {
		root = filepath.Join(home, ".qwen")
	}
	return newQwen(root, false)
}

func newQwen(root string, extra bool) *Qwen {
	root = absClean(root)
	return &Qwen{root: root, extra: extra, unbound: !extra}
}

func (q *Qwen) Kind() model.Kind    { return model.Qwen }
func (q *Qwen) Root() string        { return q.root }
func (q *Qwen) Label() string       { return extraLabel(model.Qwen, q.extra, q.root) }
func (q *Qwen) ClaimsUnbound() bool { return q.unbound }

func (q *Qwen) Installed() bool {
	st, err := os.Stat(q.projectsDir())
	return err == nil && st.IsDir()
}

func (q *Qwen) projectsDir() string { return filepath.Join(q.root, "projects") }

func (q *Qwen) Scan(ctx context.Context) ([]model.Session, error) {
	entries, err := os.ReadDir(q.projectsDir())
	if err != nil {
		return nil, err
	}
	type files struct{ jsonl, runtime string }
	byID := map[string]files{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		chatDir := filepath.Join(q.projectsDir(), e.Name(), "chats")
		chats, err := os.ReadDir(chatDir)
		if err != nil {
			continue
		}
		for _, f := range chats {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			path := filepath.Join(chatDir, name)
			switch {
			case strings.HasSuffix(name, ".jsonl"):
				id := strings.TrimSuffix(name, ".jsonl")
				ent := byID[id]
				ent.jsonl = path
				byID[id] = ent
			case strings.HasSuffix(name, ".runtime.json"):
				id := strings.TrimSuffix(name, ".runtime.json")
				ent := byID[id]
				ent.runtime = path
				byID[id] = ent
			}
		}
	}

	var out []model.Session
	for id, files := range byID {
		s := model.Session{Kind: model.Qwen, ID: id, File: files.jsonl}
		if s.File == "" {
			s.File = files.runtime
		}
		if files.jsonl != "" {
			parsed, err := scanQwenJSONL(files.jsonl)
			if err == nil {
				s = parsed
			}
		}
		if files.runtime != "" {
			applyQwenRuntime(&s, files.runtime)
		}
		out = append(out, s)
	}
	return out, nil
}

func scanQwenJSONL(path string) (model.Session, error) {
	s := model.Session{Kind: model.Qwen, File: path}
	s.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if st, err := os.Stat(path); err == nil {
		s.SizeBytes = st.Size()
		s.UpdatedAt = st.ModTime()
	}
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"cwd"`)) &&
			!bytes.Contains(line, []byte(`"type"`)) {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		typ, _ := rec["type"].(string)
		switch typ {
		case "user", "assistant":
			s.Messages++
			if typ == "user" && s.Title == "" {
				s.Title = qwenUserTitle(rec)
			}
			if typ == "assistant" && s.Model == "" {
				if m, ok := rec["model"].(string); ok {
					s.Model = m
				}
			}
		}
		if s.Cwd == "" {
			if cwd, ok := rec["cwd"].(string); ok && cwd != "" {
				s.Cwd = cwd
			}
		}
		if ts, ok := rec["timestamp"].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				if s.CreatedAt.IsZero() {
					s.CreatedAt = t
				}
				if t.After(s.UpdatedAt) {
					s.UpdatedAt = t
				}
			}
		}
	}
	return s, sc.Err()
}

func qwenUserTitle(rec map[string]any) string {
	msg, _ := rec["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	parts, _ := msg["parts"].([]any)
	var b strings.Builder
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if t, ok := pm["text"].(string); ok {
			b.WriteString(t)
		}
	}
	return truncate(strings.TrimSpace(b.String()), 100)
}

type qwenRuntime struct {
	PID       int     `json:"pid"`
	SessionID string  `json:"session_id"`
	WorkDir   string  `json:"work_dir"`
	StartedAt float64 `json:"started_at"`
	Title     string  `json:"title"`
}

func readQwenRuntime(path string) (qwenRuntime, error) {
	var rt qwenRuntime
	b, err := os.ReadFile(path)
	if err != nil {
		return rt, err
	}
	err = json.Unmarshal(b, &rt)
	return rt, err
}

func applyQwenRuntime(s *model.Session, path string) {
	rt, err := readQwenRuntime(path)
	if err != nil {
		return
	}
	if s.Cwd == "" {
		s.Cwd = rt.WorkDir
	}
	if s.ID == "" {
		s.ID = rt.SessionID
	}
	if rt.Title != "" {
		s.Title = rt.Title
	}
	if PidAlive(rt.PID) {
		s.Active = true
	}
	if st, err := os.Stat(path); err == nil {
		s.SizeBytes += st.Size()
		if st.ModTime().After(s.UpdatedAt) {
			s.UpdatedAt = st.ModTime()
		}
	}
}

func (q *Qwen) RemapPlan(sessions []model.Session, newCwd string) (*migrate.Plan, error) {
	if len(sessions) == 0 {
		return nil, fmt.Errorf("no sessions to remap")
	}
	if err := q.guardActive(sessions); err != nil {
		return nil, err
	}
	oldCwd := sessions[0].Cwd
	oldDir := q.projectDirOf(sessions[0])
	newDir := filepath.Join(q.projectsDir(), EncodePath(newCwd))
	plan := &migrate.Plan{Agent: string(model.Qwen), OldCwd: oldCwd, NewCwd: newCwd}

	planned := map[string]bool{}
	for _, s := range sessions {
		jsonl := filepath.Join(oldDir, "chats", s.ID+".jsonl")
		runtime := filepath.Join(oldDir, "chats", s.ID+".runtime.json")
		if _, err := os.Stat(jsonl); err == nil {
			plan.Add(migrate.Action{
				Kind: migrate.RewriteCwd, Src: jsonl, Old: oldCwd, New: newCwd,
				Desc: fmt.Sprintf("rewrite cwd in %s.jsonl", s.ID),
			})
			plan.Add(migrate.Action{
				Kind: migrate.MoveFile, Src: jsonl, Dst: filepath.Join(newDir, "chats", s.ID+".jsonl"),
				Desc: fmt.Sprintf("move %s.jsonl to %s/chats/", s.ID, EncodePath(newCwd)),
			})
			planned[s.ID+".jsonl"] = true
		}
		if _, err := os.Stat(runtime); err == nil {
			plan.Add(migrate.Action{
				Kind: migrate.RewriteCwd, Src: runtime, Field: "work_dir", Old: oldCwd, New: newCwd,
				Desc: fmt.Sprintf("rewrite work_dir in %s.runtime.json", s.ID),
			})
			plan.Add(migrate.Action{
				Kind: migrate.MoveFile, Src: runtime, Dst: filepath.Join(newDir, "chats", s.ID+".runtime.json"),
				Desc: fmt.Sprintf("move %s.runtime.json to %s/chats/", s.ID, EncodePath(newCwd)),
			})
			planned[s.ID+".runtime.json"] = true
		}
	}

	if entries, err := os.ReadDir(oldDir); err == nil {
		existing := map[string]bool{}
		if destEntries, err := os.ReadDir(newDir); err == nil {
			for _, e := range destEntries {
				existing[e.Name()] = true
			}
		}
		for _, e := range entries {
			name := e.Name()
			if name == "chats" && e.IsDir() {
				if err := q.planChatLeftovers(plan, oldDir, newDir, planned); err != nil {
					return nil, err
				}
				continue
			}
			if existing[name] {
				return nil, fmt.Errorf("conflict: %s already exists in target project dir %s", name, EncodePath(newCwd))
			}
			src := filepath.Join(oldDir, name)
			dst := filepath.Join(newDir, name)
			kind, desc := migrate.MoveFile, fmt.Sprintf("move project data %s", name)
			if e.IsDir() {
				kind, desc = migrate.MoveDir, fmt.Sprintf("move project data %s/", name)
			}
			plan.Add(migrate.Action{Kind: kind, Src: src, Dst: dst, Desc: desc})
		}
	}

	chatsDir := filepath.Join(oldDir, "chats")
	if oldDir != newDir {
		plan.Add(migrate.Action{
			Kind: migrate.RemoveEmptyDir, Src: chatsDir,
			Desc: fmt.Sprintf("remove old chats dir in %s", filepath.Base(oldDir)),
		})
		plan.Add(migrate.Action{
			Kind: migrate.RemoveEmptyDir, Src: oldDir,
			Desc: fmt.Sprintf("remove old project dir %s", filepath.Base(oldDir)),
		})
	}
	return plan, nil
}

func (q *Qwen) planChatLeftovers(plan *migrate.Plan, oldDir, newDir string, planned map[string]bool) error {
	chatDir := filepath.Join(oldDir, "chats")
	entries, err := os.ReadDir(chatDir)
	if err != nil {
		return nil
	}
	destChat := filepath.Join(newDir, "chats")
	existing := map[string]bool{}
	if destEntries, err := os.ReadDir(destChat); err == nil {
		for _, e := range destEntries {
			existing[e.Name()] = true
		}
	}
	for _, e := range entries {
		name := e.Name()
		if planned[name] {
			continue
		}
		if existing[name] {
			return fmt.Errorf("conflict: chats/%s already exists in target project dir", name)
		}
		src := filepath.Join(chatDir, name)
		dst := filepath.Join(destChat, name)
		kind, desc := migrate.MoveFile, fmt.Sprintf("move project data chats/%s", name)
		if e.IsDir() {
			kind, desc = migrate.MoveDir, fmt.Sprintf("move project data chats/%s/", name)
		}
		plan.Add(migrate.Action{Kind: kind, Src: src, Dst: dst, Desc: desc})
	}
	return nil
}

func (q *Qwen) projectDirOf(s model.Session) string {
	d := filepath.Dir(s.File)
	if filepath.Base(d) == "chats" {
		return filepath.Dir(d)
	}
	return filepath.Join(q.projectsDir(), EncodePath(s.Cwd))
}

func (q *Qwen) RenamePlan(s model.Session, newTitle string) (*migrate.Plan, error) {
	if err := q.guardActive([]model.Session{s}); err != nil {
		return nil, err
	}
	plan, err := newRenamePlan(string(model.Qwen), s, newTitle)
	if err != nil {
		return nil, err
	}
	rt := filepath.Join(q.projectDirOf(s), "chats", s.ID+".runtime.json")
	if _, err := os.Stat(rt); err == nil {
		plan.Add(migrate.Action{
			Kind: migrate.SetJSONKey, Src: rt, Field: "title", New: plan.NewTitle,
			Desc: fmt.Sprintf("set title in %s.runtime.json", s.ID),
		})
	} else {
		body, _ := json.Marshal(map[string]any{"session_id": s.ID, "work_dir": s.Cwd, "title": plan.NewTitle})
		plan.Add(migrate.Action{
			Kind: migrate.WriteFile, Src: rt, New: string(body) + "\n",
			Desc: fmt.Sprintf("write %s.runtime.json title", s.ID),
		})
	}
	return plan, nil
}

func (q *Qwen) DeletePlan(sessions []model.Session) (*migrate.Plan, error) {
	if err := q.guardActive(sessions); err != nil {
		return nil, err
	}
	plan := &migrate.Plan{Agent: string(model.Qwen)}
	for _, s := range sessions {
		proj := q.projectDirOf(s)
		jsonl := filepath.Join(proj, "chats", s.ID+".jsonl")
		runtime := filepath.Join(proj, "chats", s.ID+".runtime.json")
		if _, err := os.Stat(jsonl); err == nil {
			plan.Add(migrate.Action{Kind: migrate.Archive, Src: jsonl, Desc: fmt.Sprintf("archive %s.jsonl", s.ID)})
		}
		if _, err := os.Stat(runtime); err == nil {
			plan.Add(migrate.Action{Kind: migrate.Archive, Src: runtime, Desc: fmt.Sprintf("archive %s.runtime.json", s.ID)})
		}
	}
	return plan, nil
}

func (q *Qwen) ResumeCmd(s model.Session) ([]string, string) {
	return []string{"qwen", "--resume", s.ID}, s.Cwd
}

func (q *Qwen) ResumeEnv() []string { return []string{"QWEN_HOME=" + q.root} }

func (q *Qwen) ProcessNames() []string { return []string{"qwen"} }

func (q *Qwen) CmdlineHints() []string { return []string{"qwen-code"} }

func (q *Qwen) OwnsProcess(pid int, comm, cmdline string) bool {
	return ownsByEnvOrFD(pid, "QWEN_HOME", q.root, q.unbound)
}

func (q *Qwen) ActiveMarkers() []LiveActivity {
	var out []LiveActivity
	seen := map[int]bool{}
	_ = filepath.WalkDir(q.projectsDir(), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".runtime.json") {
			return nil
		}
		rt, err := readQwenRuntime(p)
		if err != nil || !PidAlive(rt.PID) || seen[rt.PID] {
			return nil
		}
		seen[rt.PID] = true
		id := rt.SessionID
		if id == "" {
			id = strings.TrimSuffix(d.Name(), ".runtime.json")
		}
		out = append(out, LiveActivity{
			Desc: fmt.Sprintf("runtime pid %d session %s", rt.PID, id),
			PID:  rt.PID,
		})
		return nil
	})
	return out
}

func (q *Qwen) guardActive(sessions []model.Session) error {
	for _, s := range sessions {
		rtPath := filepath.Join(q.projectDirOf(s), "chats", s.ID+".runtime.json")
		rt, err := readQwenRuntime(rtPath)
		if err != nil {
			continue
		}
		if PidAlive(rt.PID) {
			return fmt.Errorf("session %s is currently active in a running qwen process (pid %d); exit it first", s.ID, rt.PID)
		}
	}
	return nil
}
