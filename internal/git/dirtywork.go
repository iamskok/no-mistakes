package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// DirtyWorkRefPrefix is the gate-local ref namespace for a checkout's
// uncommitted work. It is not a custody recovery anchor: those live under
// refs/no-mistakes/recover/ and record commits, which cannot represent an
// index, unstaged edits, or untracked files.
const DirtyWorkRefPrefix = "refs/no-mistakes/dirty/"

// ErrDirtyWorkRejected means the checkout has unique uncommitted work that
// must not be copied into the snapshot. Callers keep the checkout.
var ErrDirtyWorkRejected = errors.New("rejected dirty work capture")

// DirtyWorkRequest names the checkout to snapshot and the repository that
// keeps the snapshot after that checkout is removed.
type DirtyWorkRequest struct {
	// GateDir is the repository that must own Worktree and that stores the
	// snapshot ref. For a run checkout this is the bare gate.
	GateDir string
	// Worktree is the checkout about to be removed.
	Worktree string
	// RunID becomes the final component of the snapshot ref.
	RunID string
	// BaseSHA is the run's recorded base when the caller knows it. It is
	// stored in the snapshot message and is not required to exist as an object.
	BaseSHA string
}

// DirtyWorkSnapshot is a verified stash-shaped commit stored outside the
// checkout. Its first parent is the checkout HEAD, its second parent records
// the index, and a third parent records owned untracked files when any exist.
// Restore it with RestoreDirtyWork, or by checking out the first parent and
// running `git stash apply --index <commit>`.
type DirtyWorkSnapshot struct {
	Ref    string
	Commit string
	Head   string
	Base   string
}

// DirtyWorkRef is the gate ref for one run's uncommitted snapshot.
func DirtyWorkRef(runID string) string {
	return DirtyWorkRefPrefix + runID
}

// CaptureDirtyWork snapshots staged changes, unstaged changes, and untracked
// files that are not ignored. A clean checkout returns a nil snapshot and a
// nil error. The snapshot ref is created only after the trees match the
// checkout; any error, including cancellation, means the checkout must stay.
// Secret-bearing paths and paths that resolve outside the checkout are
// refused before any of their bytes are written into the object store.
// Symlinks are stored as their link text. Sockets, fifos, and devices are
// refused without being opened.
func CaptureDirtyWork(ctx context.Context, req DirtyWorkRequest) (*DirtyWorkSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wt := req.Worktree
	info, err := os.Stat(wt)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat checkout: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: checkout is not a directory", ErrDirtyWorkRejected)
	}
	// A leftover directory that is not a checkout has no index or untracked
	// work to snapshot. Callers still remove it, including the orphan sweep's
	// directory fallback when git worktree remove cannot.
	if _, err := os.Lstat(filepath.Join(wt, ".git")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat checkout git dir: %w", err)
	}
	if err := assertCheckoutOwned(ctx, req.GateDir, wt); err != nil {
		return nil, err
	}

	entries, err := porcelainEntries(ctx, wt)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	ref := DirtyWorkRef(req.RunID)
	if _, err := Run(ctx, req.GateDir, "check-ref-format", ref); err != nil {
		return nil, fmt.Errorf("snapshot ref %s: %w", ref, err)
	}
	paths, err := loadDirtyEntries(ctx, wt, entries, true)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	snap, err := writeDirtySnapshot(ctx, req, ref, paths)
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// DirtyWorkStillMatches reports whether worktree still has exactly the
// uncommitted work snap recorded. A nil snapshot matches only a clean
// checkout, or a directory that is not a checkout. Callers invoke it
// immediately before deleting the checkout; any error means it must stay.
func DirtyWorkStillMatches(ctx context.Context, worktree string, snap *DirtyWorkSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(worktree)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: checkout is not a directory", ErrDirtyWorkRejected)
	}
	if _, err := os.Lstat(filepath.Join(worktree, ".git")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat checkout git dir: %w", err)
	}
	if snap == nil {
		return assertCheckoutClean(ctx, worktree)
	}
	head, indexTree, workTree, untrackedTree, err := readSnapshotTrees(ctx, worktree, snap.Commit)
	if err != nil {
		return err
	}
	if head != snap.Head {
		return fmt.Errorf("dirty work snapshot head does not match the recorded checkout")
	}
	tmp, err := os.MkdirTemp("", "nm-dirty-work-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	return recheckDirtyWork(ctx, worktree, tmp, head, indexTree, workTree, untrackedTree)
}

