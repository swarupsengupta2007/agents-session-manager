// agents-session-manager is a TUI that lists chat sessions of every
// supported agent CLI on the machine (Claude Code, Codex CLI, Grok CLI,
// Antigravity CLI, Qwen Code, Muse Code) and repairs sessions whose
// project folder has moved, so the agents' own resume commands find them
// again.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"agents-session-manager/internal/agents"
	"agents-session-manager/internal/config"
	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
	"agents-session-manager/internal/ui"
)

func main() {
	rest, extra := splitExtraDirFlags(os.Args[1:])
	opts := agents.DiscoverOptions{Extra: extra}

	if len(rest) > 0 {
		switch rest[0] {
		case "scan":
			runScan(rest[1:], opts)
			return
		case "remap":
			runRemap(rest[1:], opts)
			return
		case "rename":
			runRename(rest[1:], opts)
			return
		case "config":
			runConfig(rest[1:])
			return
		case "help", "--help", "-h":
			usage()
			return
		}
	}

	as := agents.DiscoverWith(opts)
	m := ui.New(as, backupRoot()).WithExtraDirs(extra)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

var extraDirFlags = map[string]model.Kind{
	"--claude-dir": model.Claude,
	"--codex-dir":  model.Codex,
	"--grok-dir":   model.Grok,
	"--qwen-dir":   model.Qwen,
	"--muse-dir":   model.Muse,
}

// splitExtraDirFlags pulls repeatable --<agent>-dir flags out of argv.
func splitExtraDirFlags(args []string) (rest []string, extra map[model.Kind][]string) {
	extra = map[model.Kind][]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		matched := false
		for flag, kind := range extraDirFlags {
			switch {
			case a == flag && i+1 < len(args):
				extra[kind] = append(extra[kind], args[i+1])
				i++
				matched = true
			case strings.HasPrefix(a, flag+"="):
				extra[kind] = append(extra[kind], strings.TrimPrefix(a, flag+"="))
				matched = true
			}
			if matched {
				break
			}
		}
		if !matched {
			rest = append(rest, a)
		}
	}
	return rest, extra
}

func backupRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".agents-session-manager", "backups")
}

func usage() {
	fmt.Print(`agents-session-manager — manage chat sessions of local agent CLIs

Usage:
  agents-session-manager            launch the TUI
  agents-session-manager scan       list sessions headlessly (--json, --orphans)
  agents-session-manager remap      move sessions of one agent from an old
                                    project path to a new one
                                    (--agent --from --to [--yes] [--store])
  agents-session-manager rename     change a session title/name
                                    (--agent --to [--id|--name|--session] [--store] [--yes])
  agents-session-manager config     list/add/remove extra agent home dirs

Global (repeatable; persist with "config add-dir <agent> PATH" or TUI a):
  --claude-dir PATH                 extra CLAUDE_CONFIG_DIR
  --codex-dir PATH                  extra CODEX_HOME
  --grok-dir PATH                   extra GROK_HOME
  --qwen-dir PATH                   extra QWEN_HOME
  --muse-dir PATH                   extra muse data dir (XDG_DATA_HOME/muse)

The TUI lists sessions from Claude Code, Codex CLI, Grok CLI,
Antigravity CLI (agy), Qwen Code and Muse Code, flags sessions whose
project folder no longer exists, and can remap them so the agents' own
resume commands (claude --resume, codex resume, grok --resume,
agy --conversation, qwen --resume, muse resume) find them again.
Originals are always copied to a backup directory under
~/.agents-session-manager/backups before anything is changed.
Remap/delete/rename are refused for an agent while that agent is running.
The TUI re-detects locks every second (press L to refresh now).
Writes snapshot, mutate a copy, swap, then verify the on-disk delta.
A process only locks the home dir it is actually using (env / open files).
agy has no relocatable home.
`)
}

