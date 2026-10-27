package git

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const dirtyWorkRunID = "01JZ8XQ7V6K9M3B0T5N2R4C8YD"

func TestCaptureDirtyWork_RoundTripAfterWorktreeRemoval(t *testing.T) {
	ctx := context.Background()
	gate, wt, head, base := newDirtyWorkGate(t, "work tree")

	writeFile(t, filepath.Join(wt, "file.txt"), "staged\n")
	run(t, wt, "git", "add", "file.txt")
	writeFile(t, filepath.Join(wt, "file.txt"), "unstaged\n")
	if err := os.WriteFile(filepath.Join(wt, "bin.dat"), []byte("bin\x00NEW"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "sub dir", "a b.txt"), "edited name\n")
	if err := os.Remove(filepath.Join(wt, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	run(t, wt, "git", "rm", "staged-del.txt")
	if err := os.MkdirAll(filepath.Join(wt, "sub dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "sub dir", "new file.txt"), "untracked\n")

	beforeStatus := run(t, wt, "git", "status", "--porcelain")
	beforeCached := run(t, wt, "git", "diff", "--cached")
	beforeFile := mustRead(t, filepath.Join(wt, "file.txt"))

	snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{
		GateDir:  gate,
		Worktree: wt,
		RunID:    dirtyWorkRunID,
		BaseSHA:  base,
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if snap == nil || snap.Commit == "" || snap.Ref != DirtyWorkRef(dirtyWorkRunID) || snap.Head != head || snap.Base != base {
		t.Fatalf("snapshot = %#v", snap)
	}
	if got := run(t, wt, "git", "status", "--porcelain"); got != beforeStatus {
		t.Fatalf("capture changed the checkout status:\n%s", got)
	}
	if got := mustRead(t, filepath.Join(wt, "file.txt")); !bytes.Equal(got, beforeFile) {
		t.Fatalf("capture rewrote file.txt: %q", got)
	}

	if err := WorktreeRemove(ctx, gate, wt); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still present: %v", err)
	}
	recorded := run(t, gate, "git", "rev-parse", "--verify", snap.Ref+"^{commit}")
	if recorded != snap.Commit {
		t.Fatalf("gate ref after removal = %s, want %s", recorded, snap.Commit)
	}
	message := run(t, gate, "git", "log", "-1", "--format=%B", snap.Ref)
	for _, want := range []string{"schema: 1", "head: " + head, "base: " + base, "path: both file.txt", "path: untracked sub dir/new file.txt", "path: unstaged sub dir/a b.txt"} {
		if !strings.Contains(message, want) {
			t.Fatalf("manifest missing %q:\n%s", want, message)
		}
	}

	restored := filepath.Join(t.TempDir(), "restored tree")
	if err := WorktreeAdd(ctx, gate, restored, head); err != nil {
		t.Fatalf("add restored worktree: %v", err)
	}
	t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, restored) })
	if err := RestoreDirtyWork(ctx, restored, snap.Commit); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := run(t, restored, "git", "rev-parse", "HEAD"); got != head {
		t.Fatalf("restored HEAD = %s, want %s", got, head)
	}
	if got := run(t, restored, "git", "status", "--porcelain"); got != beforeStatus {
		t.Fatalf("restored status =\n%s\nwant\n%s", got, beforeStatus)
	}
	if got := run(t, restored, "git", "diff", "--cached"); got != beforeCached {
		t.Fatalf("restored index diff =\n%s\nwant\n%s", got, beforeCached)
	}
	if got := mustRead(t, filepath.Join(restored, "file.txt")); !bytes.Equal(got, []byte("unstaged\n")) {
		t.Fatalf("restored file.txt = %q", got)
	}
	if got := mustRead(t, filepath.Join(restored, "bin.dat")); !bytes.Equal(got, []byte("bin\x00NEW")) {
		t.Fatalf("restored binary = %q", got)
	}
	if got := mustRead(t, filepath.Join(restored, "sub dir", "a b.txt")); string(got) != "edited name\n" {
		t.Fatalf("restored spaced path = %q", got)
	}
	if got := mustRead(t, filepath.Join(restored, "sub dir", "new file.txt")); string(got) != "untracked\n" {
		t.Fatalf("restored untracked = %q", got)
	}
	if _, err := os.Stat(filepath.Join(restored, "gone.txt")); !os.IsNotExist(err) {
		t.Fatalf("unstaged deletion was restored as a file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(restored, "staged-del.txt")); !os.IsNotExist(err) {
		t.Fatalf("staged deletion was restored as a file: %v", err)
	}
}

func TestCaptureDirtyWork_PlainDirectoryIsNotACheckout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "leftover")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "notes.txt"), "not a checkout\n")
	snap, err := CaptureDirtyWork(context.Background(), DirtyWorkRequest{
		GateDir:  filepath.Join(t.TempDir(), "missing.git"),
		Worktree: dir,
		RunID:    dirtyWorkRunID,
	})
	if err != nil || snap != nil {
		t.Fatalf("plain directory capture = %#v, %v", snap, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureDirtyWork_UnchangedTreeRemovesWithoutSnapshot(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "clean")
	snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID, BaseSHA: "abc"})
	if err != nil || snap != nil {
		t.Fatalf("clean capture = %#v, %v", snap, err)
	}
	if _, err := Run(ctx, gate, "rev-parse", "--verify", "--quiet", DirtyWorkRef(dirtyWorkRunID)); err == nil {
		t.Fatal("clean checkout created a snapshot ref")
	}
	if err := WorktreeRemove(ctx, gate, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("clean worktree was kept: %v", err)
	}
}

