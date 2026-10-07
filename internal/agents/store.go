package agents

import (
	"fmt"
	"path/filepath"
	"strings"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

// SupportsExtra reports whether kind has an official relocatable home.
func SupportsExtra(k model.Kind) bool {
	switch k {
	case model.Claude, model.Codex, model.Grok, model.Qwen, model.Muse:
		return true
	default:
		return false
	}
}

// LooksLikeHome reports whether dir has the store layout kind expects, so
// a caller can warn before saving a folder that is not an agent home.
func LooksLikeHome(k model.Kind, dir string) bool {
	switch k {
	case model.Claude:
		return newClaude(dir, true).Installed()
	case model.Codex:
		return newCodex(dir, true).Installed()
	case model.Grok:
		return newGrok(dir, true).Installed()
	case model.Qwen:
		return newQwen(dir, true).Installed()
	case model.Muse:
		return newMuse(dir, true).Installed()
	default:
		return false
	}
}

func absClean(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	return filepath.Clean(p)
}

func extraLabel(kind model.Kind, extra bool, root string) string {
	if !extra {
		return string(kind)
	}
	return string(kind) + "@" + filepath.Base(root)
}

func newRenamePlan(agent string, s model.Session, newTitle string) (*migrate.Plan, error) {
	newTitle = strings.TrimSpace(newTitle)
	if newTitle == "" {
		return nil, fmt.Errorf("new title is empty")
	}
	if newTitle == strings.TrimSpace(s.Title) {
		return nil, fmt.Errorf("title is unchanged")
	}
	return &migrate.Plan{Agent: agent, NewTitle: newTitle}, nil
}

// ownsByEnvOrFD binds a process to root via envKey (if set) or open files
// under root. Unbound processes (no env, no matching fds) match only when
// unbound is true.
func ownsByEnvOrFD(pid int, envKey, root string, unbound bool) bool {
	if envKey != "" {
		if dir, ok := procEnvValue(pid, envKey); ok && strings.TrimSpace(dir) != "" {
			return SamePath(dir, root)
		}
	}
	if procTouchesDir(pid, root) {
		return true
	}
	return unbound
}
