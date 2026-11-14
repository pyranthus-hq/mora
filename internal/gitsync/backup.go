package gitsync

// Selected-target backup wrappers. Provider verification is injected at the
// composition boundary; this package never obtains credentials or creates repos.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"github.com/pyranthus-hq/mora/internal/managedtree"
)

type SelectedTarget struct {
	ID, CanonicalURL, RemoteDigest, PolicyDigest string
	AuthorizedReaderIDs                          []string
}

type VerifiedTarget struct {
	ID, CanonicalURL    string
	Private             bool
	AuthorizedReaderIDs []string
}

// TargetVerifier is called before every selected-target transfer. A verifier
// unavailable to the integrator is a denial, never an assumed private target.
type TargetVerifier func(context.Context, SelectedTarget) (VerifiedTarget, error)

type BackupOptions struct {
	Vault, StateDir, RunID, RemoteRef string
	Target                            SelectedTarget
	Verify                            TargetVerifier
	// Only package tests can replace transport or command execution.
	testTransport   bool
	disabledFilters []string
	run             func(context.Context, string, int, ...string) ([]byte, error)
}

type Relation string

const (
	RelationClean       Relation = "clean"
	RelationRemoteAhead Relation = "remote_ahead"
	RelationDiverged    Relation = "diverged"
)

type RelationResult struct {
	Relation                 Relation
	LocalOID, RemoteOID      string
	BundlePath, ManifestPath string
}

type PullResult struct {
	Commit, Destination, ManifestPath string
	Published                         bool
}

// PushSelected revalidates the provider identity and current origin before it
// stages a bounded, filter-free commit and pushes only the selected ref.
func PushSelected(ctx context.Context, opts BackupOptions) error {
	return withBackupGuard(ctx, opts, func(opts BackupOptions) error { return pushSelected(ctx, opts) })
}
func pushSelected(ctx context.Context, opts BackupOptions) error {
	if err := authorize(ctx, opts); err != nil {
		return err
	}
	if err := checkOrigin(ctx, opts); err != nil {
		return err
	}
	if head, err := gitOutput(ctx, opts, "symbolic-ref", "-q", "HEAD"); err != nil || head != opts.RemoteRef {
		return errors.New("selected backup branch does not match target ref")
	}
	filters, filterErr := gitOutput(ctx, opts, "config", "--name-only", "--get-regexp", "^filter\\.")
	if filterErr != nil && !absentGitConfig(filterErr) {
		return errors.New("backup content filter configuration is unreadable")
	}
	seenFilters := map[string]bool{}
	for _, key := range strings.Split(filters, "\n") {
		if key == "" {
			continue
		}
		parts := strings.Split(key, ".")
		if len(parts) < 3 {
			return errors.New("invalid backup content filter")
		}
		name := strings.Join(parts[1:len(parts)-1], ".")
		if !safeRun(name) || len(seenFilters) >= 100 {
			return errors.New("invalid backup content filter")
		}
		if !seenFilters[name] {
			opts.disabledFilters = append(opts.disabledFilters, name)
			seenFilters[name] = true
		}
	}
	if _, err := gitOutput(ctx, opts, "add", "-A"); err != nil {
		return fmt.Errorf("selected backup staging failed: %w", err)
	}
	tracked, err := gitBytes(ctx, opts, 2<<20, "ls-files", "-z")
	if err != nil {
		return err
	}
	for _, path := range strings.Split(string(tracked), "\x00") {
		if path != "" && sensitivePath(path) {
			return errors.New("selected backup contains tracked sensitive files")
		}
	}
	status, err := gitOutput(ctx, opts, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		args := append(CommitIdentityArgs(ctx, opts.Vault, func(ctx context.Context, _, _ string, args ...string) (string, error) {
			return gitOutput(ctx, opts, args...)
		}), "commit", "-m", "mora selected backup")
		if _, err := gitOutput(ctx, opts, args...); err != nil {
			return err
		}
	}
	// Provider and routing are checked again after staging and directly before push.
	if err := authorize(ctx, opts); err != nil {
		return err
	}
	if err := checkOrigin(ctx, opts); err != nil {
		return err
	}
	if _, err := gitOutput(ctx, opts, "push", opts.Target.CanonicalURL, "HEAD:"+opts.RemoteRef); err != nil {
		return fmt.Errorf("selected backup push failed: %w", err)
	}
	return nil
}

