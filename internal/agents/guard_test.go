package agents

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agents-session-manager/internal/model"
)

func TestDetectLocksProcessScan(t *testing.T) {
	proc := t.TempDir()
	if err := os.Mkdir(filepath.Join(proc, "42"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "42", "comm"), []byte("agy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(proc, "7"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "7", "comm"), []byte("bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(proc, "9"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "9", "comm"), []byte("grok\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	old := procRoot
	procRoot = proc
	t.Cleanup(func() { procRoot = old })

	home := t.TempDir()
	locks := DetectLocks([]Agent{NewAgy(home), NewGrok(home), NewClaude(home), NewCodex(home)})
	got := LockedKinds(locks)
	if !got[model.Agy] || !got[model.Grok] {
		t.Fatalf("locks = %+v", locks)
	}
	if got[model.Claude] || got[model.Codex] {
		t.Fatalf("unexpected lock: %+v", locks)
	}
	// herdr wrappers (comm=bash) must not count.
	for _, l := range locks {
		for _, p := range l.PIDs {
			if p == 7 {
				t.Fatal("bash helper should not lock")
			}
		}
	}
	s := FormatLocks(locks)
	if s == "" || !strings.Contains(s, "agy") || !strings.Contains(s, "42") {
		t.Fatalf("FormatLocks = %q", s)
	}
}

func TestDetectLocksMarkersUnion(t *testing.T) {
	proc := t.TempDir()
	old := procRoot
	procRoot = proc
	t.Cleanup(func() { procRoot = old })

	home := t.TempDir()
	// Fake a live grok marker pointing at a fake pid that exists under procRoot.
	if err := os.Mkdir(filepath.Join(proc, "77"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".grok", "active_sessions.json"),
		[]byte(`[{"session_id":"s1","pid":77,"cwd":"/tmp"}]`), 0o644); err != nil {
		// parent dir missing
		if err := os.MkdirAll(filepath.Join(home, ".grok"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".grok", "active_sessions.json"),
			[]byte(`[{"session_id":"s1","pid":77,"cwd":"/tmp"}]`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	locks := DetectLocks([]Agent{NewGrok(home)})
	if len(locks) != 1 || locks[0].Kind != model.Grok || len(locks[0].PIDs) != 1 || locks[0].PIDs[0] != 77 {
		t.Fatalf("locks = %+v", locks)
	}
}

func TestDetectLocksIgnoresReusedMarkerPID(t *testing.T) {
	proc := t.TempDir()
	old := procRoot
	procRoot = proc
	t.Cleanup(func() { procRoot = old })

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".grok"), 0o755); err != nil {
		t.Fatal(err)
	}
	// pid 77 exists but is now some other process (pid reuse after grok exited).
	if err := os.Mkdir(filepath.Join(proc, "77"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "77", "comm"), []byte("sshd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".grok", "active_sessions.json"),
		[]byte(`[{"session_id":"s1","pid":77,"cwd":"/tmp"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	locks := DetectLocks([]Agent{NewGrok(home)})
	if len(locks) != 0 {
		t.Fatalf("reused pid must not lock: %+v", locks)
	}
}

func TestDetectLocksIgnoresDeadMarkerPID(t *testing.T) {
	proc := t.TempDir()
	old := procRoot
	procRoot = proc
	t.Cleanup(func() { procRoot = old })

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".grok"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".grok", "active_sessions.json"),
		[]byte(`[{"session_id":"s1","pid":999999,"cwd":"/tmp"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	locks := DetectLocks([]Agent{NewGrok(home)})
	if len(locks) != 0 {
		t.Fatalf("dead pid should not lock: %+v", locks)
	}
}

func TestDetectLocksCommPrefixAndCmdline(t *testing.T) {
	proc := t.TempDir()
	writeProcComm := func(pid int, comm, cmdline string) {
		t.Helper()
		dir := filepath.Join(proc, strconv.Itoa(pid))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeProcComm(11, "muse-bin-0.2.1-", "/root/.local/bin/muse-bin-0.2.1-R1215.1 --yolo")
	writeProcComm(12, "node", "/root/.local/lib/qwen-code/node/bin/node\x00/root/.local/lib/qwen-code/lib/cli.js")
	writeProcComm(13, "node", "/usr/bin/node\x00some-other-app.js")
	writeProcComm(14, "bash", "/usr/bin/bash\x00/root/.local/bin/muse")

	old := procRoot
	procRoot = proc
	t.Cleanup(func() { procRoot = old })

	home := t.TempDir()
	locks := DetectLocks([]Agent{NewMuse(home), NewQwen(home)})
	got := LockedKinds(locks)
	if !got[model.Muse] || !got[model.Qwen] {
		t.Fatalf("locks = %+v", locks)
	}
	for _, l := range locks {
		for _, p := range l.PIDs {
			if p == 13 || p == 14 {
				t.Fatalf("unrelated process %d should not lock: %+v", p, locks)
			}
		}
	}
}

func TestClaudeLockBindsToConfigDir(t *testing.T) {
	proc := t.TempDir()
	write := func(pid int, comm, cmdline, environ string) {
		t.Helper()
		dir := filepath.Join(proc, strconv.Itoa(pid))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatal(err)
		}
		if environ != "" {
			if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(environ), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	defRoot := filepath.Join(t.TempDir(), "default-claude")
	extraRoot := filepath.Join(t.TempDir(), "custom-claude")
	for _, r := range []string{defRoot, extraRoot} {
		if err := os.MkdirAll(filepath.Join(r, "projects"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// pid 21: custom store via env
	write(21, "claude", "claude", "CLAUDE_CONFIG_DIR="+extraRoot+"\x00")
	// pid 22: default store via daemon --json-path
	write(22, "claude", "claude daemon run --json-path "+defRoot+"/daemon.json", "")
	// pid 23: unbound claude (no env, no flag)
	write(23, "claude", "claude", "")

	old := procRoot
	procRoot = proc
	t.Cleanup(func() { procRoot = old })

	def := newClaude(defRoot, false)
	def.unbound = true
	extra := newClaude(extraRoot, true)

	locks := DetectLocks([]Agent{def, extra})
	byRoot := map[string]Lock{}
	for _, l := range locks {
		byRoot[l.Root] = l
	}
	if l, ok := byRoot[extra.Root()]; !ok || len(l.PIDs) != 1 || l.PIDs[0] != 21 {
		t.Fatalf("extra lock %+v", locks)
	}
	if l, ok := byRoot[def.Root()]; !ok {
		t.Fatalf("default not locked: %+v", locks)
	} else {
		for _, p := range l.PIDs {
			if p == 21 {
				t.Fatalf("custom-dir process locked the default store: %+v", l)
			}
		}
		has22, has23 := false, false
		for _, p := range l.PIDs {
			if p == 22 {
				has22 = true
			}
			if p == 23 {
				has23 = true
			}
		}
		if !has22 || !has23 {
			t.Fatalf("default should lock json-path + unbound, got %+v", l)
		}
	}
}

func TestGrokLockBindsToHome(t *testing.T) {
	proc := t.TempDir()
	write := func(pid int, comm, environ string) {
		t.Helper()
		dir := filepath.Join(proc, strconv.Itoa(pid))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(comm), 0o644); err != nil {
			t.Fatal(err)
		}
		if environ != "" {
			if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(environ), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	defRoot := filepath.Join(t.TempDir(), "default-grok")
	extraRoot := filepath.Join(t.TempDir(), "custom-grok")
	for _, r := range []string{defRoot, extraRoot} {
		if err := os.MkdirAll(filepath.Join(r, "sessions"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(31, "grok", "GROK_HOME="+extraRoot+"\x00")
	write(32, "grok", "")

	old := procRoot
	procRoot = proc
	t.Cleanup(func() { procRoot = old })

	def := newGrok(defRoot, false)
	def.unbound = true
	extra := newGrok(extraRoot, true)

	locks := DetectLocks([]Agent{def, extra})
	byRoot := map[string]Lock{}
	for _, l := range locks {
		byRoot[l.Root] = l
	}
	if l, ok := byRoot[extra.Root()]; !ok || len(l.PIDs) != 1 || l.PIDs[0] != 31 {
		t.Fatalf("extra grok lock %+v", locks)
	}
	if l, ok := byRoot[def.Root()]; !ok {
		t.Fatalf("default grok not locked: %+v", locks)
	} else {
		for _, p := range l.PIDs {
			if p == 31 {
				t.Fatalf("custom-home grok locked the default store: %+v", l)
			}
		}
	}
}

func TestClaudeDirFromCmdline(t *testing.T) {
	got := claudeDirFromCmdline("claude daemon run --json-path /opt/c/daemon.json --log-file /opt/c/daemon.log")
	if got != "/opt/c" {
		t.Fatalf("got %q", got)
	}
	got = claudeDirFromCmdline("claude --json-path=/tmp/x/daemon.json")
	if got != "/tmp/x" {
		t.Fatalf("eq form %q", got)
	}
}

func TestCommMatches(t *testing.T) {
	if !commMatches("muse-bin-0.2.1-", "muse-bin-*") {
		t.Fatal("prefix")
	}
	if commMatches("muse", "muse-bin-*") {
		t.Fatal("should not match muse wrapper comm")
	}
	if !commMatches("qwen", "qwen") {
		t.Fatal("exact")
	}
}

func TestParseProcLock(t *testing.T) {
	pid, ok := parseProcLock("108: FLOCK  ADVISORY  WRITE 3282747 fc:01:4215070 0 EOF", 0xfc, 1, 4215070)
	if !ok || pid != 3282747 {
		t.Fatalf("got pid=%d ok=%v", pid, ok)
	}
	if _, ok := parseProcLock("108: FLOCK  ADVISORY  WRITE 3282747 fc:01:4215070 0 EOF", 0xfc, 1, 1); ok {
		t.Fatal("inode mismatch should not match")
	}
}

func TestLinuxDev(t *testing.T) {
	maj, min := linuxDev(0xfc01)
	if maj != 0xfc || min != 1 {
		t.Fatalf("maj=%#x min=%#x", maj, min)
	}
}
