package agents

import (
	"os"
	"path/filepath"
	"testing"

	"agents-session-manager/internal/model"
)

func TestLooksLikeHome(t *testing.T) {
	for kind, marker := range map[model.Kind]string{
		model.Claude: "projects",
		model.Codex:  "sessions",
		model.Grok:   "sessions",
		model.Qwen:   "projects",
		model.Muse:   "sessions",
	} {
		dir := t.TempDir()
		if LooksLikeHome(kind, dir) {
			t.Fatalf("%s: empty dir should not look like a home", kind)
		}
		if err := os.Mkdir(filepath.Join(dir, marker), 0o755); err != nil {
			t.Fatal(err)
		}
		if !LooksLikeHome(kind, dir) {
			t.Fatalf("%s: dir with %s/ should look like a home", kind, marker)
		}
	}
	if LooksLikeHome(model.Agy, t.TempDir()) {
		t.Fatal("agy has no relocatable home")
	}
}