// ProbeRelation fetches only the selected ref into a private temporary ref.
// On remote-ahead/diverged it retains both tips in named refs and a verified,
// self-contained bundle without modifying the working tree or index.
func ProbeRelation(ctx context.Context, opts BackupOptions) (result RelationResult, err error) {
	err = withBackupGuard(ctx, opts, func(opts BackupOptions) error { var e error; result, e = probeRelation(ctx, opts); return e })
	return
}
func probeRelation(ctx context.Context, opts BackupOptions) (RelationResult, error) {
	if err := authorize(ctx, opts); err != nil {
		return RelationResult{}, err
	}
	if err := checkOrigin(ctx, opts); err != nil {
		return RelationResult{}, err
	}
	if !validRef(opts.RemoteRef) || !safeRun(opts.RunID) {
		return RelationResult{}, errors.New("invalid selected backup relation request")
	}
	remoteRef := "refs/mora/backup/" + opts.RunID + "/remote"
	if err := requireUnusedBackupRef(ctx, opts, remoteRef); err != nil {
		return RelationResult{}, err
	}
	if _, err := gitOutput(ctx, opts, "fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", opts.Target.CanonicalURL, opts.RemoteRef+":"+remoteRef); err != nil {
		return RelationResult{}, fmt.Errorf("selected backup relation fetch failed: %w", err)
	}
	remote, err := gitOutput(ctx, opts, "rev-parse", remoteRef)
	if err != nil {
		return RelationResult{}, fmt.Errorf("selected backup relation is unavailable: %w", err)
	}
	local, localErr := gitOutput(ctx, opts, "rev-parse", "--verify", "HEAD")
	if localErr != nil {
		local = ""
	}
	result := RelationResult{LocalOID: local, RemoteOID: remote}
	if localErr != nil {
		branch, branchErr := gitOutput(ctx, opts, "symbolic-ref", "-q", "HEAD")
		if branchErr != nil || !validRef(branch) {
			return RelationResult{}, errors.New("local backup history is unavailable")
		}
		_, refErr := gitOutput(ctx, opts, "show-ref", "--verify", "--quiet", branch)
		if !absentGitConfig(refErr) {
			return RelationResult{}, errors.New("local backup history is corrupt or unreadable")
		}
		result.Relation = RelationRemoteAhead
		if err := preserveRelation(ctx, opts, &result); err != nil {
			return RelationResult{}, err
		}
		return result, nil
	}
	base, mergeErr := gitOutput(ctx, opts, "merge-base", local, remote)
	if mergeErr != nil && !absentGitConfig(mergeErr) {
		return RelationResult{}, errors.New("backup histories cannot be compared")
	}
	switch {
	case mergeErr == nil && base == remote:
		result.Relation = RelationClean
	case mergeErr == nil && base == local:
		result.Relation = RelationRemoteAhead
	default:
		result.Relation = RelationDiverged
	}
	if result.Relation == RelationClean {
		return result, nil
	}
	if err := preserveRelation(ctx, opts, &result); err != nil {
		return RelationResult{}, err
	}
	return result, nil
}