// RestoreDirtyWork reapplies a snapshot onto a checkout already at the
// snapshot's recorded HEAD. The index and the working tree are both restored.
func RestoreDirtyWork(ctx context.Context, worktree, commit string) error {
	head, err := Run(ctx, worktree, "rev-parse", "--verify", commit+"^1")
	if err != nil {
		return fmt.Errorf("read snapshot head: %w", err)
	}
	current, err := Run(ctx, worktree, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("read checkout head: %w", err)
	}
	if current != head {
		return fmt.Errorf("checkout HEAD %s is not the snapshot head %s", current, head)
	}
	if _, err := Run(ctx, worktree, "stash", "apply", "--index", commit); err != nil {
		return fmt.Errorf("apply dirty work snapshot: %w", err)
	}
	return nil
}

type dirtyEntry struct {
	code string
	rel  string
	kind string
}

func parsePorcelainZ(out []byte) ([]dirtyEntry, error) {
	if len(out) == 0 {
		return nil, nil
	}
	var entries []dirtyEntry
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		if len(raw) < 4 || raw[2] != ' ' {
			return nil, fmt.Errorf("%w: unreadable status entry", ErrDirtyWorkRejected)
		}
		code := string(raw[:2])
		if code == "R " || code == "C " || strings.HasPrefix(code, "R") || strings.HasPrefix(code, "C") {
			return nil, fmt.Errorf("%w: rename or copy in status; refusing a partial snapshot", ErrDirtyWorkRejected)
		}
		entries = append(entries, dirtyEntry{code: code, rel: string(raw[3:])})
	}
	return entries, nil
}

func porcelainEntries(ctx context.Context, wt string) ([]dirtyEntry, error) {
	status, err := RunRaw(ctx, wt, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	return parsePorcelainZ(status)
}

func readDirtyEntries(ctx context.Context, wt string, screen bool) ([]dirtyEntry, error) {
	entries, err := porcelainEntries(ctx, wt)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	return loadDirtyEntries(ctx, wt, entries, screen)
}

func loadDirtyEntries(ctx context.Context, wt string, entries []dirtyEntry, screen bool) ([]dirtyEntry, error) {
	root := canonicalPath(wt)
	for i := range entries {
		rel, err := cleanCheckoutRel(entries[i].rel)
		if err != nil {
			return nil, err
		}
		entries[i].rel = rel
		full := filepath.Join(wt, rel)
		if err := assertPathOwned(root, full); err != nil {
			return nil, fmt.Errorf("%w: %s", ErrDirtyWorkRejected, err)
		}
		info, lerr := os.Lstat(full)
		switch {
		case lerr != nil && !errors.Is(lerr, os.ErrNotExist):
			return nil, fmt.Errorf("stat %s: %w", rel, lerr)
		case lerr == nil && !snapshotableFile(info):
			return nil, notSnapshotable(rel, info)
		}
		if screen {
			secret, err := secretBearing(ctx, wt, entries[i], full)
			if err != nil {
				return nil, err
			}
			if secret {
				return nil, fmt.Errorf("%w: %s is secret-bearing", ErrDirtyWorkRejected, rel)
			}
		}
		entries[i].kind = dirtyKind(entries[i].code)
	}
	return entries, nil
}

func snapshotableFile(info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	return info.Mode().IsRegular()
}

func notSnapshotable(rel string, info os.FileInfo) error {
	if info.IsDir() {
		return fmt.Errorf("%w: %s is a directory and cannot be snapshotted as a file", ErrDirtyWorkRejected, rel)
	}
	return fmt.Errorf("%w: %s is not a regular file", ErrDirtyWorkRejected, rel)
}

func dirtyKind(code string) string {
	if code == "??" {
		return "untracked"
	}
	index := code[0] != ' ' && code[0] != '?'
	work := code[1] != ' ' && code[1] != '?'
	switch {
	case index && work:
		return "both"
	case index:
		return "staged"
	default:
		return "unstaged"
	}
}

func assertCheckoutOwned(ctx context.Context, gateDir, wt string) error {
	if strings.TrimSpace(gateDir) == "" {
		return fmt.Errorf("%w: snapshot repository is missing", ErrDirtyWorkRejected)
	}
	gateCommon, err := gitCommonDir(ctx, gateDir)
	if err != nil {
		return fmt.Errorf("snapshot repository: %w", err)
	}
	wtCommon, err := gitCommonDir(ctx, wt)
	if err != nil {
		return fmt.Errorf("checkout repository: %w", err)
	}
	if gateCommon != wtCommon {
		return fmt.Errorf("%w: worktree is not a checkout of this repository", ErrDirtyWorkRejected)
	}
	if pathWithin(canonicalPath(wt), gateCommon) {
		return fmt.Errorf("%w: snapshot repository is inside the checkout being removed", ErrDirtyWorkRejected)
	}
	return nil
}

func gitCommonDir(ctx context.Context, dir string) (string, error) {
	out, err := Run(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return canonicalPath(out), nil
}

func assertPathOwned(root, full string) error {
	parent := filepath.Dir(full)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("%s escapes the checkout", filepath.Base(full))
	}
	lexical := filepath.Join(resolvedParent, filepath.Base(full))
	if !pathWithin(root, lexical) {
		return fmt.Errorf("%s escapes the checkout", filepath.Base(full))
	}
	info, err := os.Lstat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	target, err := filepath.EvalSymlinks(full)
	if err != nil {
		return nil
	}
	if !pathWithin(root, target) {
		return fmt.Errorf("%s escapes the checkout", filepath.Base(full))
	}
	return nil
}

func cleanCheckoutRel(rel string) (string, error) {
	if rel == "" || strings.ContainsAny(rel, "\x00\n\r\t") {
		return "", fmt.Errorf("%w: dirty path is not a single checkout-relative path", ErrDirtyWorkRejected)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: dirty path is absolute", ErrDirtyWorkRejected)
	}
	cleaned := filepath.Clean(filepath.FromSlash(rel))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: dirty path leaves the checkout", ErrDirtyWorkRejected)
	}
	return cleaned, nil
}

