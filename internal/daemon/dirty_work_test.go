package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestRemoveRunWorktreeCapturesDirtyWorkBeforeRemoval(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, headSHA := setupTestGitRepo(t, p, d, "repo1")
	const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YD"
	wtDir := filepath.Join(t.TempDir(), "run worktree")
	gitCmd(t, p.RepoDir("repo1"), "worktree", "add", "--detach", wtDir, headSHA)
	if err := os.WriteFile(filepath.Join(wtDir, "test.txt"), []byte("unique dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	NewRunManager(d, p, nil).removeRunWorktree("repo1", runID, p.RepoDir("repo1"), wtDir, "test")

	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("dirty worktree survived a verified capture, stat err: %v", err)
	}
	gate := p.RepoDir("repo1")
	commit := gitOutput(t, gate, "rev-parse", "--verify", git.DirtyWorkRef(runID)+"^{commit}")
	restored := filepath.Join(t.TempDir(), "restored")
	gitCmd(t, gate, "worktree", "add", "--detach", restored, headSHA)
	if err := git.RestoreDirtyWork(context.Background(), restored, commit); err != nil {
		t.Fatalf("restore after removal: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(restored, "test.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "unique dirty\n" {
		t.Fatalf("restored test.txt = %q", got)
	}
}

func TestRemoveRunWorktreeKeepsDirtyWorkWhenCaptureFails(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, headSHA := setupTestGitRepo(t, p, d, "repo1")
	const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YD"
	wtDir := filepath.Join(t.TempDir(), "run-wt")
	gate := p.RepoDir("repo1")
	gitCmd(t, gate, "worktree", "add", "--detach", wtDir, headSHA)
	if err := os.WriteFile(filepath.Join(wtDir, "test.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gate, "refs", "no-mistakes"), []byte("blocked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	NewRunManager(d, p, nil).removeRunWorktree("repo1", runID, gate, wtDir, "test")

	got, err := os.ReadFile(filepath.Join(wtDir, "test.txt"))
	if err != nil {
		t.Fatalf("failed capture removed the checkout: %v", err)
	}
	if string(got) != "keep me\n" {
		t.Fatalf("checkout contents = %q", got)
	}
}

func TestRemoveOrphanWorktreeKeepsDirtyWorkWhenCaptureFails(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, headSHA := setupTestGitRepo(t, p, d, "repo1")
	const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YD"
	wtDir := filepath.Join(t.TempDir(), "orphan-wt")
	gate := p.RepoDir("repo1")
	gitCmd(t, gate, "worktree", "add", "--detach", wtDir, headSHA)
	if err := os.WriteFile(filepath.Join(wtDir, "test.txt"), []byte("orphan unique\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gate, "refs", "no-mistakes"), []byte("blocked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed := removeOrphanWorktree(context.Background(), orphanWorktree{
		gateDir: gate,
		dir:     wtDir,
		repoID:  "repo1",
		runID:   runID,
	})
	if removed {
		t.Fatal("orphan removal deleted a checkout whose capture failed")
	}
	got, err := os.ReadFile(filepath.Join(wtDir, "test.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "orphan unique\n" {
		t.Fatalf("orphan checkout = %q", got)
	}
}
