package agents

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

func TestExportClaudeToQwen(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "aaaaaaaa-1111-2222-3333-444444444444"
	seedClaude(t, home, cwd, id)

	src := NewClaude(home)
	dst := NewQwen(home)
	ss, err := src.Scan(context.Background())
	if err != nil || len(ss) != 1 {
		t.Fatalf("scan: %v %+v", err, ss)
	}
	plan, err := TransferPlan(src, dst, ss[0], TransferExport)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Transfer != TransferExport || plan.NewID == "" || plan.NewID == id {
		t.Fatalf("plan meta: %+v", plan)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}

	// Source still listed.
	ss, _ = src.Scan(context.Background())
	if len(ss) != 1 || ss[0].ID != id {
		t.Fatalf("source should remain: %+v", ss)
	}
	got, err := dst.Scan(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("dest scan: %v %+v", err, got)
	}
	if got[0].ID == id {
		t.Fatal("dest reused source id")
	}
	if got[0].Title != "Fix the widget" {
		t.Fatalf("dest title = %q", got[0].Title)
	}
	if got[0].Cwd != cwd {
		t.Fatalf("dest cwd = %q", got[0].Cwd)
	}
}

func TestMigrateClaudeToCodexArchivesSource(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "aaaaaaaa-1111-2222-3333-444444444444"
	seedClaude(t, home, cwd, id)

	src := NewClaude(home)
	dst := NewCodex(home)
	ss, _ := src.Scan(context.Background())
	plan, err := TransferPlan(src, dst, ss[0], TransferMigrate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	ss, _ = src.Scan(context.Background())
	if len(ss) != 0 {
		t.Fatalf("source should be archived: %+v", ss)
	}
	got, err := dst.Scan(context.Background())
	if err != nil || len(got) != 1 || got[0].Title != "Fix the widget" {
		t.Fatalf("dest: %v %+v", err, got)
	}
}

func TestMigrateSameStoreRefused(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	seedClaude(t, home, cwd, "aaaaaaaa-1111-2222-3333-444444444444")
	a := NewClaude(home)
	ss, _ := a.Scan(context.Background())
	if _, err := TransferPlan(a, a, ss[0], TransferMigrate); err == nil {
		t.Fatal("expected refuse")
	}
	plan, err := TransferPlan(a, a, ss[0], TransferExport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	ss, _ = a.Scan(context.Background())
	if len(ss) != 2 {
		t.Fatalf("export to self should duplicate, got %d", len(ss))
	}
}

func TestExportQwenToGrokRoundTrip(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	os.MkdirAll(cwd, 0o755)
	id := "6d5dd3d9-01b8-4df0-a6a5-47391860d6f3"
	seedQwen(t, home, cwd, id, "hi from qwen")
	src := NewQwen(home)
	dst := NewGrok(home)
	ss, _ := src.Scan(context.Background())
	plan, err := TransferPlan(src, dst, ss[0], TransferExport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(plan, filepath.Join(home, "backups")); err != nil {
		t.Fatal(err)
	}
	got, err := dst.Scan(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("grok scan: %v %+v", err, got)
	}
	if got[0].Title != "hi from qwen" || got[0].Kind != model.Grok {
		t.Fatalf("grok session: %+v", got[0])
	}
	tr, err := ExtractTranscript(dst, got[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Messages) < 1 {
		t.Fatalf("re-extract empty: %+v", tr)
	}
}