func secretBearing(ctx context.Context, wt string, entry dirtyEntry, full string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if secretPath(entry.rel) && entryHasBytes(ctx, wt, entry, full) {
		return true, nil
	}
	if indexDiffers(entry.code) {
		secret, err := indexBlobContainsSecret(ctx, wt, entry.rel)
		if err != nil {
			return false, err
		}
		if secret {
			return true, nil
		}
	}
	info, err := os.Lstat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return false, err
		}
		return bytesContainSecret([]byte(target)), nil
	}
	if !info.Mode().IsRegular() {
		return false, notSnapshotable(entry.rel, info)
	}
	return regularFileContainsSecret(ctx, full)
}

func indexDiffers(code string) bool {
	return code != "??" && code[0] != ' ' && code[0] != 'D' && code[0] != '?'
}

func entryHasBytes(ctx context.Context, wt string, entry dirtyEntry, full string) bool {
	if _, err := os.Lstat(full); err == nil {
		return true
	}
	if entry.code == "??" || entry.code[0] == 'D' || entry.code[0] == ' ' || entry.code[0] == '?' {
		return false
	}
	_, err := Run(ctx, wt, "cat-file", "-e", ":"+entry.rel)
	return err == nil
}

func secretPath(rel string) bool {
	slashed := filepath.ToSlash(rel)
	base := strings.ToLower(filepath.Base(slashed))
	switch base {
	case ".env", "credentials", "credentials.json", "credentials.yml", "credentials.yaml",
		"secrets.json", "secrets.yaml", "secrets.yml",
		"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519":
		return true
	}
	if strings.HasPrefix(base, ".env.") && base != ".env.example" && base != ".env.sample" && base != ".env.template" {
		return true
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".pem", ".p12", ".pfx":
		return true
	}
	for _, part := range strings.Split(slashed, "/") {
		switch strings.ToLower(part) {
		case ".ssh", ".aws", ".gnupg":
			return true
		}
	}
	return false
}

var privateKeyMarkers = [][]byte{
	[]byte("-----BEGIN PRIVATE KEY-----"),
	[]byte("-----BEGIN RSA PRIVATE KEY-----"),
	[]byte("-----BEGIN OPENSSH PRIVATE KEY-----"),
	[]byte("-----BEGIN EC PRIVATE KEY-----"),
	[]byte("-----BEGIN DSA PRIVATE KEY-----"),
	[]byte("-----BEGIN PGP PRIVATE KEY BLOCK-----"),
}