func TestCaptureDirtyWork_CancelledCaptureKeepsWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gate, wt, _, _ := newDirtyWorkGate(t, "cancel")
	writeFile(t, filepath.Join(wt, "file.txt"), "keep me\n")
	_, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(wt, "file.txt")); statErr != nil {
		t.Fatal(statErr)
	}
	if _, err := Run(context.Background(), gate, "rev-parse", "--verify", "--quiet", DirtyWorkRef(dirtyWorkRunID)); err == nil {
		t.Fatal("cancelled capture published a snapshot ref")
	}
}

func TestCaptureDirtyWork_RefWriteFailureKeepsWork(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "io")
	writeFile(t, filepath.Join(wt, "file.txt"), "unique\n")
	block := filepath.Join(gate, "refs", "no-mistakes")
	if err := os.WriteFile(block, []byte("not a ref directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if err == nil || errors.Is(err, ErrDirtyWorkRejected) || errors.Is(err, context.Canceled) {
		t.Fatalf("capture error = %v, want an I/O failure", err)
	}
	if got := mustRead(t, filepath.Join(wt, "file.txt")); string(got) != "unique\n" {
		t.Fatalf("checkout after failed capture = %q", got)
	}
	if _, err := Run(ctx, gate, "rev-parse", "--verify", "--quiet", DirtyWorkRef(dirtyWorkRunID)); err == nil {
		t.Fatal("failed capture published a snapshot ref")
	}
}

func TestCaptureDirtyWork_RejectsSecretBearingWork(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "secrets")
	const marker = "nm-secret-marker-8f3a"
	writeFile(t, filepath.Join(wt, ".env"), marker+"\nTOKEN=value\n")
	_, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if !errors.Is(err, ErrDirtyWorkRejected) {
		t.Fatalf("secret path error = %v", err)
	}
	assertObjectStoreLacks(t, gate, marker)
	if _, statErr := os.Stat(filepath.Join(wt, ".env")); statErr != nil {
		t.Fatal(statErr)
	}

	if err := os.Remove(filepath.Join(wt, ".env")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "notes.txt"), "-----BEGIN OPENSSH PRIVATE KEY-----\n"+marker+"\n")
	_, err = CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if !errors.Is(err, ErrDirtyWorkRejected) {
		t.Fatalf("secret content error = %v", err)
	}
	assertObjectStoreLacks(t, gate, marker)
	if _, err := Run(ctx, gate, "rev-parse", "--verify", "--quiet", DirtyWorkRef(dirtyWorkRunID)); err == nil {
		t.Fatal("rejected capture published a snapshot ref")
	}
}