// runRemap is the headless migration path:
// `agents-session-manager remap --agent claude --from /old --to /new [--yes]`.
func runRemap(args []string, opts agents.DiscoverOptions) {
	fs := flag.NewFlagSet("remap", flag.ExitOnError)
	agentName := fs.String("agent", "", "agent to migrate (claude|codex|grok|agy|qwen|muse)")
	from := fs.String("from", "", "old project path")
	to := fs.String("to", "", "new project path")
	store := fs.String("store", "", "storage root when multiple homes exist for --agent")
	yes := fs.Bool("yes", false, "apply without confirmation")
	fs.Parse(args)

	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
		os.Exit(1)
	}
	if *agentName == "" || *from == "" || *to == "" {
		fail("--agent, --from and --to are required")
	}

	var matches []agents.Agent
	for _, a := range agents.DiscoverWith(opts) {
		if string(a.Kind()) != *agentName {
			continue
		}
		if *store != "" && !agents.SamePath(a.Root(), *store) {
			continue
		}
		matches = append(matches, a)
	}
	if len(matches) == 0 {
		if *store != "" {
			fail("agent %q store %s not found on this machine", *agentName, *store)
		}
		fail("agent %q not found on this machine", *agentName)
	}
	agent := matches[0]
	if len(matches) > 1 {
		fail("multiple %s stores; pass --store (%s)", *agentName, joinRoots(matches))
	}
	if locks := agents.DetectLocks([]agents.Agent{agent}); len(locks) > 0 {
		fail("%s; refuse to modify its files while it is running", agents.FormatLocks(locks))
	}

	fromAbs, err := filepath.Abs(*from)
	if err != nil {
		fail("invalid --from: %v", err)
	}
	toAbs, err := filepath.Abs(*to)
	if err != nil {
		fail("invalid --to: %v", err)
	}

	ss, errs := agents.ScanAll(context.Background(), []agents.Agent{agent})
	for kind, err := range errs {
		fmt.Fprintf(os.Stderr, "scan warning: %s: %v\n", kind, err)
	}
	var group []model.Session
	for _, s := range ss {
		if filepath.Clean(s.Cwd) == filepath.Clean(fromAbs) {
			group = append(group, s)
		}
	}
	if len(group) == 0 {
		fail("no %s sessions found with cwd %s", *agentName, fromAbs)
	}

	plan, err := agent.RemapPlan(group, toAbs)
	if err != nil {
		fail("%v", err)
	}
	if err := plan.Validate(); err != nil {
		fail("%v", err)
	}

	fmt.Printf("Remapping %d %s session(s):\n", len(group), *agentName)
	for _, s := range group {
		title := s.Title
		if title == "" {
			title = s.ID
		}
		fmt.Printf("  %s  %s\n", s.ID, title)
	}
	fmt.Printf("\nPlan (%d actions):\n", len(plan.Actions))
	for _, a := range plan.Actions {
		fmt.Printf("  - %s\n", a.Desc)
	}
	fmt.Printf("\nOriginals will be backed up under %s.\n", backupRoot())

	if !*yes {
		fmt.Print("Apply? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("aborted")
			return
		}
	}
	if err := agents.ProbeUnlocked([]agents.Agent{agent}); err != nil {
		fail("%s", err)
	}
	rep, err := migrate.ApplyWith(plan, backupRoot(), func() error {
		return agents.ProbeUnlocked([]agents.Agent{agent})
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "completed %d action(s), then failed: %v\nbackup: %s\n",
			len(rep.Done), err, rep.BackupDir)
		os.Exit(1)
	}
	printApplyWarnings(rep)
	fmt.Printf("✓ remapped %d session(s) to %s\nbackup: %s\n", len(group), toAbs, rep.BackupDir)
}

func printApplyWarnings(rep *migrate.Report) {
	if rep == nil {
		return
	}
	for _, w := range rep.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
}

// runScan is the headless mode used for scripting and smoke testing:
// `agents-session-manager scan [--json] [--orphans]`.
func joinRoots(as []agents.Agent) string {
	parts := make([]string, len(as))
	for i, a := range as {
		parts[i] = a.Root()
	}
	return strings.Join(parts, ", ")
}

func runConfig(args []string) {
	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
		os.Exit(1)
	}
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		f, err := config.Load()
		if err != nil {
			fail("%v", err)
		}
		fmt.Printf("config: %s\n", config.Path())
		any := false
		for _, kind := range config.Relocatable {
			dirs := f.Dirs(kind)
			if len(dirs) == 0 {
				continue
			}
			any = true
			fmt.Printf("%s:\n", kind)
			for _, d := range dirs {
				fmt.Printf("  %s\n", d)
			}
		}
		if !any {
			fmt.Println("extra dirs: (none)")
		}
	case "add-dir":
		if len(args) < 3 {
			fail("usage: config add-dir <claude|codex|grok|qwen|muse> PATH")
		}
		f, err := config.AddDir(args[1], args[2])
		if err != nil {
			fail("%v", err)
		}
		fmt.Printf("saved %s\n", config.Path())
		for _, d := range f.Dirs(args[1]) {
			fmt.Printf("  %s\n", d)
		}
	case "rm-dir":
		if len(args) < 3 {
			fail("usage: config rm-dir <agent> PATH")
		}
		if _, err := config.RemoveDir(args[1], args[2]); err != nil {
			fail("%v", err)
		}
		fmt.Printf("removed %s %s from %s\n", args[1], args[2], config.Path())
	case "add-claude-dir":
		if len(args) < 2 {
			fail("usage: config add-claude-dir PATH")
		}
		if _, err := config.AddDir("claude", args[1]); err != nil {
			fail("%v", err)
		}
		fmt.Printf("saved %s\n", args[1])
	case "rm-claude-dir":
		if len(args) < 2 {
			fail("usage: config rm-claude-dir PATH")
		}
		if _, err := config.RemoveDir("claude", args[1]); err != nil {
			fail("%v", err)
		}
		fmt.Printf("removed %s\n", args[1])
	default:
		fail("unknown config command %q (list|add-dir|rm-dir)", args[0])
	}
}