// PullSnapshot exports regular safe files from the selected ref into an empty,
// app-owned directory. It does not clone, checkout, run hooks, filters, LFS or
// submodules, and it never changes the live vault.
func PullSnapshot(ctx context.Context, opts BackupOptions, destination string) (result PullResult, err error) {
	err = withBackupGuard(ctx, opts, func(opts BackupOptions) error {
		var e error
		result, e = pullSnapshot(ctx, opts, destination)
		return e
	})
	return
}
func pullSnapshot(ctx context.Context, opts BackupOptions, destination string) (PullResult, error) {
	var canonicalErr error
	destination, canonicalErr = canonicalBackupPath(destination)
	if canonicalErr != nil {
		return PullResult{}, canonicalErr
	}
	if err := authorize(ctx, opts); err != nil {
		return PullResult{}, err
	}
	if err := checkOrigin(ctx, opts); err != nil {
		return PullResult{}, err
	}
	if !validRef(opts.RemoteRef) || !safeRun(opts.RunID) {
		return PullResult{}, errors.New("invalid selected backup pull request")
	}
	if destination != filepath.Join(opts.StateDir, "backup", "restores", opts.RunID) || !safeOwnedPath(opts.StateDir, destination) {
		return PullResult{}, errors.New("selected backup destination is not an exclusive app-owned state path")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return PullResult{}, errors.New("backup snapshot destination already exists or is inaccessible")
	}
	ref := "refs/mora/restore/" + opts.RunID
	if err := requireUnusedBackupRef(ctx, opts, ref); err != nil {
		return PullResult{}, err
	}
	if _, err := gitOutput(ctx, opts, "fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", opts.Target.CanonicalURL, opts.RemoteRef+":"+ref); err != nil {
		return PullResult{}, fmt.Errorf("selected backup pull fetch failed: %w", err)
	}
	entries, err := gitBytes(ctx, opts, 2<<20, "ls-tree", "-r", "-l", "-z", "--full-tree", ref)
	if err != nil {
		return PullResult{}, fmt.Errorf("selected backup pull tree failed: %w", err)
	}
	type treeFile struct {
		path string
		oid  string
		size int64
	}
	var files []treeFile
	seen := map[string]string{}
	var total int64
	for _, entry := range strings.Split(string(entries), "\x00") {
		if entry == "" {
			continue
		}
		mode, typ, path, oid, size, ok := parseTreeEntrySized(entry)
		if !ok || typ != "blob" || mode != "100644" || !safeTreePath(path, seen) || sensitivePath(path) || size > 8<<20 || len(files) >= 10000 || total+size > 64<<20 {
			return PullResult{}, errors.New("selected backup pull contains unsafe content")
		}
		total += size
		files = append(files, treeFile{path, oid, size})
	}

	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return PullResult{}, err
	}
	stage, err := os.MkdirTemp(parent, ".snapshot-")
	if err != nil {
		return PullResult{}, err
	}
	defer os.RemoveAll(stage)
	content := filepath.Join(stage, "vault")
	if err := os.Mkdir(content, 0700); err != nil {
		return PullResult{}, err
	}
	for _, file := range files {
		body, err := gitBytes(ctx, opts, int(file.size), "cat-file", "blob", file.oid)
		if err != nil || int64(len(body)) != file.size {
			return PullResult{}, errors.New("selected backup pull content is unavailable or too large")
		}
		out := filepath.Join(content, filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return PullResult{}, err
		}
		if err := atomicio.WriteDurable(out, body, 0o600); err != nil {
			return PullResult{}, err
		}
	}
	commit, err := gitOutput(ctx, opts, "rev-parse", ref)
	if err != nil {
		return PullResult{}, err
	}
	manifest, _ := json.Marshal(struct{ Commit string }{commit})
	if err := atomicio.WriteDurable(filepath.Join(stage, "manifest.json"), append(manifest, '\n'), 0o600); err != nil {
		return PullResult{}, err
	}
	revision, err := managedtree.DigestTree(stage)
	if err != nil {
		return PullResult{}, err
	}
	installed, err := managedtree.InstallTree(revision, destination, nil)
	if installed.Destination == "" {
		return PullResult{}, err
	}
	return PullResult{Commit: commit, Destination: filepath.Join(destination, "vault"), ManifestPath: filepath.Join(destination, "manifest.json"), Published: true}, err

}