func TestCaptureDirtyWork_SnapshotsSymlinkText(t *testing.T) {
	ctx := context.Background()
	gate, wt, head, _ := newDirtyWorkGate(t, "links")
	dirLink := filepath.Join(wt, "dirlink")
	if err := os.Symlink("sub dir", dirLink); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink("missing-target", filepath.Join(wt, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "outside-missing"), filepath.Join(wt, "dangling-out")); err != nil {
		t.Fatal(err)
	}

	snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := DirtyWorkStillMatches(ctx, wt, snap); err != nil {
		t.Fatalf("fresh symlink snapshot: %v", err)
	}
	if err := WorktreeRemove(ctx, gate, wt); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	restored := filepath.Join(t.TempDir(), "restored-links")
	if err := WorktreeAdd(ctx, gate, restored, head); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, restored) })
	if err := RestoreDirtyWork(ctx, restored, snap.Commit); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, tc := range []struct{ name, want string }{
		{"dirlink", "sub dir"},
		{"dangling", "missing-target"},
		{"dangling-out", filepath.Join("..", "outside-missing")},
	} {
		got, err := os.Readlink(filepath.Join(restored, tc.name))
		if err != nil {
			t.Fatalf("readlink %s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("restored %s = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCaptureDirtyWork_RejectsSecretPastPrefix(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "long-secret")
	const marker = "nm-late-secret-marker-8f3a"
	body := append(bytes.Repeat([]byte("a"), 256*1024), []byte("-----BEGIN RSA PRIVATE KEY-----\n"+marker+"\n")...)
	if err := os.WriteFile(filepath.Join(wt, "late.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if !errors.Is(err, ErrDirtyWorkRejected) {
		t.Fatalf("late secret error = %v", err)
	}
	assertObjectStoreLacks(t, gate, marker)
	if _, err := Run(ctx, gate, "rev-parse", "--verify", "--quiet", DirtyWorkRef(dirtyWorkRunID)); err == nil {
		t.Fatal("late secret published a snapshot ref")
	}

	if err := os.Remove(filepath.Join(wt, "late.txt")); err != nil {
		t.Fatal(err)
	}
	staged := append(bytes.Repeat([]byte("b"), 256*1024), []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"+marker+"-index\n")...)
	stagedPath := filepath.Join(wt, "staged-late.txt")
	if err := os.WriteFile(stagedPath, staged, 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, wt, "git", "add", "--", "staged-late.txt")
	if err := os.WriteFile(stagedPath, []byte("plain worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if !errors.Is(err, ErrDirtyWorkRejected) {
		t.Fatalf("staged late secret error = %v", err)
	}
	if _, err := Run(ctx, gate, "rev-parse", "--verify", "--quiet", DirtyWorkRef(dirtyWorkRunID)); err == nil {
		t.Fatal("staged late secret published a snapshot ref")
	}
}

func TestReaderContainsSecretFindsMarkerAcrossReads(t *testing.T) {
	marker := []byte("-----BEGIN OPENSSH PRIVATE KEY-----")
	body := append(bytes.Repeat([]byte("a"), 3), marker...)
	body = append(body, []byte("tail")...)
	found, err := readerContainsSecret(context.Background(), &oneByteReader{rest: body})
	if err != nil || !found {
		t.Fatalf("found = %v, err = %v", found, err)
	}
	found, err = readerContainsSecret(context.Background(), &oneByteReader{rest: bytes.Repeat([]byte("a"), len(marker)+5)})
	if err != nil || found {
		t.Fatalf("clean found = %v, err = %v", found, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = readerContainsSecret(ctx, &oneByteReader{rest: bytes.Repeat([]byte("a"), 8)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reader error = %v", err)
	}
}

type oneByteReader struct{ rest []byte }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.rest) == 0 {
		return 0, io.EOF
	}
	p[0] = r.rest[0]
	r.rest = r.rest[1:]
	return 1, nil
}

func TestDirtyWorkStillMatchesRefusesDriftBeforeRemoval(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "drift")
	if err := DirtyWorkStillMatches(ctx, wt, nil); err != nil {
		t.Fatalf("clean checkout: %v", err)
	}
	writeFile(t, filepath.Join(wt, "file.txt"), "edited\n")
	snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := DirtyWorkStillMatches(ctx, wt, snap); err != nil {
		t.Fatalf("unchanged snapshot: %v", err)
	}
	writeFile(t, filepath.Join(wt, "file.txt"), "edited again\n")
	if err := DirtyWorkStillMatches(ctx, wt, snap); err == nil {
		t.Fatal("tracked edit after the snapshot still matched")
	}
	if got := mustRead(t, filepath.Join(wt, "file.txt")); string(got) != "edited again\n" {
		t.Fatalf("drift check rewrote file.txt: %q", got)
	}
	writeFile(t, filepath.Join(wt, "file.txt"), "edited\n")
	writeFile(t, filepath.Join(wt, "extra.txt"), "appeared later\n")
	if err := DirtyWorkStillMatches(ctx, wt, snap); err == nil {
		t.Fatal("untracked file created after the snapshot still matched")
	}
	if err := DirtyWorkStillMatches(ctx, wt, nil); err == nil {
		t.Fatal("nil snapshot matched a dirty checkout")
	}
}

func TestCaptureDirtyWork_RejectsUnrelatedHome(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "owned")
	otherGate, otherWT, _, _ := newDirtyWorkGate(t, "other home")
	const marker = "nm-outside-home-marker-8f3a"
	writeFile(t, filepath.Join(otherWT, "file.txt"), marker+"\n")

	_, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: otherWT, RunID: dirtyWorkRunID})
	if !errors.Is(err, ErrDirtyWorkRejected) {
		t.Fatalf("other checkout error = %v", err)
	}
	assertObjectStoreLacks(t, gate, marker)
	if got := mustRead(t, filepath.Join(otherWT, "file.txt")); string(got) != marker+"\n" {
		t.Fatalf("other checkout = %q", got)
	}

	outside := filepath.Join(t.TempDir(), "other-home.txt")
	writeFile(t, outside, marker+"-link\n")
	link := filepath.Join(wt, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	_, err = CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if !errors.Is(err, ErrDirtyWorkRejected) {
		t.Fatalf("symlink escape error = %v", err)
	}
	assertObjectStoreLacks(t, gate, marker)
	assertObjectStoreLacks(t, otherGate, marker+"-link")
	if _, err := Run(ctx, gate, "rev-parse", "--verify", "--quiet", DirtyWorkRef(dirtyWorkRunID)); err == nil {
		t.Fatal("rejected capture published a snapshot ref")
	}
}

func newDirtyWorkGate(t *testing.T, wtName string) (gate, wt, head, base string) {
	t.Helper()
	ctx := context.Background()
	src := initTestRepo(t)
	base = run(t, src, "git", "rev-parse", "HEAD")
	writeFile(t, filepath.Join(src, "file.txt"), "base\n")
	if err := os.MkdirAll(filepath.Join(src, "sub dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "sub dir", "a b.txt"), "old\n")
	if err := os.WriteFile(filepath.Join(src, "bin.dat"), []byte("bin\x00data"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "gone.txt"), "gone\n")
	writeFile(t, filepath.Join(src, "staged-del.txt"), "delete me\n")
	run(t, src, "git", "add", ".")
	run(t, src, "git", "commit", "-m", "files")
	head = run(t, src, "git", "rev-parse", "HEAD")

	gate = filepath.Join(t.TempDir(), "gate.git")
	if err := InitBare(ctx, gate); err != nil {
		t.Fatal(err)
	}
	run(t, src, "git", "remote", "add", "gate", gate)
	run(t, src, "git", "push", "gate", "HEAD:refs/heads/main")
	wt = filepath.Join(t.TempDir(), wtName)
	if err := WorktreeAdd(ctx, gate, wt, head); err != nil {
		t.Fatal(err)
	}
	run(t, wt, "git", "config", "user.email", "test@test.com")
	run(t, wt, "git", "config", "user.name", "Test")
	run(t, wt, "git", "config", "core.autocrlf", "false")
	return gate, wt, head, base
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return buf
}

func assertObjectStoreLacks(t *testing.T, gate, marker string) {
	t.Helper()
	out, err := RunRaw(context.Background(), gate, "cat-file", "--batch-all-objects", "--batch")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(marker)) {
		t.Fatalf("object store contains %q", marker)
	}
}
