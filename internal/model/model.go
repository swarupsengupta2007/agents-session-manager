// Package model defines the shared session types used across agents.
package model

import (
	"sort"
	"time"
)

// Kind identifies a supported agent CLI.
type Kind string

const (
	Claude Kind = "claude"
	Codex  Kind = "codex"
	Grok   Kind = "grok"
	Agy    Kind = "agy"
	Qwen   Kind = "qwen"
	Muse   Kind = "muse"
)

// Session is one recoverable chat session of an agent.
type Session struct {
	Kind      Kind
	ID        string
	Cwd       string // project path recorded in the transcript
	Title     string
	Model     string
	File      string // primary artifact: transcript file (claude/codex) or session dir (grok)
	CreatedAt time.Time
	UpdatedAt time.Time
	Messages  int
	SizeBytes int64
	Orphan    bool   // Cwd no longer exists on disk
	Active    bool   // an agent process currently owns this session
	Store     string // agent storage root (e.g. ~/.claude or a custom CLAUDE_CONFIG_DIR)
}

// SortSessions orders sessions orphans-first, then most recently updated.
func SortSessions(ss []Session) {
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].Orphan != ss[j].Orphan {
			return ss[i].Orphan
		}
		return ss[i].UpdatedAt.After(ss[j].UpdatedAt)
	})
}