func authorize(ctx context.Context, opts BackupOptions) error {
	if opts.Verify == nil || !validTargetIdentity(opts.Target) {
		return errors.New("selected backup authorization is unavailable")
	}
	if opts.testTransport {
		if !strings.HasPrefix(opts.Target.CanonicalURL, "file://") {
			return errors.New("selected backup test transport is invalid")
		}
	} else if !validGitHubURL(opts.Target.CanonicalURL) {
		return errors.New("selected backup target is not a production GitHub transport")
	}
	verified, err := opts.Verify(ctx, opts.Target)
	if err != nil || !verified.Private || verified.ID != opts.Target.ID || verified.CanonicalURL != opts.Target.CanonicalURL || !sameReaders(verified.AuthorizedReaderIDs, opts.Target.AuthorizedReaderIDs) {
		return errors.New("selected backup target verification failed")
	}
	return nil
}
func checkOrigin(ctx context.Context, opts BackupOptions) error {
	fetch, err := gitOutput(ctx, opts, "remote", "get-url", "--all", "origin")
	push, pushErr := gitOutput(ctx, opts, "remote", "get-url", "--push", "--all", "origin")
	rewrites, rewriteErr := gitOutput(ctx, opts, "config", "--get-regexp", "^url\\.")
	if err != nil || pushErr != nil || fetch != opts.Target.CanonicalURL || push != opts.Target.CanonicalURL || rewrites != "" || (rewriteErr != nil && !absentGitConfig(rewriteErr)) {
		return errors.New("selected backup origin does not match verified target")
	}
	return nil
}
func gitOutput(ctx context.Context, opts BackupOptions, args ...string) (string, error) {
	out, err := selectedCommand(ctx, opts, 1<<20, args...)
	return strings.TrimSpace(string(out)), err
}
func gitBytes(ctx context.Context, opts BackupOptions, limit int, args ...string) ([]byte, error) {
	return selectedCommand(ctx, opts, limit, args...)
}
func validTargetIdentity(t SelectedTarget) bool {
	return safeRun(t.ID) && len(t.CanonicalURL) > 0 && len(t.CanonicalURL) <= 1024 && safeRun(t.RemoteDigest) && safeRun(t.PolicyDigest) && validReaders(t.AuthorizedReaderIDs)
}
func validGitHubURL(v string) bool {
	for _, prefix := range []string{"https://github.com/", "ssh://git@github.com/", "git@github.com:"} {
		if path, ok := strings.CutPrefix(v, prefix); ok {
			parts := strings.Split(path, "/")
			return len(parts) == 2 && safeRun(parts[0]) && safeRun(parts[1])
		}
	}
	return false
}
func validReaders(v []string) bool {
	if len(v) == 0 || len(v) > 100 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range v {
		if !safeRun(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
func safeRun(v string) bool {
	if v == "" || len(v) > 128 || v == "." || v == ".." || !safeTreePath(v, map[string]string{}) || strings.HasSuffix(v, ".") || strings.Contains(v, "..") {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			continue
		}
		return false
	}
	return true
}
func validRef(v string) bool {
	branch, ok := strings.CutPrefix(v, "refs/heads/")
	if !ok || len(v) > 256 {
		return false
	}
	for _, part := range strings.Split(branch, "/") {
		if !safeRun(part) || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
func sameReaders(a, b []string) bool {
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	return strings.Join(a, "\x00") == strings.Join(b, "\x00")
}

func preserveRelation(ctx context.Context, opts BackupOptions, result *RelationResult) error {
	root := filepath.Join(opts.StateDir, "backup", "reconcile", opts.RunID)
	if !safeOwnedPath(opts.StateDir, root) {
		return errors.New("selected backup reconcile path is unsafe")
	}
	if err := emptyPrivateDir(root); err != nil {
		return err
	}
	localRef, remoteRef := "refs/mora/backup/"+opts.RunID+"/local", "refs/mora/backup/"+opts.RunID+"/remote"
	if result.LocalOID != "" {
		if _, err := gitOutput(ctx, opts, "update-ref", localRef, result.LocalOID, ""); err != nil {
			return fmt.Errorf("preserving local backup history failed: %w", err)
		}
	}

	bundle := filepath.Join(root, "histories.bundle")
	bundleRefs := []string{"bundle", "create", bundle}
	if result.LocalOID != "" {
		bundleRefs = append(bundleRefs, localRef)
	}
	bundleRefs = append(bundleRefs, remoteRef)
	if _, err := gitOutput(ctx, opts, bundleRefs...); err != nil {
		return fmt.Errorf("preserving backup histories failed: %w", err)
	}
	f, err := os.OpenFile(bundle, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("preserved backup histories cannot be made durable")
	}
	if err := atomicio.SyncDir(root); err != nil {
		return err
	}
	if _, err := gitOutput(ctx, opts, "bundle", "verify", bundle); err != nil {
		return fmt.Errorf("preserved backup histories cannot be verified: %w", err)
	}
	manifest, _ := json.Marshal(struct{ LocalOID, RemoteOID, Relation string }{result.LocalOID, result.RemoteOID, string(result.Relation)})
	manifestPath := filepath.Join(root, "manifest.json")
	if err := atomicio.WriteDurable(manifestPath, append(manifest, '\n'), 0o600); err != nil {
		return err
	}
	result.BundlePath, result.ManifestPath = bundle, manifestPath
	return nil
}
func emptyPrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("selected backup destination is not empty")
	}
	return nil
}
func safeOwnedPath(root, path string) bool {
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && filepath.IsLocal(rel) && safeBackupDirectory(root) == nil && safeBackupDirectory(path) == nil
}
func parseTreeEntrySized(v string) (mode, typ, path, oid string, size int64, ok bool) {
	head, path, found := strings.Cut(v, "\t")
	if !found {
		return
	}
	fields := strings.Fields(head)
	if len(fields) != 4 {
		return
	}
	size, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || size < 0 {
		return "", "", "", "", 0, false
	}
	oid = fields[2]
	if len(oid) != 40 && len(oid) != 64 {
		return "", "", "", "", 0, false
	}
	if _, err := hex.DecodeString(oid); err != nil {
		return "", "", "", "", 0, false
	}
	return fields[0], fields[1], path, oid, size, true
}
func safeTreePath(path string, seen map[string]string) bool {
	if path == "" || len(path) > 1024 || strings.HasPrefix(path, "/") {
		return false
	}
	var parent string
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "<>:\"\\|?*") || strings.TrimRight(part, ". ") != part {
			return false
		}
		for _, r := range part {
			if unicode.IsControl(r) {
				return false
			}
		}
		name := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if name == "CON" || name == "PRN" || name == "AUX" || name == "NUL" || name == "CONIN$" || name == "CONOUT$" || name == "CLOCK$" || (len(name) == 4 && (strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) && name[3] >= '1' && name[3] <= '9') {
			return false
		}
		key := parent + "/" + strings.ToUpper(part)
		if prev, found := seen[key]; found && prev != part {
			return false
		}
		seen[key] = part
		parent = key
	}
	return true
}
func sensitivePath(path string) bool {
	for _, part := range strings.Split(strings.ToLower(path), "/") {
		if part == ".git" || part == ".env" || strings.HasPrefix(part, ".env.") || part == "tokens" || part == "share" || strings.HasPrefix(part, "identity") || strings.HasPrefix(part, "id_rsa") || strings.HasPrefix(part, "id_ed25519") || part == "auth.json" || strings.HasPrefix(part, "credentials") || strings.Contains(part, "token") || strings.HasSuffix(part, ".pem") || strings.HasSuffix(part, ".key") || strings.HasSuffix(part, ".db") || strings.HasSuffix(part, ".db-shm") || strings.HasSuffix(part, ".db-wal") {
			return true
		}
	}
	return false
}

