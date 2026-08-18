package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAddAndRemoveClaudeDir(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("ASM_CONFIG", cfg)

	dir := t.TempDir()
	f, err := AddClaudeDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Dirs("claude")) != 1 {
		t.Fatalf("got %+v", f.Dirs("claude"))
	}
	// idempotent
	f, err = AddClaudeDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Dirs("claude")) != 1 {
		t.Fatalf("dup: %+v", f.Dirs("claude"))
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Dirs("claude")) != 1 || got.Dirs("claude")[0] != f.Dirs("claude")[0] {
		t.Fatalf("load %+v", got)
	}
	if _, err := RemoveClaudeDir(dir); err != nil {
		t.Fatal(err)
	}
	got, _ = Load()
	if len(got.Dirs("claude")) != 0 {
		t.Fatalf("remove leftover %+v", got)
	}
}

func TestAddDirCodexAndRejectAgy(t *testing.T) {
	t.Setenv("ASM_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	dir := t.TempDir()
	f, err := AddDir("codex", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Dirs("codex")) != 1 {
		t.Fatalf("%+v", f)
	}
	if _, err := AddDir("agy", dir); err == nil {
		t.Fatal("agy has no relocatable home")
	}
}

func TestAddClaudeDirRejectsMissing(t *testing.T) {
	t.Setenv("ASM_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if _, err := AddClaudeDir(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	t.Setenv("ASM_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	f, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Dirs("claude")) != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestAddClaudeDirRejectsFile(t *testing.T) {
	t.Setenv("ASM_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AddClaudeDir(p); err == nil {
		t.Fatal("expected error for file")
	}
}
