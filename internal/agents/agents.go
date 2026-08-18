// Package agents implements per-agent session discovery and migration
// planning for Claude Code, Codex CLI, Grok CLI, Antigravity CLI (agy),
// Qwen Code and Muse Code.
package agents

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"agents-session-manager/internal/config"
	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

// Agent knows how one CLI stores its chat sessions.
type Agent interface {
	Kind() model.Kind
	Root() string
	Installed() bool
	Scan(ctx context.Context) ([]model.Session, error)
	// RemapPlan migrates the given sessions (all sharing one old cwd) to newCwd.
	RemapPlan(sessions []model.Session, newCwd string) (*migrate.Plan, error)
	// DeletePlan soft-deletes the sessions (artifacts moved to the backup dir).
	DeletePlan(sessions []model.Session) (*migrate.Plan, error)
	// RenamePlan changes the session's display title / session name.
	RenamePlan(s model.Session, newTitle string) (*migrate.Plan, error)
	// ResumeCmd returns the command that resumes the session in the agent CLI.
	ResumeCmd(s model.Session) (argv []string, dir string)
	// ProcessNames lists process comm names that may write this agent's data.
	ProcessNames() []string
}

// LiveActivity is one on-disk marker of a live agent session or process.
type LiveActivity struct {
	Desc string
	PID  int // 0 if the holder pid is unknown
}

// ActivityMarker is optionally implemented by agents that maintain
// authoritative on-disk markers of live sessions (lock files, pid files,
// status files).
type ActivityMarker interface {
	ActiveMarkers() []LiveActivity
}

// CmdlineHint is optionally implemented by agents whose writer processes
// run under a generic comm (e.g. node). A process is matched if its
// /proc/<pid>/cmdline contains any of the returned substrings.
type CmdlineHint interface {
	CmdlineHints() []string
}

// ProcessBinder decides whether a name-matched process is writing THIS
// agent's store. Used when the same binary (claude) can target more than
// one CONFIG_DIR.
type ProcessBinder interface {
	OwnsProcess(pid int, comm, cmdline string) bool
}

// UnboundClaimer is a ProcessBinder that also claims processes with no
// explicit store binding (no CLAUDE_CONFIG_DIR, no --json-path, no open
// files under another store). Only the default ~/.claude instance should
// implement this as true.
type UnboundClaimer interface {
	ClaimsUnbound() bool
}

// DiscoverOptions tunes agent discovery.
type DiscoverOptions struct {
	// Extra maps agent kind → extra storage roots for this run
	// (on top of the default home and persisted config).
	Extra map[model.Kind][]string
	// ExtraClaudeDirs is a convenience alias for Extra[claude].
	ExtraClaudeDirs []string
}

func (o DiscoverOptions) extra(k model.Kind) []string {
	var out []string
	if k == model.Claude {
		out = append(out, o.ExtraClaudeDirs...)
	}
	if o.Extra != nil {
		out = append(out, o.Extra[k]...)
	}
	return out
}

// AgentLabel is the UI/CLI name for an agent instance. Extra Claude
// stores are "claude@<basename>".
func AgentLabel(a Agent) string {
	if n, ok := a.(interface{ Label() string }); ok {
		if s := n.Label(); s != "" {
			return s
		}
	}
	return string(a.Kind())
}

// SamePath reports whether two filesystem paths name the same location.
func SamePath(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}

// PidAlive reports whether <procRoot>/<pid> exists. Used to validate
// pids recorded in agent marker files before trusting them.
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(filepath.Join(procRoot, strconv.Itoa(pid)))
	return err == nil
}

