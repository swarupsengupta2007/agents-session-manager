package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procRoot is the process-info filesystem. Overridden in tests.
var procRoot = "/proc"

type procInfo struct {
	comm    string
	cmdline string
}

func listProcs() map[int]procInfo {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	out := make(map[int]procInfo)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "comm"))
		if err != nil {
			continue
		}
		info := procInfo{comm: strings.TrimSpace(string(b))}
		if cmd, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
			info.cmdline = strings.ReplaceAll(string(cmd), "\x00", " ")
		}
		out[pid] = info
	}
	return out
}

// commMatches reports whether comm equals spec, or starts with spec
// without a trailing "*" (e.g. spec "muse-bin-" matches "muse-bin-0.2.1-").
func procEnvValue(pid int, key string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "environ"))
	if err != nil {
		return "", false
	}
	for _, kv := range bytesSplitNull(b) {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == key {
			return v, true
		}
	}
	return "", false
}

func bytesSplitNull(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := strings.Split(string(b), "\x00")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func procTouchesDir(pid int, root string) bool {
	if root == "" {
		return false
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	root = filepath.Clean(root)
	fdDir := filepath.Join(procRoot, strconv.Itoa(pid), "fd")
	ents, err := os.ReadDir(fdDir)
	if err != nil {
		return false
	}
	prefix := root + string(filepath.Separator)
	for _, e := range ents {
		target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			continue
		}
		if target == root || strings.HasPrefix(target, prefix) {
			return true
		}
	}
	return false
}

// explicitClaudeConfigDir returns the CONFIG_DIR a claude process is bound
// to, if we can tell from its environment or command line.
func explicitClaudeConfigDir(pid int, cmdline string) (string, bool) {
	if dir, ok := procEnvValue(pid, "CLAUDE_CONFIG_DIR"); ok && strings.TrimSpace(dir) != "" {
		return filepath.Clean(dir), true
	}
	if dir := claudeDirFromCmdline(cmdline); dir != "" {
		return dir, true
	}
	return "", false
}

// claudeDirFromCmdline picks the config dir out of `claude daemon --json-path
// <dir>/daemon.json` (and --json-path=<path>).
func claudeDirFromCmdline(cmdline string) string {
	fields := strings.Fields(cmdline)
	for i, f := range fields {
		switch {
		case f == "--json-path" && i+1 < len(fields):
			return filepath.Clean(filepath.Dir(fields[i+1]))
		case strings.HasPrefix(f, "--json-path="):
			return filepath.Clean(filepath.Dir(strings.TrimPrefix(f, "--json-path=")))
		}
	}
	return ""
}

func commMatches(comm, spec string) bool {
	if spec == "" {
		return false
	}
	if strings.HasSuffix(spec, "*") {
		return strings.HasPrefix(comm, strings.TrimSuffix(spec, "*"))
	}
	return comm == spec
}

// parseProcLock matches one /proc/locks line against a file's device + inode.
// Format after the leading "N:": TYPE MODE KIND PID MAJ:MIN:INO START END
func parseProcLock(line string, maj, min uint32, ino uint64) (int, bool) {
	_, rest, found := strings.Cut(strings.TrimSpace(line), ":")
	if !found {
		return 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) < 5 {
		return 0, false
	}
	pid, err := strconv.Atoi(fields[3])
	if err != nil || pid <= 0 {
		return 0, false
	}
	parts := strings.Split(fields[4], ":")
	if len(parts) != 3 {
		return 0, false
	}
	m1, err1 := strconv.ParseUint(parts[0], 16, 32)
	m2, err2 := strconv.ParseUint(parts[1], 16, 32)
	inode, err3 := strconv.ParseUint(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	if uint32(m1) == maj && uint32(m2) == min && inode == ino {
		return pid, true
	}
	return 0, false
}

// linuxDev splits a Linux kdev_t into major/minor, matching /proc/locks.
func linuxDev(dev uint64) (maj, min uint32) {
	maj = uint32((dev >> 8) & 0xfff)
	min = uint32((dev & 0xff) | ((dev >> 12) & 0xfff00))
	return maj, min
}

func formatPIDs(pids []int) string {
	if len(pids) == 0 {
		return "?"
	}
	parts := make([]string, len(pids))
	for i, p := range pids {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ", ")
}

func appendUniquePID(pids []int, pid int) []int {
	if pid <= 0 {
		return pids
	}
	for _, p := range pids {
		if p == pid {
			return pids
		}
	}
	return append(pids, pid)
}

func describeLock(l Lock) string {
	name := l.Label
	if name == "" {
		name = string(l.Kind)
	}
	return fmt.Sprintf("%s (PID %s)", name, formatPIDs(l.PIDs))
}
