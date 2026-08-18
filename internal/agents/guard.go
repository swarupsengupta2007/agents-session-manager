package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"agents-session-manager/internal/model"
)

// Lock is a live writer for one agent's on-disk session store.
// Detection is the union of comm-matched processes and live activity
// markers: over-locking is acceptable, under-locking is not.
type Lock struct {
	Kind    model.Kind
	Root    string // storage root this lock applies to
	Label   string // display name, e.g. claude@custom
	PIDs    []int
	Sources []string
}

// DetectLocks reports which of the given agents currently have a live
// process or activity marker that may write their session files.
func DetectLocks(as []Agent) []Lock {
	type lockID struct {
		kind model.Kind
		root string
	}
	byID := map[lockID]*Lock{}
	ensure := func(a Agent) *Lock {
		id := lockID{a.Kind(), a.Root()}
		if byID[id] == nil {
			byID[id] = &Lock{Kind: a.Kind(), Root: a.Root(), Label: AgentLabel(a)}
		}
		return byID[id]
	}

	type hit struct {
		agent Agent
		pid   int
		via   string
	}
	var hits []hit
	for pid, p := range listProcs() {
		for _, a := range as {
			via, ok := processNameHit(a, p)
			if !ok {
				continue
			}
			if b, ok := a.(ProcessBinder); ok && !b.OwnsProcess(pid, p.comm, p.cmdline) {
				continue
			}
			hits = append(hits, hit{a, pid, via})
		}
	}

	bound := map[int]bool{}
	for _, h := range hits {
		if u, ok := h.agent.(UnboundClaimer); !ok || !u.ClaimsUnbound() {
			bound[h.pid] = true
		}
	}
	for _, h := range hits {
		if u, ok := h.agent.(UnboundClaimer); ok && u.ClaimsUnbound() && bound[h.pid] {
			continue
		}
		l := ensure(h.agent)
		l.PIDs = appendUniquePID(l.PIDs, h.pid)
		l.Sources = append(l.Sources, "process "+h.via+" pid "+strconv.Itoa(h.pid))
	}

	for _, a := range as {
		m, ok := a.(ActivityMarker)
		if !ok {
			continue
		}
		for _, act := range m.ActiveMarkers() {
			// PID 0 means a lock/marker is held but we could not name
			// the holder — still lock (over-locking is OK).
			if act.PID > 0 && !procStillOurs(a, act.PID) {
				continue
			}
			l := ensure(a)
			l.PIDs = appendUniquePID(l.PIDs, act.PID)
			if act.Desc != "" {
				l.Sources = append(l.Sources, act.Desc)
			}
		}
	}

	out := make([]Lock, 0, len(byID))
	seen := map[lockID]bool{}
	for _, a := range as {
		id := lockID{a.Kind(), a.Root()}
		if seen[id] {
			continue
		}
		if l := byID[id]; l != nil {
			out = append(out, *l)
			seen[id] = true
		}
	}
	return out
}

// ProbeUnlocked is a migrate.ProbeFunc that fails while any of as has a
// live writer. Used around copy-modify-swap so an agent that starts mid-
// write aborts before the original is replaced.
func ProbeUnlocked(as []Agent) error {
	locks := DetectLocks(as)
	if len(locks) == 0 {
		return nil
	}
	return fmt.Errorf("%s; refuse to modify its files while it is running", FormatLocks(locks))
}

// procStillOurs reports whether pid is still a process of this agent.
// An unreadable /proc entry is treated as ours (under-locking is worse).
// A live pid whose comm/cmdline no longer matches is a reused pid and
// must not keep the store locked.
func procStillOurs(a Agent, pid int) bool {
	if !PidAlive(pid) {
		return false
	}
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	b, err := os.ReadFile(filepath.Join(dir, "comm"))
	comm := ""
	if err == nil {
		comm = strings.TrimSpace(string(b))
	}
	cmdline := ""
	if cmd, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		cmdline = strings.ReplaceAll(string(cmd), "\x00", " ")
	}
	if comm == "" && cmdline == "" {
		return true
	}
	_, ok := processNameHit(a, procInfo{comm: comm, cmdline: cmdline})
	return ok
}

func processNameHit(a Agent, p procInfo) (string, bool) {
	for _, spec := range a.ProcessNames() {
		if commMatches(p.comm, spec) {
			return "comm " + p.comm, true
		}
	}
	if h, ok := a.(CmdlineHint); ok {
		for _, hint := range h.CmdlineHints() {
			if hint != "" && strings.Contains(p.cmdline, hint) {
				return "cmdline " + hint, true
			}
		}
	}
	return "", false
}

// LockedKinds is a set of agent kinds currently locked.
func LockedKinds(locks []Lock) map[model.Kind]bool {
	out := make(map[model.Kind]bool, len(locks))
	for _, l := range locks {
		out[l.Kind] = true
	}
	return out
}

// FormatLocks is a short banner/error phrase naming each locked agent and its PIDs.
func FormatLocks(locks []Lock) string {
	if len(locks) == 0 {
		return ""
	}
	parts := make([]string, len(locks))
	for i, l := range locks {
		parts[i] = describeLock(l)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return joinAnd(parts)
}

func joinAnd(parts []string) string {
	if len(parts) == 2 {
		return parts[0] + " and " + parts[1]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
}