func runRename(args []string, opts agents.DiscoverOptions) {
	fs := flag.NewFlagSet("rename", flag.ExitOnError)
	agentName := fs.String("agent", "", "agent (claude|codex|grok|agy|qwen|muse)")
	id := fs.String("id", "", "session GUID")
	name := fs.String("name", "", "current session title/name")
	session := fs.String("session", "", "GUID or current title")
	to := fs.String("to", "", "new title / session name")
	store := fs.String("store", "", "storage root when multiple homes exist")
	yes := fs.Bool("yes", false, "apply without confirmation")
	fs.Parse(args)

	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
		os.Exit(1)
	}
	if *agentName == "" || *to == "" {
		fail("--agent and --to are required")
	}
	sel := strings.TrimSpace(*session)
	if sel == "" {
		sel = strings.TrimSpace(*id)
	}
	if sel == "" {
		sel = strings.TrimSpace(*name)
	}
	if sel == "" {
		fail("pass --id, --name, or --session (GUID or current title)")
	}

	var matches []agents.Agent
	for _, a := range agents.DiscoverWith(opts) {
		if string(a.Kind()) != *agentName {
			continue
		}
		if *store != "" && !agents.SamePath(a.Root(), *store) {
			continue
		}
		matches = append(matches, a)
	}
	if len(matches) == 0 {
		fail("agent %q not found", *agentName)
	}
	if len(matches) > 1 {
		fail("multiple %s stores; pass --store (%s)", *agentName, joinRoots(matches))
	}
	agent := matches[0]
	if locks := agents.DetectLocks([]agents.Agent{agent}); len(locks) > 0 {
		fail("%s; refuse to modify its files while it is running", agents.FormatLocks(locks))
	}

	ss, errs := agents.ScanAll(context.Background(), []agents.Agent{agent})
	for kind, err := range errs {
		fmt.Fprintf(os.Stderr, "scan warning: %s: %v\n", kind, err)
	}
	found, err := findSession(ss, sel)
	if err != nil {
		fail("%v", err)
	}
	plan, err := agent.RenamePlan(found, *to)
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("Rename %s %s\n  %q  →  %q\n", found.Kind, found.ID, found.Title, plan.NewTitle)
	for _, a := range plan.Actions {
		fmt.Printf("  - %s\n", a.Desc)
	}
	if !*yes {
		fmt.Print("Apply? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("aborted")
			return
		}
	}
	if err := agents.ProbeUnlocked([]agents.Agent{agent}); err != nil {
		fail("%s", err)
	}
	rep, err := migrate.ApplyWith(plan, backupRoot(), func() error {
		return agents.ProbeUnlocked([]agents.Agent{agent})
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed: %v\nbackup: %s\n", err, rep.BackupDir)
		os.Exit(1)
	}
	printApplyWarnings(rep)
	fmt.Printf("✓ renamed to %q\nbackup: %s\n", plan.NewTitle, rep.BackupDir)
}

func findSession(ss []model.Session, sel string) (model.Session, error) {
	var byID, byName []model.Session
	lower := strings.ToLower(sel)
	for _, s := range ss {
		if s.ID == sel {
			byID = append(byID, s)
		}
		if s.Title != "" && strings.ToLower(s.Title) == lower {
			byName = append(byName, s)
		}
	}
	if len(byID) == 1 {
		return byID[0], nil
	}
	if len(byID) > 1 {
		return model.Session{}, fmt.Errorf("multiple sessions with id %s", sel)
	}
	if len(byName) == 1 {
		return byName[0], nil
	}
	if len(byName) > 1 {
		return model.Session{}, fmt.Errorf("multiple sessions named %q; pass --id", sel)
	}
	return model.Session{}, fmt.Errorf("no session matching %q", sel)
}

func runScan(args []string, opts agents.DiscoverOptions) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "output JSON")
	orphans := fs.Bool("orphans", false, "only show orphaned sessions")
	fs.Parse(args)

	as := agents.DiscoverWith(opts)
	if len(as) == 0 {
		fmt.Fprintln(os.Stderr, "no agent storage found (~/.claude, ~/.codex, ~/.grok, ~/.gemini, ~/.qwen, ~/.local/share/muse)")
		os.Exit(1)
	}
	ss, errs := agents.ScanAll(context.Background(), as)
	for kind, err := range errs {
		fmt.Fprintf(os.Stderr, "scan warning: %s: %v\n", kind, err)
	}
	if *orphans {
		filtered := ss[:0]
		for _, s := range ss {
			if s.Orphan {
				filtered = append(filtered, s)
			}
		}
		ss = filtered
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(ss); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	fmt.Printf("%-7s %-8s %-8s %-20s %-6s %s\n", "AGENT", "ORPHAN", "ACTIVE", "UPDATED", "MSGS", "PROJECT / TITLE")
	for _, s := range ss {
		orph := ""
		if s.Orphan {
			orph = "!"
		}
		act := ""
		if s.Active {
			act = "A"
		}
		title := s.Title
		if title == "" {
			title = s.ID
		}
		if len(title) > 48 {
			title = title[:47] + "…"
		}
		fmt.Printf("%-7s %-8s %-8s %-20s %-6d %s\n",
			s.Kind, orph, act, s.UpdatedAt.Format("2006-01-02 15:04"), s.Messages, s.Cwd)
		fmt.Printf("%s%s\n", "        ", title)
	}
	fmt.Printf("\n%d sessions across %d agents\n", len(ss), len(as))
}