func withBackupGuard(ctx context.Context, opts BackupOptions, fn func(BackupOptions) error) error {
	if err := authorize(ctx, opts); err != nil {
		return err
	}
	if !safeRun(opts.RunID) || !validRef(opts.RemoteRef) || opts.StateDir == "" || !filepath.IsAbs(opts.StateDir) || !filepath.IsAbs(opts.Vault) {
		return errors.New("invalid backup operation identity")
	}
	if err := safeBackupDirectory(opts.Vault); err != nil {
		return err
	}
	if ok, err := VaultRepoState(filepath.Join(opts.Vault, ".git")); err != nil || !ok {
		return errors.New("backup vault must already be a plain Git repository")
	}
	if err := safeBackupDirectory(opts.StateDir); err != nil {
		return err
	}
	canonicalState, err := canonicalBackupPath(opts.StateDir)
	if err != nil {
		return err
	}
	opts.StateDir = canonicalState
	vault, err := filepath.EvalSymlinks(opts.Vault)
	if err != nil {
		return err
	}
	opts.Vault = vault
	if rel, err := filepath.Rel(vault, opts.StateDir); err == nil && (rel == "." || filepath.IsLocal(rel)) {
		return errors.New("backup state must be outside the vault")
	}
	dir := filepath.Join(opts.StateDir, "backup", "operations")
	if err := safeBackupDirectory(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(vault))
	return leasefile.WithGuard(filepath.Join(dir, hex.EncodeToString(sum[:])+".lock"), func() error { return fn(opts) })
}

// Verify the whole ancestor chain, allowing only the two standard Darwin aliases.
func safeBackupDirectory(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for current := abs; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if current != abs && backupSystemAlias(current) {
			} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("backup path is linked or unsafe")
			} else if bad, err := backupReparse(current); err != nil || bad {
				return errors.New("backup path is a reparse point")
			}
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}

func canonicalBackupPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := abs
	for {
		if _, err := os.Lstat(current); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if filepath.Dir(current) == current {
			return "", errors.New("backup path has no existing ancestor")
		}
		current = filepath.Dir(current)
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	tail, err := filepath.Rel(current, abs)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, tail), nil
}

func requireUnusedBackupRef(ctx context.Context, opts BackupOptions, ref string) error {
	_, err := gitOutput(ctx, opts, "show-ref", "--verify", "--quiet", ref)
	if absentGitConfig(err) {
		return nil
	}
	return errors.New("backup operation ref already exists or is unreadable")
}