const secretReadChunk = 32 * 1024

func secretMarkerOverlap() int {
	n := 0
	for _, marker := range privateKeyMarkers {
		if len(marker)-1 > n {
			n = len(marker) - 1
		}
	}
	return n
}

func bytesContainSecret(buf []byte) bool {
	for _, marker := range privateKeyMarkers {
		if bytes.Contains(buf, marker) {
			return true
		}
	}
	return false
}

func regularFileContainsSecret(ctx context.Context, full string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f, err := os.Open(full)
	if err != nil {
		return false, err
	}
	defer f.Close()
	return readerContainsSecret(ctx, f)
}

func readerContainsSecret(ctx context.Context, r io.Reader) (bool, error) {
	overlap := secretMarkerOverlap()
	buf := make([]byte, secretReadChunk)
	var tail []byte
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, err := r.Read(buf)
		if n > 0 {
			window := make([]byte, 0, len(tail)+n)
			window = append(window, tail...)
			window = append(window, buf[:n]...)
			if bytesContainSecret(window) {
				return true, nil
			}
			if len(window) > overlap {
				tail = append([]byte(nil), window[len(window)-overlap:]...)
			} else {
				tail = append([]byte(nil), window...)
			}
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

func indexBlobContainsSecret(ctx context.Context, dir, rel string) (bool, error) {
	if _, err := Run(ctx, dir, "cat-file", "-e", ":"+rel); err != nil {
		return false, fmt.Errorf("index blob %s: %w", rel, err)
	}
	found, err := gitStreamContainsSecret(ctx, dir, "cat-file", "blob", ":"+rel)
	if err != nil {
		return false, fmt.Errorf("read index %s: %w", rel, err)
	}
	return found, nil
}

func gitStreamContainsSecret(ctx context.Context, dir string, args ...string) (bool, error) {
	if isBareGitDir(dir) {
		args = append([]string{"--git-dir=" + dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = nonInteractiveEnvForContext(ctx, dir)
	winproc.Harden(cmd)
	shellenv.ConfigureShellCommand(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return false, err
	}
	found, readErr := readerContainsSecret(ctx, stdout)
	if found || readErr != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, readErr
	}
	if found {
		return true, nil
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), waitErr, strings.TrimSpace(stderr.String()))
	}
	return false, nil
}

func writeDirtySnapshot(ctx context.Context, req DirtyWorkRequest, ref string, paths []dirtyEntry) (*DirtyWorkSnapshot, error) {
	wt := req.Worktree
	head, err := Run(ctx, wt, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("read checkout head: %w", err)
	}
	tmp, err := os.MkdirTemp("", "nm-dirty-work-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	indexPath, err := absoluteGitPath(ctx, wt, "index")
	if err != nil {
		return nil, err
	}
	indexCopy := filepath.Join(tmp, "index")
	if err := copyFile(indexCopy, indexPath); err != nil {
		return nil, err
	}
	indexEnv := []string{"GIT_INDEX_FILE=" + indexCopy}
	indexTree, err := RunWithEnv(ctx, wt, indexEnv, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("write index tree: %w", err)
	}
	liveTree, err := Run(ctx, wt, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("read live index: %w", err)
	}
	if liveTree != indexTree {
		return nil, fmt.Errorf("index changed while capturing dirty work")
	}

	workTree, err := buildWorkTree(ctx, wt, filepath.Join(tmp, "worktree-index"))
	if err != nil {
		return nil, err
	}
	liveAgain, err := Run(ctx, wt, "write-tree")
	if err != nil {
		return nil, err
	}
	if liveAgain != indexTree {
		return nil, fmt.Errorf("capture changed the checkout index")
	}

	var untracked []dirtyEntry
	for _, path := range paths {
		if path.code == "??" {
			untracked = append(untracked, path)
		}
	}
	untrackedTree := ""
	untrackedCommit := ""
	ident := dirtyWorkIdentity()
	if len(untracked) > 0 {
		untrackedTree, err = writeUntrackedTree(ctx, wt, tmp, untracked)
		if err != nil {
			return nil, err
		}
		untrackedCommit, err = RunWithEnv(ctx, wt, ident, "commit-tree", untrackedTree, "-m", "untracked files")
		if err != nil {
			return nil, fmt.Errorf("commit untracked tree: %w", err)
		}
	}
	indexCommit, err := RunWithEnv(ctx, wt, ident, "commit-tree", indexTree, "-p", head, "-m", "index")
	if err != nil {
		return nil, fmt.Errorf("commit index tree: %w", err)
	}
	message := dirtyWorkMessage(head, req.BaseSHA, indexTree, workTree, untrackedTree, paths)
	msgPath := filepath.Join(tmp, "message")
	if err := os.WriteFile(msgPath, []byte(message), 0o644); err != nil {
		return nil, err
	}
	args := []string{"commit-tree", workTree, "-p", head, "-p", indexCommit}
	if untrackedCommit != "" {
		args = append(args, "-p", untrackedCommit)
	}
	args = append(args, "-F", msgPath)
	commit, err := RunWithEnv(ctx, wt, ident, args...)
	if err != nil {
		return nil, fmt.Errorf("commit dirty work snapshot: %w", err)
	}
	if err := verifyDirtySnapshot(ctx, wt, commit, head, indexCommit, untrackedCommit, indexTree, workTree, message); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := Run(ctx, req.GateDir, "cat-file", "-t", commit); err != nil {
		return nil, fmt.Errorf("snapshot is not in the gate repository: %w", err)
	}
	if err := recheckDirtyWork(ctx, wt, tmp, head, indexTree, workTree, untrackedTree); err != nil {
		return nil, err
	}
	if _, err := Run(ctx, req.GateDir, "update-ref", ref, commit); err != nil {
		return nil, fmt.Errorf("record dirty work snapshot: %w", err)
	}
	recorded, err := Run(ctx, req.GateDir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("read dirty work snapshot ref: %w", err)
	}
	if recorded != commit {
		return nil, fmt.Errorf("dirty work snapshot ref recorded %s, verified %s", recorded, commit)
	}
	body, err := Run(ctx, req.GateDir, "log", "-1", "--format=%B", ref)
	if err != nil {
		return nil, fmt.Errorf("read dirty work snapshot from the gate: %w", err)
	}
	if strings.TrimSpace(body) != strings.TrimSpace(message) {
		return nil, fmt.Errorf("gate snapshot message does not match the verified manifest")
	}
	base := req.BaseSHA
	if base == "" {
		base = "-"
	}
	return &DirtyWorkSnapshot{Ref: ref, Commit: commit, Head: head, Base: base}, nil
}

func writeUntrackedTree(ctx context.Context, wt, tmp string, paths []dirtyEntry) (string, error) {
	index := filepath.Join(tmp, "untracked-index")
	if err := os.WriteFile(index, nil, 0o644); err != nil {
		return "", err
	}
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := RunWithEnv(ctx, wt, env, "read-tree", "--empty"); err != nil {
		return "", fmt.Errorf("prepare untracked index: %w", err)
	}
	var info strings.Builder
	for _, path := range paths {
		full := filepath.Join(wt, path.rel)
		mode, blob, err := hashUntracked(ctx, wt, full)
		if err != nil {
			return "", fmt.Errorf("hash %s: %w", path.rel, err)
		}
		fmt.Fprintf(&info, "%s %s\t%s\n", mode, blob, filepath.ToSlash(path.rel))
	}
	if _, err := runInDirWithEnvAndInput(ctx, wt, env, info.String(), "update-index", "--index-info"); err != nil {
		return "", fmt.Errorf("record untracked files: %w", err)
	}
	tree, err := RunWithEnv(ctx, wt, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write untracked tree: %w", err)
	}
	for _, path := range paths {
		got, err := RunWithEnv(ctx, wt, env, "rev-parse", "--verify", tree+":"+filepath.ToSlash(path.rel))
		if err != nil {
			return "", fmt.Errorf("untracked snapshot missing %s: %w", path.rel, err)
		}
		full := filepath.Join(wt, path.rel)
		want, err := hashFileBlob(ctx, wt, full)
		if err != nil {
			return "", err
		}
		if got != want {
			return "", fmt.Errorf("untracked snapshot of %s does not match the checkout", path.rel)
		}
	}
	return tree, nil
}

func hashUntracked(ctx context.Context, wt, full string) (mode, blob string, err error) {
	info, err := os.Lstat(full)
	if err != nil {
		return "", "", err
	}
	if !snapshotableFile(info) {
		return "", "", notSnapshotable(filepath.Base(full), info)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return "", "", err
		}
		if bytesContainSecret([]byte(target)) {
			return "", "", fmt.Errorf("%w: %s is secret-bearing", ErrDirtyWorkRejected, filepath.Base(full))
		}
		blob, err = runInDirWithEnvAndInput(ctx, wt, nil, target, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", "", err
		}
		return "120000", blob, nil
	}
	secret, err := regularFileContainsSecret(ctx, full)
	if err != nil {
		return "", "", err
	}
	if secret {
		return "", "", fmt.Errorf("%w: %s is secret-bearing", ErrDirtyWorkRejected, filepath.Base(full))
	}
	mode = "100644"
	if info.Mode()&0o111 != 0 {
		mode = "100755"
	}
	blob, err = Run(ctx, wt, "hash-object", "-w", "--", full)
	if err != nil {
		return "", "", err
	}
	return mode, blob, nil
}

func hashFileBlob(ctx context.Context, wt, full string) (string, error) {
	info, err := os.Lstat(full)
	if err != nil {
		return "", err
	}
	if !snapshotableFile(info) {
		return "", notSnapshotable(filepath.Base(full), info)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return "", err
		}
		return runInDirWithEnvAndInput(ctx, wt, nil, target, "hash-object", "--stdin")
	}
	return Run(ctx, wt, "hash-object", "--", full)
}

func buildWorkTree(ctx context.Context, wt, indexFile string) (string, error) {
	if _, err := readDirtyEntries(ctx, wt, true); err != nil {
		return "", err
	}
	indexPath, err := absoluteGitPath(ctx, wt, "index")
	if err != nil {
		return "", err
	}
	if err := copyFile(indexFile, indexPath); err != nil {
		return "", err
	}
	env := []string{"GIT_INDEX_FILE=" + indexFile}
	if _, err := RunWithEnv(ctx, wt, env, "add", "-u"); err != nil {
		return "", fmt.Errorf("snapshot tracked worktree: %w", err)
	}
	if _, err := RunWithEnv(ctx, wt, env, "diff-files", "--quiet"); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("tracked worktree snapshot does not match the checkout: %w", err)
	}
	tree, err := RunWithEnv(ctx, wt, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write worktree tree: %w", err)
	}
	return tree, nil
}

