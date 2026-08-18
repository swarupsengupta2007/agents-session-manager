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

// Claude reads <config-dir>/projects/<sanitized-cwd>/<uuid>.jsonl transcripts.
// The default config dir is ~/.claude (or $CLAUDE_CONFIG_DIR). Extra
// instances can be registered for other CONFIG_DIR roots.
type Claude struct {
	root    string
	extra   bool // user-added / non-default store
	unbound bool // claims claude processes with no explicit CONFIG_DIR binding
}

func NewClaude(home string) *Claude {
	return newClaude(defaultClaudeRoot(home), false)
}

// NewClaudeAt builds a Claude adapter for an explicit CONFIG_DIR.
func NewClaudeAt(root string) *Claude {
	return newClaude(root, true)
}

func newClaude(root string, extra bool) *Claude {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	root = filepath.Clean(root)
	return &Claude{root: root, extra: extra, unbound: !extra}
}

func (c *Claude) Kind() model.Kind { return model.Claude }
func (c *Claude) Root() string     { return c.root }

func (c *Claude) Label() string { return extraLabel(model.Claude, c.extra, c.root) }

func (c *Claude) Installed() bool {
	st, err := os.Stat(c.projectsDir())
	return err == nil && st.IsDir()
}

func (c *Claude) ClaimsUnbound() bool { return c.unbound }

func (c *Claude) projectsDir() string { return filepath.Join(c.root, "projects") }

// EncodePath sanitizes an absolute path the way Claude Code and Qwen Code
// name their project directories: every non-alphanumeric character becomes
// "-". Verified against real Claude output (/tmp/enc.probe_x -> -tmp-enc-probe-x)
// and real Qwen dirs (/root/configs -> -root-configs).
func EncodePath(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

func (c *Claude) Scan(ctx context.Context) ([]model.Session, error) {
	entries, err := os.ReadDir(c.projectsDir())
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(c.projectsDir(), e.Name())
		fsys, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range fsys {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".jsonl") {
				files = append(files, filepath.Join(dir, f.Name()))
			}
		}
	}

	type result struct {
		sess model.Session
		err  error
	}
	jobs := make(chan string)
	results := make(chan result, len(files))
	go func() {
		for _, f := range files {
			jobs <- f
		}
		close(jobs)
	}()
	workers := 4
	for w := 0; w < workers; w++ {
		go func() {
			for f := range jobs {
				s, err := scanClaudeSession(f)
				results <- result{s, err}
			}
		}()
	}

	var out []model.Session
	var firstErr error
	for range files {
		r := <-results
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		out = append(out, r.sess)
	}
	return out, firstErr
}

