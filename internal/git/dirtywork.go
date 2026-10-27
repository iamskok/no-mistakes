package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	if err := rejectNonSnapshotable(ctx, wt); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	ref := DirtyWorkRef(req.RunID)
	if _, err := Run(ctx, req.GateDir, "check-ref-format", ref); err != nil {
		return nil, fmt.Errorf("snapshot ref %s: %w", ref, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	snap, err := writeDirtySnapshot(ctx, req, ref, entries)
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

func readDirtyEntries(ctx context.Context, wt string) ([]dirtyEntry, error) {
	entries, err := porcelainEntries(ctx, wt)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	return loadDirtyEntries(ctx, wt, entries)
}

func loadDirtyEntries(ctx context.Context, wt string, entries []dirtyEntry) ([]dirtyEntry, error) {
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

var errNotRegular = errors.New("not a regular file")

type preparedEntry struct {
	dirtyEntry
	mode string
	tmp  string
	link string
}

func worktreeDeleted(code string) bool {
	return len(code) == 2 && code[1] == 'D'
}

func worktreeChanged(code string) bool {
	return len(code) == 2 && code[1] != ' ' && code[1] != '?' && code[1] != 'D'
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

func readerContainsSecret(ctx context.Context, r io.Reader) (bool, error) {
	return writeReaderScan(ctx, io.Discard, r)
}

func writeReaderScan(ctx context.Context, dst io.Writer, r io.Reader) (bool, error) {
	overlap := secretMarkerOverlap()
	buf := make([]byte, secretReadChunk)
	var tail []byte
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return false, werr
			}
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

func prepareDirtyContent(ctx context.Context, wt, tmp string, entries []dirtyEntry) ([]preparedEntry, error) {
	cleaned, err := loadDirtyEntries(ctx, wt, entries)
	if err != nil {
		return nil, err
	}
	prepared := make([]preparedEntry, 0, len(cleaned))
	for _, entry := range cleaned {
		full := filepath.Join(wt, entry.rel)
		if secretPath(entry.rel) && entryHasBytes(ctx, wt, entry, full) {
			return nil, fmt.Errorf("%w: %s is secret-bearing", ErrDirtyWorkRejected, entry.rel)
		}
		item := preparedEntry{dirtyEntry: entry}
		if _, err := os.Lstat(full); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if entry.code == "??" || worktreeChanged(entry.code) {
					return nil, fmt.Errorf("%w: %s disappeared while capturing dirty work", ErrDirtyWorkRejected, entry.rel)
				}
				prepared = append(prepared, item)
				continue
			}
			return nil, err
		}
		mode, tmpPath, link, err := stageSnapshot(ctx, tmp, entry.rel, full)
		if err != nil {
			return nil, err
		}
		item.mode = mode
		item.tmp = tmpPath
		item.link = link
		prepared = append(prepared, item)
	}
	return prepared, nil
}

func stageSnapshot(ctx context.Context, tmpDir, rel, full string) (mode, tmpPath, link string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", "", err
	}
	info, err := os.Lstat(full)
	if err != nil {
		return "", "", "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return "", "", "", err
		}
		if bytesContainSecret([]byte(target)) {
			return "", "", "", fmt.Errorf("%w: %s is secret-bearing", ErrDirtyWorkRejected, rel)
		}
		return "120000", "", target, nil
	}
	if !info.Mode().IsRegular() {
		return "", "", "", notSnapshotable(rel, info)
	}
	f, err := openRegular(full)
	if err != nil {
		if errors.Is(err, errNotRegular) {
			return "", "", "", notSnapshotable(rel, info)
		}
		return "", "", "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", "", "", err
	}
	if !st.Mode().IsRegular() {
		return "", "", "", notSnapshotable(rel, st)
	}
	dst, err := os.CreateTemp(tmpDir, "blob-")
	if err != nil {
		return "", "", "", err
	}
	secret, copyErr := writeReaderScan(ctx, dst, f)
	closeErr := dst.Close()
	if copyErr != nil {
		return "", "", "", copyErr
	}
	if closeErr != nil {
		return "", "", "", closeErr
	}
	if secret {
		return "", "", "", fmt.Errorf("%w: %s is secret-bearing", ErrDirtyWorkRejected, rel)
	}
	mode = "100644"
	if st.Mode()&0o111 != 0 {
		mode = "100755"
	}
	return mode, dst.Name(), "", nil
}