func recheckDirtyWork(ctx context.Context, wt, tmp, head, indexTree, workTree, untrackedTree string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := Run(ctx, wt, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("read checkout head: %w", err)
	}
	if current != head {
		return fmt.Errorf("checkout HEAD changed while capturing dirty work")
	}
	live, err := Run(ctx, wt, "write-tree")
	if err != nil {
		return fmt.Errorf("read live index: %w", err)
	}
	if live != indexTree {
		return fmt.Errorf("index changed while capturing dirty work")
	}
	gotWork, err := buildWorkTree(ctx, wt, filepath.Join(tmp, "recheck-worktree-index"))
	if err != nil {
		return err
	}
	if gotWork != workTree {
		return fmt.Errorf("tracked worktree changed while capturing dirty work")
	}
	liveAgain, err := Run(ctx, wt, "write-tree")
	if err != nil {
		return err
	}
	if liveAgain != indexTree {
		return fmt.Errorf("capture changed the checkout index")
	}
	entries, err := readDirtyEntries(ctx, wt, true)
	if err != nil {
		return err
	}
	var untracked []dirtyEntry
	for _, entry := range entries {
		if entry.code == "??" {
			untracked = append(untracked, entry)
		}
	}
	if untrackedTree == "" {
		if len(untracked) != 0 {
			return fmt.Errorf("untracked files appeared while capturing dirty work")
		}
		return nil
	}
	if len(untracked) == 0 {
		return fmt.Errorf("untracked files disappeared while capturing dirty work")
	}
	got, err := writeUntrackedTree(ctx, wt, tmp, untracked)
	if err != nil {
		return err
	}
	if got != untrackedTree {
		return fmt.Errorf("untracked files changed while capturing dirty work")
	}
	return nil
}

