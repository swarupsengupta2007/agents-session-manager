//go:build linux

package agents

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// flockHeld reports whether another process currently holds a flock on path.
// Same-process holders are treated as not held (Linux flock is per-process).
func flockHeld(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// flockHolderPIDs returns pids listed in /proc/locks for path.
func flockHolderPIDs(path string) []int {
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	maj, min := linuxDev(uint64(sys.Dev))
	data, err := os.ReadFile(filepath.Join(procRoot, "locks"))
	if err != nil {
		return nil
	}
	var pids []int
	seen := map[int]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		pid, ok := parseProcLock(line, maj, min, sys.Ino)
		if !ok || seen[pid] {
			continue
		}
		seen[pid] = true
		pids = append(pids, pid)
	}
	return pids
}