func scanStagedBlobs(ctx context.Context, wt, indexFile string) error {
	out, err := runInDirWithEnvAndInputRaw(ctx, wt, []string{"GIT_INDEX_FILE=" + indexFile}, "", "diff-index", "--cached", "-z", "HEAD")
	if err != nil {
		return fmt.Errorf("read staged changes: %w", err)
	}
	return walkStagedBlobs(out, func(path, sha string) error {
		found, err := gitStreamContainsSecret(ctx, wt, "cat-file", "blob", sha)
		if err != nil {
			return fmt.Errorf("read index %s: %w", path, err)
		}
		if found {
			return fmt.Errorf("%w: %s is secret-bearing", ErrDirtyWorkRejected, path)
		}
		return nil
	})
}

func walkStagedBlobs(out []byte, fn func(path, sha string) error) error {
	for len(out) > 0 {
		metaEnd := bytes.IndexByte(out, 0)
		if metaEnd < 0 {
			return fmt.Errorf("%w: unreadable staged diff", ErrDirtyWorkRejected)
		}
		meta := out[:metaEnd]
		out = out[metaEnd+1:]
		pathEnd := bytes.IndexByte(out, 0)
		if pathEnd < 0 {
			return fmt.Errorf("%w: unreadable staged diff", ErrDirtyWorkRejected)
		}
		path := string(out[:pathEnd])
		out = out[pathEnd+1:]
		fields := bytes.Fields(meta)
		if len(fields) < 5 {
			return fmt.Errorf("%w: unreadable staged diff", ErrDirtyWorkRejected)
		}
		status := string(fields[4])
		sha := string(fields[3])
		if status == "D" || zeroSHA(sha) {
			continue
		}
		if status[0] == 'R' || status[0] == 'C' {
			return fmt.Errorf("%w: rename or copy in the index; refusing a partial snapshot", ErrDirtyWorkRejected)
		}
		if err := fn(path, sha); err != nil {
			return err
		}
	}
	return nil
}