func assertCheckoutClean(ctx context.Context, wt string) error {
	entries, err := readDirtyEntries(ctx, wt, false)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("checkout changed after the dirty work snapshot")
	}
	return nil
}

func readSnapshotTrees(ctx context.Context, wt, commit string) (head, indexTree, workTree, untrackedTree string, err error) {
	line, err := Run(ctx, wt, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return "", "", "", "", fmt.Errorf("read snapshot parents: %w", err)
	}
	fields := strings.Fields(line)
	if len(fields) < 3 || len(fields) > 4 {
		return "", "", "", "", fmt.Errorf("dirty work snapshot %s has %d parents", commit, len(fields)-1)
	}
	head = fields[1]
	indexTree, err = Run(ctx, wt, "rev-parse", "--verify", fields[2]+"^{tree}")
	if err != nil {
		return "", "", "", "", fmt.Errorf("read snapshot index: %w", err)
	}
	workTree, err = Run(ctx, wt, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return "", "", "", "", fmt.Errorf("read snapshot worktree: %w", err)
	}
	if len(fields) == 4 {
		untrackedTree, err = Run(ctx, wt, "rev-parse", "--verify", fields[3]+"^{tree}")
		if err != nil {
			return "", "", "", "", fmt.Errorf("read snapshot untracked tree: %w", err)
		}
	}
	return head, indexTree, workTree, untrackedTree, nil
}