func scanClaudeSession(path string) (model.Session, error) {
	s := model.Session{Kind: model.Claude, File: path}
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
		// Cheap pre-filters: skip JSON parsing for lines that cannot
		// contribute anything we track.
		if !bytes.Contains(line, []byte(`"cwd"`)) &&
			!bytes.Contains(line, []byte(`"ai-title"`)) &&
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
			if typ == "assistant" && s.Model == "" {
				s.Model = claudeModelOf(rec)
			}
		case "ai-title":
			if t, ok := rec["aiTitle"].(string); ok && t != "" {
				s.Title = t
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
	if err := sc.Err(); err != nil && s.ID == "" {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func claudeModelOf(rec map[string]any) string {
	if m, ok := rec["model"].(string); ok && m != "" {
		return m
	}
	if msg, ok := rec["message"].(map[string]any); ok {
		if m, ok := msg["model"].(string); ok && m != "" {
			return m
		}
	}
	return ""
}

func (c *Claude) RemapPlan(sessions []model.Session, newCwd string) (*migrate.Plan, error) {
	if len(sessions) == 0 {
		return nil, fmt.Errorf("no sessions to remap")
	}
	oldCwd := sessions[0].Cwd
	oldDir := filepath.Dir(sessions[0].File)
	plan := &migrate.Plan{Agent: string(model.Claude), OldCwd: oldCwd, NewCwd: newCwd}
	newDir := filepath.Join(c.projectsDir(), EncodePath(newCwd))

	inGroup := map[string]bool{}
	for _, s := range sessions {
		inGroup[s.ID] = true
		src := s.File
		dst := filepath.Join(newDir, filepath.Base(src))
		plan.Add(migrate.Action{
			Kind: migrate.RewriteCwd, Src: src, Old: oldCwd, New: newCwd,
			Desc: fmt.Sprintf("rewrite cwd in %s", filepath.Base(src)),
		})
		plan.Add(migrate.Action{
			Kind: migrate.MoveFile, Src: src, Dst: dst,
			Desc: fmt.Sprintf("move %s to %s/", filepath.Base(src), EncodePath(newCwd)),
		})
	}

	// A Claude project dir holds more than transcripts: per-session sidecar
	// dirs (<uuid>/tool-results) and project-scoped data (memory/). Everything
	// left over moves with the project so resume keeps full context.
	if entries, err := os.ReadDir(oldDir); err == nil {
		existing := map[string]bool{}
		if destEntries, err := os.ReadDir(newDir); err == nil {
			for _, e := range destEntries {
				existing[e.Name()] = true
			}
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, ".jsonl") && inGroup[strings.TrimSuffix(name, ".jsonl")] {
				continue // already planned above
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

	if oldDir != newDir {
		plan.Add(migrate.Action{
			Kind: migrate.RemoveEmptyDir, Src: oldDir,
			Desc: fmt.Sprintf("remove old project dir %s", filepath.Base(oldDir)),
		})
	}
	return plan, nil
}

func (c *Claude) RenamePlan(s model.Session, newTitle string) (*migrate.Plan, error) {
	plan, err := newRenamePlan(string(model.Claude), s, newTitle)
	if err != nil {
		return nil, err
	}
	newTitle = plan.NewTitle
	in, err := os.ReadFile(s.File)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(in, []byte(`"aiTitle"`)) && s.Title != "" {
		plan.Add(migrate.Action{
			Kind: migrate.RewriteCwd, Src: s.File, Field: "aiTitle",
			Old: s.Title, New: newTitle,
			Desc: fmt.Sprintf("rewrite aiTitle in %s", filepath.Base(s.File)),
		})
	} else {
		line, _ := json.Marshal(map[string]any{
			"type": "ai-title", "aiTitle": newTitle, "sessionId": s.ID,
		})
		plan.Add(migrate.Action{
			Kind: migrate.AppendJSONL, Src: s.File, New: string(line),
			Desc: fmt.Sprintf("append ai-title to %s", filepath.Base(s.File)),
		})
	}
	return plan, nil
}

func (c *Claude) DeletePlan(sessions []model.Session) (*migrate.Plan, error) {
	plan := &migrate.Plan{Agent: string(model.Claude)}
	for _, s := range sessions {
		plan.Add(migrate.Action{
			Kind: migrate.Archive, Src: s.File,
			Desc: fmt.Sprintf("archive %s", filepath.Base(s.File)),
		})
	}
	return plan, nil
}

func (c *Claude) ResumeCmd(s model.Session) ([]string, string) {
	return []string{"claude", "--resume", s.ID}, s.Cwd
}

func (c *Claude) ResumeEnv() []string {
	return []string{"CLAUDE_CONFIG_DIR=" + c.root}
}

func (c *Claude) ProcessNames() []string { return []string{"claude"} }

// OwnsProcess binds a claude pid to this CONFIG_DIR. A process is ours if
// CLAUDE_CONFIG_DIR or --json-path points here, or it has files open under
// this root. Unbound processes (no env, no flag, no matching fds) belong
// only to the default ~/.claude instance.
func (c *Claude) OwnsProcess(pid int, comm, cmdline string) bool {
	if dir, ok := explicitClaudeConfigDir(pid, cmdline); ok {
		return SamePath(dir, c.root)
	}
	if procTouchesDir(pid, c.root) {
		return true
	}
	return c.unbound
}

func (c *Claude) ActiveMarkers() []LiveActivity {
	var out []LiveActivity
	seen := map[int]bool{}
	add := func(pid int, desc string) {
		if !PidAlive(pid) || seen[pid] {
			return
		}
		seen[pid] = true
		out = append(out, LiveActivity{Desc: desc, PID: pid})
	}

	if data, err := os.ReadFile(filepath.Join(c.root, "daemon.status.json")); err == nil {
		var st struct {
			SupervisorPID int `json:"supervisorPid"`
			Workers       map[string]struct {
				PID int `json:"pid"`
			} `json:"workers"`
		}
		if json.Unmarshal(data, &st) == nil {
			add(st.SupervisorPID, fmt.Sprintf("daemon supervisor pid %d", st.SupervisorPID))
			for id, w := range st.Workers {
				add(w.PID, fmt.Sprintf("daemon worker %s pid %d", id, w.PID))
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(c.root, "daemon.lock")); err == nil {
		var lk struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(data, &lk) == nil {
			add(lk.PID, fmt.Sprintf("daemon.lock pid %d", lk.PID))
		}
	}
	return out
}
