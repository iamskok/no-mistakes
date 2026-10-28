package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDirtyWorkScratchCleanupUsesCaptureContext(t *testing.T) {
	for _, phase := range []string{"write", "confirm", "clean"} {
		t.Run(phase, func(t *testing.T) {
			gate, wt, _, _ := newDirtyWorkGate(t, "cancelled scratch")
			var snap *DirtyWorkSnapshot
			if phase != "clean" {
				writeFile(t, filepath.Join(wt, "file.txt"), "unique retained bytes\n")
			}
			if phase == "confirm" {
				var err error
				snap, err = CaptureDirtyWork(context.Background(), DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
				if err != nil || snap == nil {
					t.Fatalf("initial capture = %#v, %v", snap, err)
				}
			}
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			t.Setenv("TMP", root)
			t.Setenv("TEMP", root)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			previousRef := dirtyWorkBeforeRefUpdate
			previousCheck := dirtyWorkAfterBookkeeping
			t.Cleanup(func() {
				dirtyWorkBeforeRefUpdate = previousRef
				dirtyWorkAfterBookkeeping = previousCheck
			})
			var retained string
			stop := func() {
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 1 || !entries[0].IsDir() {
					t.Fatal("capture did not create its isolated scratch directory")
				}
				retained = filepath.Join(root, entries[0].Name(), "unfinished.txt")
				writeFile(t, retained, "retained scratch bytes\n")
				cancel()
			}
			if phase == "write" {
				dirtyWorkBeforeRefUpdate = stop
			} else {
				dirtyWorkAfterBookkeeping = func(path string) {
					if path == wt {
						stop()
					}
				}
			}
			var err error
			if phase == "confirm" {
				err = DirtyWorkStillMatches(ctx, wt, snap)
			} else {
				_, err = CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
			}
			if err == nil || retained == "" || ctx.Err() == nil {
				t.Fatalf("capture did not observe cancellation: %v", err)
			}
			if got := string(mustRead(t, retained)); got != "retained scratch bytes\n" {
				t.Fatal("cancelled cleanup changed unfinished scratch bytes")
			}
			if phase != "confirm" {
				assertNoDirtySnapshot(t, gate)
			}
			if phase != "clean" && string(mustRead(t, filepath.Join(wt, "file.txt"))) != "unique retained bytes\n" {
				t.Fatal("cancelled cleanup changed source work")
			}
		})
	}
}