func verifyDirtySnapshot(ctx context.Context, wt, commit, head, indexCommit, untrackedCommit, indexTree, workTree, message string) error {
	parents := []struct {
		spec string
		want string
	}{
		{commit + "^1", head},
		{commit + "^2", indexCommit},
	}
	if untrackedCommit != "" {
		parents = append(parents, struct {
			spec string
			want string
		}{commit + "^3", untrackedCommit})
	}
	for _, parent := range parents {
		got, err := Run(ctx, wt, "rev-parse", "--verify", parent.spec)
		if err != nil {
			return fmt.Errorf("snapshot parent %s: %w", parent.spec, err)
		}
		if got != parent.want {
			return fmt.Errorf("snapshot parent %s is %s, want %s", parent.spec, got, parent.want)
		}
	}
	if untrackedCommit == "" {
		if _, err := Run(ctx, wt, "rev-parse", "--verify", "--quiet", commit+"^3"); err == nil {
			return fmt.Errorf("snapshot has an unexpected untracked parent")
		}
	}
	gotTree, err := Run(ctx, wt, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return err
	}
	if gotTree != workTree {
		return fmt.Errorf("snapshot worktree tree is %s, want %s", gotTree, workTree)
	}
	gotIndex, err := Run(ctx, wt, "rev-parse", "--verify", indexCommit+"^{tree}")
	if err != nil {
		return err
	}
	if gotIndex != indexTree {
		return fmt.Errorf("snapshot index tree is %s, want %s", gotIndex, indexTree)
	}
	body, err := Run(ctx, wt, "log", "-1", "--format=%B", commit)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body) != strings.TrimSpace(message) {
		return fmt.Errorf("snapshot message does not match the verified manifest")
	}
	return nil
}

func dirtyWorkMessage(head, base, indexTree, workTree, untrackedTree string, paths []dirtyEntry) string {
	if base == "" {
		base = "-"
	}
	if untrackedTree == "" {
		untrackedTree = "-"
	}
	var b strings.Builder
	b.WriteString("no-mistakes dirty-work snapshot\n\n")
	b.WriteString("schema: 1\n")
	fmt.Fprintf(&b, "head: %s\n", head)
	fmt.Fprintf(&b, "base: %s\n", base)
	fmt.Fprintf(&b, "index: %s\n", indexTree)
	fmt.Fprintf(&b, "worktree: %s\n", workTree)
	fmt.Fprintf(&b, "untracked: %s\n", untrackedTree)
	for _, path := range paths {
		fmt.Fprintf(&b, "path: %s %s\n", path.kind, filepath.ToSlash(path.rel))
	}
	return b.String()
}

func dirtyWorkIdentity() []string {
	return []string{
		"GIT_AUTHOR_NAME=no-mistakes",
		"GIT_AUTHOR_EMAIL=no-mistakes@localhost",
		"GIT_COMMITTER_NAME=no-mistakes",
		"GIT_COMMITTER_EMAIL=no-mistakes@localhost",
	}
}

func absoluteGitPath(ctx context.Context, dir, name string) (string, error) {
	p, err := Run(ctx, dir, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p, nil
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return resolved
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
