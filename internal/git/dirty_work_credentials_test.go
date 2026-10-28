package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureDirtyWorkRejectsConventionalCredentialFiles(t *testing.T) {
	for _, rel := range []string{".npmrc", ".git-credentials", "sub dir/.npmrc", "sub dir/.GIT-CREDENTIALS"} {
		for _, state := range []string{"untracked", "tracked", "staged"} {
			t.Run(rel+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				gate, wt, _, _ := newDirtyWorkGate(t, "credential capture")
				path := filepath.Join(wt, filepath.FromSlash(rel))
				if state == "tracked" {
					writeFile(t, path, "committed placeholder\n")
					run(t, wt, "git", "add", "--", rel)
					run(t, wt, "git", "commit", "-m", "placeholder")
				}
				const marker = "synthetic-capture-credential-12345"
				body := []byte("//registry.npmjs.org/:_authToken=" + marker + "\n")
				if strings.Contains(strings.ToLower(rel), "git-credentials") {
					body = []byte("https://fixture:" + marker + "@example.invalid/blob\n")
				}
				if err := os.WriteFile(path, body, 0o644); err != nil {
					t.Fatal(err)
				}
				if state == "staged" {
					run(t, wt, "git", "add", "--", rel)
				}
				_, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
				if !errors.Is(err, ErrDirtyWorkRejected) {
					t.Fatal("credential capture was not refused")
				}
				assertNoDirtySnapshot(t, gate)
				if got := mustRead(t, path); !bytes.Equal(got, body) {
					t.Fatal("refusal changed source credential bytes")
				}
				if state != "staged" {
					assertObjectStoreLacks(t, gate, marker)
				} else {
					staged, err := RunRaw(ctx, wt, "show", ":"+rel)
					if err != nil || !bytes.Equal(staged, body) {
						t.Fatal("refusal changed staged credential bytes")
					}
				}
			})
		}
	}
}

func TestWriteDirtySnapshotRejectsCredentialFilesAddedAfterStatus(t *testing.T) {
	for _, rel := range []string{".npmrc", ".git-credentials"} {
		t.Run(rel, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, _, _ := newDirtyWorkGate(t, "late credentials")
			writeFile(t, filepath.Join(wt, "file.txt"), "ordinary unique bytes\n")
			entries, err := checkoutDirtyEntries(ctx, wt)
			if err != nil {
				t.Fatal(err)
			}
			body := "//registry.npmjs.org/:_authToken=synthetic-index-credential-12345\n"
			if rel == ".git-credentials" {
				body = "https://fixture:synthetic-index-credential-12345@example.invalid/blob\n"
			}
			path := filepath.Join(wt, rel)
			writeFile(t, path, body)
			run(t, wt, "git", "add", "--", rel)
			writeFile(t, path, "worktree replacement\n")
			_, err = writeDirtySnapshot(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID}, DirtyWorkRef(dirtyWorkRunID), entries)
			if !errors.Is(err, ErrDirtyWorkRejected) {
				t.Fatal("copied-index credential path was not refused")
			}
			assertNoDirtySnapshot(t, gate)
			if got := string(mustRead(t, path)); got != "worktree replacement\n" {
				t.Fatal("refusal changed working bytes")
			}
			if staged, err := RunRaw(ctx, wt, "show", ":"+rel); err != nil || string(staged) != body {
				t.Fatal("refusal changed copied-index bytes")
			}
		})
	}
}
