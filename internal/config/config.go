// Package config loads the user's agents-session-manager settings.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Relocatable agents — those with an official home/config override.
var Relocatable = []string{"claude", "codex", "grok", "qwen", "muse"}

// File is the on-disk settings document.
type File struct {
	// ClaudeDirs is the legacy extra-Claude list. Still read; new writes
	// go to ExtraDirs["claude"].
	ClaudeDirs []string `json:"claude_dirs,omitempty"`
	// ExtraDirs maps agent kind → extra storage roots
	// (CLAUDE_CONFIG_DIR / CODEX_HOME / GROK_HOME / QWEN_HOME / muse data dir).
	ExtraDirs map[string][]string `json:"extra_dirs,omitempty"`
}

// KnownKind reports whether kind accepts extra storage roots.
func KnownKind(kind string) bool {
	for _, k := range Relocatable {
		if k == kind {
			return true
		}
	}
	return false
}

// Dirs returns extra roots registered for kind, including legacy claude_dirs.
func (f File) Dirs(kind string) []string {
	var out []string
	if kind == "claude" {
		out = append(out, f.ClaudeDirs...)
	}
	if f.ExtraDirs != nil {
		out = append(out, f.ExtraDirs[kind]...)
	}
	return uniquePaths(out)
}

// Path is the settings file. ASM_CONFIG overrides it (used by tests).
func Path() string {
	if p := os.Getenv("ASM_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "agents-session-manager.json"
	}
	return filepath.Join(home, ".agents-session-manager", "config.json")
}

// Load reads the settings file. A missing file is an empty config.
func Load() (File, error) {
	var f File
	b, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("config %s: %w", Path(), err)
	}
	return f, nil
}

// Save writes the settings file, creating the parent directory.
func Save(f File) error {
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

// AddClaudeDir is AddDir("claude", dir).
func AddClaudeDir(dir string) (File, error) { return AddDir("claude", dir) }

// RemoveClaudeDir is RemoveDir("claude", dir).
func RemoveClaudeDir(dir string) (File, error) { return RemoveDir("claude", dir) }

// AddDir appends an extra storage root for kind and persists the config.
func AddDir(kind, dir string) (File, error) {
	if !KnownKind(kind) {
		return File{}, fmt.Errorf("agent %q does not support a custom home dir", kind)
	}
	abs, err := absDir(dir)
	if err != nil {
		return File{}, err
	}
	f, err := Load()
	if err != nil {
		return f, err
	}
	if containsPath(f.Dirs(kind), abs) {
		return f, nil
	}
	if f.ExtraDirs == nil {
		f.ExtraDirs = map[string][]string{}
	}
	f.ExtraDirs[kind] = append(f.ExtraDirs[kind], abs)
	return f, Save(f)
}

// RemoveDir drops a previously added extra root for kind.
func RemoveDir(kind, dir string) (File, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	abs = filepath.Clean(abs)
	f, err := Load()
	if err != nil {
		return f, err
	}
	if kind == "claude" {
		f.ClaudeDirs = dropPath(f.ClaudeDirs, abs)
	}
	if f.ExtraDirs != nil {
		f.ExtraDirs[kind] = dropPath(f.ExtraDirs[kind], abs)
		if len(f.ExtraDirs[kind]) == 0 {
			delete(f.ExtraDirs, kind)
		}
	}
	return f, Save(f)
}

func uniquePaths(in []string) []string {
	var out []string
	for _, p := range in {
		if !containsPath(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func containsPath(list []string, p string) bool {
	for _, e := range list {
		if same(e, p) {
			return true
		}
	}
	return false
}

func dropPath(list []string, p string) []string {
	out := list[:0]
	for _, e := range list {
		if !same(e, p) {
			out = append(out, e)
		}
	}
	return out
}

func absDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("store dir: %w", err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("store dir %s is not a directory", abs)
	}
	return abs, nil
}

func same(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}