func zeroSHA(sha string) bool {
	if sha == "" {
		return true
	}
	for _, c := range sha {
		if c != '0' {
			return false
		}
	}
	return true
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

func writeDirtySnapshot(ctx context.Context, req DirtyWorkRequest, ref string, entries []dirtyEntry) (*DirtyWorkSnapshot, error) {
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

	prepared, err := prepareDirtyContent(ctx, wt, tmp, entries)
	if err != nil {
		return nil, err
	}
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
	if err := scanStagedBlobs(ctx, wt, indexCopy); err != nil {
		return nil, err
	}

	workTree, err := buildWorkTree(ctx, wt, tmp, indexCopy, prepared)
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

	untrackedTree := ""
	untrackedCommit := ""
	ident := dirtyWorkIdentity()
	if hasUntracked(prepared) {
		untrackedTree, err = writeUntrackedTree(ctx, wt, tmp, prepared)
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
	message := dirtyWorkMessage(head, req.BaseSHA, indexTree, workTree, untrackedTree, dirtyEntries(prepared))
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

func dirtyEntries(prepared []preparedEntry) []dirtyEntry {
	entries := make([]dirtyEntry, len(prepared))
	for i := range prepared {
		entries[i] = prepared[i].dirtyEntry
	}
	return entries
}

func hasUntracked(prepared []preparedEntry) bool {
	for _, path := range prepared {
		if path.code == "??" {
			return true
		}
	}
	return false
}

func writeUntrackedTree(ctx context.Context, wt, tmp string, prepared []preparedEntry) (string, error) {
	index := filepath.Join(tmp, "untracked-index")
	if err := os.WriteFile(index, nil, 0o644); err != nil {
		return "", err
	}
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := RunWithEnv(ctx, wt, env, "read-tree", "--empty"); err != nil {
		return "", fmt.Errorf("prepare untracked index: %w", err)
	}
	info, err := indexInfo(ctx, wt, prepared, func(path preparedEntry) bool { return path.code == "??" })
	if err != nil {
		return "", err
	}
	if info != "" {
		if _, err := runInDirWithEnvAndInput(ctx, wt, env, info, "update-index", "--index-info"); err != nil {
			return "", fmt.Errorf("record untracked files: %w", err)
		}
	}
	tree, err := RunWithEnv(ctx, wt, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write untracked tree: %w", err)
	}
	return tree, nil
}

func buildWorkTree(ctx context.Context, wt, tmp, indexCopy string, prepared []preparedEntry) (string, error) {
	if !needsWorktreeApply(prepared) {
		tree, err := RunWithEnv(ctx, wt, []string{"GIT_INDEX_FILE=" + indexCopy}, "write-tree")
		if err != nil {
			return "", fmt.Errorf("write worktree tree: %w", err)
		}
		return tree, nil
	}
	workIndex := filepath.Join(tmp, "worktree-index")
	if err := copyFile(workIndex, indexCopy); err != nil {
		return "", err
	}
	env := []string{"GIT_INDEX_FILE=" + workIndex}
	info, err := indexInfo(ctx, wt, prepared, func(path preparedEntry) bool {
		return path.code != "??" && worktreeChanged(path.code)
	})
	if err != nil {
		return "", err
	}
	if info != "" {
		if _, err := runInDirWithEnvAndInput(ctx, wt, env, info, "update-index", "--index-info"); err != nil {
			return "", fmt.Errorf("snapshot tracked worktree: %w", err)
		}
	}
	var removes []string
	for _, path := range prepared {
		if path.code != "??" && worktreeDeleted(path.code) {
			removes = append(removes, filepath.ToSlash(path.rel))
		}
	}
	if len(removes) > 0 {
		args := append([]string{"update-index", "--force-remove", "--"}, removes...)
		if _, err := RunWithEnv(ctx, wt, env, args...); err != nil {
			return "", fmt.Errorf("snapshot tracked deletion: %w", err)
		}
	}
	tree, err := RunWithEnv(ctx, wt, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write worktree tree: %w", err)
	}
	return tree, nil
}

func needsWorktreeApply(prepared []preparedEntry) bool {
	for _, path := range prepared {
		if path.code == "??" {
			continue
		}
		if worktreeDeleted(path.code) || worktreeChanged(path.code) {
			return true
		}
	}
	return false
}

func indexInfo(ctx context.Context, wt string, prepared []preparedEntry, include func(preparedEntry) bool) (string, error) {
	var info strings.Builder
	for _, path := range prepared {
		if !include(path) {
			continue
		}
		blob, err := hashPrepared(ctx, wt, path, true)
		if err != nil {
			return "", fmt.Errorf("hash %s: %w", path.rel, err)
		}
		fmt.Fprintf(&info, "%s %s\t%s\n", path.mode, blob, filepath.ToSlash(path.rel))
	}
	return info.String(), nil
}

func hashPrepared(ctx context.Context, wt string, path preparedEntry, write bool) (string, error) {
	args := []string{"hash-object"}
	if write {
		args = append(args, "-w")
	}
	if path.mode == "120000" {
		args = append(args, "--no-filters", "--stdin")
		return runInDirWithEnvAndInput(ctx, wt, nil, path.link, args...)
	}
	if path.tmp == "" {
		return "", fmt.Errorf("%w: %s has no staged bytes", ErrDirtyWorkRejected, path.rel)
	}
	args = append(args, "--path="+filepath.ToSlash(path.rel), "--", path.tmp)
	return Run(ctx, wt, args...)
}

func recheckDirtyWork(ctx context.Context, wt, tmp, head, indexTree, workTree, untrackedTree string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := rejectNonSnapshotable(ctx, wt); err != nil {
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
	return worktreeEqualsSnapshot(ctx, wt, tmp, workTree, untrackedTree)
}

func worktreeEqualsSnapshot(ctx context.Context, wt, tmp, workTree, untrackedTree string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	index := filepath.Join(tmp, "equal-index")
	if err := os.WriteFile(index, nil, 0o644); err != nil {
		return err
	}
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := RunWithEnv(ctx, wt, env, "read-tree", workTree); err != nil {
		return fmt.Errorf("read tracked snapshot: %w", err)
	}
	if untrackedTree != "" {
		info, err := untrackedIndexInfo(ctx, wt, untrackedTree)
		if err != nil {
			return err
		}
		if len(info) > 0 {
			if _, err := runInDirWithEnvAndInput(ctx, wt, env, string(info), "update-index", "-z", "--index-info"); err != nil {
				return fmt.Errorf("read untracked snapshot: %w", err)
			}
		}
	}
	out, err := runInDirWithEnvAndInputRaw(ctx, wt, env, "", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	return statusMatchesSnapshot(out)
}

func untrackedIndexInfo(ctx context.Context, wt, tree string) ([]byte, error) {
	out, err := RunRaw(ctx, wt, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return nil, fmt.Errorf("list untracked snapshot: %w", err)
	}
	var info bytes.Buffer
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		tab := bytes.IndexByte(raw, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("%w: unreadable untracked snapshot", ErrDirtyWorkRejected)
		}
		fields := bytes.Fields(raw[:tab])
		if len(fields) < 3 {
			return nil, fmt.Errorf("%w: unreadable untracked snapshot", ErrDirtyWorkRejected)
		}
		info.Write(fields[0])
		info.WriteByte(' ')
		info.Write(fields[2])
		info.WriteByte('\t')
		info.Write(raw[tab+1:])
		info.WriteByte(0)
	}
	return info.Bytes(), nil
}

func statusMatchesSnapshot(out []byte) error {
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		if len(raw) < 4 || raw[2] != ' ' {
			return fmt.Errorf("%w: unreadable status entry", ErrDirtyWorkRejected)
		}
		if raw[0] == '?' || raw[1] != ' ' {
			return fmt.Errorf("checkout changed while capturing dirty work")
		}
	}
	return nil
}

func rejectNonSnapshotable(ctx context.Context, wt string) error {
	prefixes, err := ignoredDirPrefixes(ctx, wt)
	if err != nil {
		return err
	}
	var odd []string
	err = filepath.WalkDir(wt, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(wt, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		slash := filepath.ToSlash(rel)
		if hasIgnoredPrefix(prefixes, slash) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if snapshotableFile(info) {
			return nil
		}
		odd = append(odd, slash)
		return nil
	})
	if err != nil {
		return err
	}
	if len(odd) == 0 {
		return nil
	}
	ignored, err := ignoredPathSet(ctx, wt, odd)
	if err != nil {
		return err
	}
	for _, rel := range odd {
		if _, ok := ignored[rel]; ok {
			continue
		}
		info, err := os.Lstat(filepath.Join(wt, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		return notSnapshotable(rel, info)
	}
	return nil
}

func ignoredDirPrefixes(ctx context.Context, wt string) ([]string, error) {
	out, err := RunRaw(ctx, wt, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return nil, err
	}
	var prefixes []string
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 || raw[len(raw)-1] != '/' {
			continue
		}
		prefixes = append(prefixes, filepath.ToSlash(string(raw[:len(raw)-1])))
	}
	return prefixes, nil
}

func hasIgnoredPrefix(prefixes []string, slash string) bool {
	for _, prefix := range prefixes {
		if slash == prefix || strings.HasPrefix(slash, prefix+"/") {
			return true
		}
	}
	return false
}

func ignoredPathSet(ctx context.Context, wt string, paths []string) (map[string]struct{}, error) {
	var input bytes.Buffer
	for _, rel := range paths {
		input.WriteString(rel)
		input.WriteByte(0)
	}
	cmd := exec.CommandContext(ctx, "git", "check-ignore", "-z", "--stdin")
	cmd.Dir = wt
	cmd.Stdin = &input
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Env = nonInteractiveEnvForContext(ctx, wt)
	winproc.Harden(cmd)
	shellenv.ConfigureShellCommand(cmd)
	out, err := shellenv.OutputShellCommand(cmd)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return map[string]struct{}{}, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("check ignored paths: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	ignored := make(map[string]struct{})
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		ignored[filepath.ToSlash(string(raw))] = struct{}{}
	}
	return ignored, nil
}

func assertCheckoutClean(ctx context.Context, wt string) error {
	if err := rejectNonSnapshotable(ctx, wt); err != nil {
		return err
	}
	entries, err := readDirtyEntries(ctx, wt)
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