var (
	_ Agent          = (*Claude)(nil)
	_ Agent          = (*Codex)(nil)
	_ Agent          = (*Grok)(nil)
	_ Agent          = (*Agy)(nil)
	_ Agent          = (*Qwen)(nil)
	_ Agent          = (*Muse)(nil)
	_ ActivityMarker = (*Claude)(nil)
	_ ActivityMarker = (*Grok)(nil)
	_ ActivityMarker = (*Agy)(nil)
	_ ActivityMarker = (*Qwen)(nil)
	_ ActivityMarker = (*Muse)(nil)
	_ CmdlineHint    = (*Qwen)(nil)
	_ ProcessBinder  = (*Claude)(nil)
	_ ProcessBinder  = (*Codex)(nil)
	_ ProcessBinder  = (*Grok)(nil)
	_ ProcessBinder  = (*Qwen)(nil)
	_ ProcessBinder  = (*Muse)(nil)
	_ UnboundClaimer = (*Claude)(nil)
	_ UnboundClaimer = (*Codex)(nil)
	_ UnboundClaimer = (*Grok)(nil)
	_ UnboundClaimer = (*Qwen)(nil)
	_ UnboundClaimer = (*Muse)(nil)
)

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// Discover returns agents whose storage directory exists on this machine.
func Discover() []Agent { return DiscoverWith(DiscoverOptions{}) }

// DiscoverWith is Discover plus extra relocatable storage roots.
func DiscoverWith(opts DiscoverOptions) []Agent {
	home := homeDir()
	cfg, _ := config.Load()

	var candidates []Agent
	seen := map[string]bool{}
	add := func(a Agent) {
		key := string(a.Kind()) + "\x00" + filepath.Clean(a.Root())
		if seen[key] {
			return
		}
		seen[key] = true
		candidates = append(candidates, a)
	}

	add(newClaude(defaultClaudeRoot(home), false))
	for _, d := range append(cfg.Dirs("claude"), opts.extra(model.Claude)...) {
		add(newClaude(d, true))
	}

	add(NewCodex(home))
	for _, d := range append(cfg.Dirs("codex"), opts.extra(model.Codex)...) {
		add(newCodex(d, true))
	}

	add(NewGrok(home))
	for _, d := range append(cfg.Dirs("grok"), opts.extra(model.Grok)...) {
		add(newGrok(d, true))
	}

	add(NewQwen(home))
	for _, d := range append(cfg.Dirs("qwen"), opts.extra(model.Qwen)...) {
		add(newQwen(d, true))
	}

	add(NewMuse(home))
	for _, d := range append(cfg.Dirs("muse"), opts.extra(model.Muse)...) {
		add(newMuse(d, true))
	}

	add(NewAgy(home))

	var out []Agent
	for _, a := range candidates {
		if a.Installed() {
			out = append(out, a)
		}
	}
	return out
}

func defaultClaudeRoot(home string) string {
	if e := os.Getenv("CLAUDE_CONFIG_DIR"); e != "" {
		return e
	}
	return filepath.Join(home, ".claude")
}

// ScanAll scans every agent concurrently and marks orphaned sessions
// (recorded cwd missing on disk). Per-agent scan errors are returned keyed
// by kind; partial results are still included.
func ScanAll(ctx context.Context, as []Agent) ([]model.Session, map[model.Kind]error) {
	type result struct {
		kind model.Kind
		ss   []model.Session
		err  error
	}
	ch := make(chan result, len(as))
	var wg sync.WaitGroup
	for _, a := range as {
		wg.Add(1)
		go func(a Agent) {
			defer wg.Done()
			ss, err := a.Scan(ctx)
			for i := range ss {
				if ss[i].Store == "" {
					ss[i].Store = a.Root()
				}
			}
			ch <- result{a.Kind(), ss, err}
		}(a)
	}
	wg.Wait()
	close(ch)

	var out []model.Session
	errs := map[model.Kind]error{}
	for r := range ch {
		if r.err != nil {
			errs[r.kind] = r.err
		}
		out = append(out, r.ss...)
	}
	for i := range out {
		out[i].Orphan = isOrphan(out[i].Cwd)
	}
	model.SortSessions(out)
	return out, errs
}

func isOrphan(cwd string) bool {
	if cwd == "" {
		return false
	}
	_, err := os.Stat(cwd)
	return os.IsNotExist(err)
}
