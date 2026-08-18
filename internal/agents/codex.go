package agents

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

// Codex reads ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl files. The
// project path is not encoded anywhere on disk: it lives in the cwd field
// of the session_meta record, so a remap only rewrites that field.
type Codex struct {
	root    string
	extra   bool
	unbound bool
}

func NewCodex(home string) *Codex {
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		root = filepath.Join(home, ".codex")
	}
	return newCodex(root, false)
}

func newCodex(root string, extra bool) *Codex {
	root = absClean(root)
	return &Codex{root: root, extra: extra, unbound: !extra}
}

func (c *Codex) Kind() model.Kind    { return model.Codex }
func (c *Codex) Root() string        { return c.root }
func (c *Codex) Label() string       { return extraLabel(model.Codex, c.extra, c.root) }
func (c *Codex) ClaimsUnbound() bool { return c.unbound }

func (c *Codex) Installed() bool {
	st, err := os.Stat(c.sessionsDir())
	return err == nil && st.IsDir()
}

func (c *Codex) sessionsDir() string { return filepath.Join(c.root, "sessions") }

func (c *Codex) Scan(ctx context.Context) ([]model.Session, error) {
	var files []string
	err := filepath.WalkDir(c.sessionsDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // tolerate partial trees
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), ".jsonl") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
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
	for w := 0; w < 4; w++ {
		go func() {
			for f := range jobs {
				s, err := scanCodexSession(f)
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

func titleSidecar(transcript string) string { return transcript + ".title" }

func scanCodexSession(path string) (model.Session, error) {
	s := model.Session{Kind: model.Codex, File: path}
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
		if !bytes.Contains(line, []byte(`"session_meta"`)) &&
			!bytes.Contains(line, []byte(`"user_message"`)) &&
			!bytes.Contains(line, []byte(`"agent_message"`)) {
			continue
		}
		var env struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				ID        string `json:"id"`
				SessionID string `json:"session_id"`
				Cwd       string `json:"cwd"`
				Timestamp string `json:"timestamp"`
				Message   string `json:"message"`
				Type      string `json:"type"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		switch {
		case env.Type == "session_meta":
			s.ID = env.Payload.ID
			if s.ID == "" {
				s.ID = env.Payload.SessionID
			}
			s.Cwd = env.Payload.Cwd
			ts := env.Payload.Timestamp
			if ts == "" {
				ts = env.Timestamp
			}
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				s.CreatedAt = t
			}
		case env.Payload.Type == "user_message":
			s.Messages++
			if s.Title == "" {
				s.Title = truncate(env.Payload.Message, 100)
			}
		case env.Payload.Type == "agent_message":
			s.Messages++
		}
	}
	if s.ID == "" {
		// Fall back to the UUID embedded in the file name.
		base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if i := strings.LastIndex(base, "-0"); i > 0 {
			s.ID = base[i+1:]
		}
	}
	if b, err := os.ReadFile(titleSidecar(path)); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			s.Title = t
		}
	}
	return s, sc.Err()
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (c *Codex) RemapPlan(sessions []model.Session, newCwd string) (*migrate.Plan, error) {
	if len(sessions) == 0 {
		return nil, fmt.Errorf("no sessions to remap")
	}
	oldCwd := sessions[0].Cwd
	plan := &migrate.Plan{Agent: string(model.Codex), OldCwd: oldCwd, NewCwd: newCwd}
	for _, s := range sessions {
		plan.Add(migrate.Action{
			Kind: migrate.RewriteCwd, Src: s.File, Old: oldCwd, New: newCwd,
			Desc: fmt.Sprintf("rewrite cwd in %s", filepath.Base(s.File)),
		})
	}
	return plan, nil
}

func (c *Codex) RenamePlan(s model.Session, newTitle string) (*migrate.Plan, error) {
	plan, err := newRenamePlan(string(model.Codex), s, newTitle)
	if err != nil {
		return nil, err
	}
	plan.Add(migrate.Action{
		Kind: migrate.WriteFile, Src: titleSidecar(s.File), New: plan.NewTitle + "\n",
		Desc: fmt.Sprintf("set title sidecar for %s", filepath.Base(s.File)),
	})
	return plan, nil
}

func (c *Codex) DeletePlan(sessions []model.Session) (*migrate.Plan, error) {
	plan := &migrate.Plan{Agent: string(model.Codex)}
	for _, s := range sessions {
		plan.Add(migrate.Action{
			Kind: migrate.Archive, Src: s.File,
			Desc: fmt.Sprintf("archive %s", filepath.Base(s.File)),
		})
	}
	return plan, nil
}

func (c *Codex) ResumeCmd(s model.Session) ([]string, string) {
	return []string{"codex", "resume", s.ID}, s.Cwd
}

func (c *Codex) ResumeEnv() []string { return []string{"CODEX_HOME=" + c.root} }

func (c *Codex) ProcessNames() []string { return []string{"codex"} }

func (c *Codex) OwnsProcess(pid int, comm, cmdline string) bool {
	return ownsByEnvOrFD(pid, "CODEX_HOME", c.root, c.unbound)
}
