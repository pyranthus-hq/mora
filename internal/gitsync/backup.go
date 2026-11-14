package gitsync

// Selected-target backup wrappers. Provider verification is injected at the
// composition boundary; this package never obtains credentials or creates repos.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
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

// BytesRunner is the narrow binary stdout seam required for tree/blob export.
// Runner's string contract is retained for existing generic Sync callers.
type BytesRunner func(ctx context.Context, dir, name string, args ...string) ([]byte, error)

type BackupOptions struct {
	Vault, StateDir, RunID, RemoteRef string
	Target                            SelectedTarget
	Verify                            TargetVerifier
	Runner                            Runner
	BytesRunner                       BytesRunner
	TestTransport                     bool // package tests inject local transport only
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

type PullResult struct{ Commit, Destination string }

// PushSelected revalidates the provider identity and current origin before it
// delegates to the existing no-init generic push path.
func PushSelected(ctx context.Context, opts BackupOptions) error {
	if err := authorize(ctx, opts); err != nil {
		return err
	}
	if err := checkOrigin(ctx, opts); err != nil {
		return err
	}
	if head, err := gitOutput(ctx, opts, "symbolic-ref", "-q", "HEAD"); err != nil || head != opts.RemoteRef {
		return errors.New("selected backup branch does not match target ref")
	}
	if _, err := opts.Runner(ctx, opts.Vault, "git", "add", "-A"); err != nil {
		return fmt.Errorf("selected backup staging failed: %w", err)
	}
	tracked, err := gitOutput(ctx, opts, "ls-files", "--", "index.db", "*.db", "*.db-shm", "*.db-wal", "*.token", "tokens", "identity*", "share")
	if err != nil || tracked != "" {
		return errors.New("selected backup contains tracked sensitive files")
	}
	status, err := gitOutput(ctx, opts, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		args := append(CommitIdentityArgs(ctx, opts.Vault, opts.Runner), "commit", "-m", "mora selected backup")
		if _, err := opts.Runner(ctx, opts.Vault, "git", args...); err != nil {
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
	if _, err := opts.Runner(ctx, opts.Vault, "git", "push", opts.Target.CanonicalURL, "HEAD:"+opts.RemoteRef); err != nil {
		return fmt.Errorf("selected backup push failed: %w", err)
	}
	return nil
}

// ProbeRelation fetches only the selected ref into a private temporary ref.
// On remote-ahead/diverged it retains both tips in named refs and a verified,
// self-contained bundle without modifying the working tree or index.
func ProbeRelation(ctx context.Context, opts BackupOptions) (RelationResult, error) {
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
	if _, err := opts.Runner(ctx, opts.Vault, "git", "fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", opts.Target.CanonicalURL, opts.RemoteRef+":"+remoteRef); err != nil {
		return RelationResult{}, fmt.Errorf("selected backup relation fetch failed: %w", err)
	}
	remote, err := gitOutput(ctx, opts, "rev-parse", remoteRef)
	if err != nil {
		return RelationResult{}, fmt.Errorf("selected backup relation is unavailable: %w", err)
	}
	local, localErr := gitOutput(ctx, opts, "rev-parse", "HEAD")
	result := RelationResult{LocalOID: local, RemoteOID: remote}
	if localErr != nil {
		result.Relation = RelationRemoteAhead
		if err := preserveRelation(ctx, opts, &result); err != nil {
			return RelationResult{}, err
		}
		return result, nil
	}
	base, mergeErr := gitOutput(ctx, opts, "merge-base", local, remote)
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
func PullSnapshot(ctx context.Context, opts BackupOptions, destination string) (PullResult, error) {
	if err := authorize(ctx, opts); err != nil {
		return PullResult{}, err
	}
	if err := checkOrigin(ctx, opts); err != nil {
		return PullResult{}, err
	}
	if !validRef(opts.RemoteRef) || !safeRun(opts.RunID) {
		return PullResult{}, errors.New("invalid selected backup pull request")
	}
	if opts.BytesRunner == nil || destination != filepath.Join(opts.StateDir, "backup", "restores", opts.RunID) || !safeOwnedPath(opts.StateDir, destination) {
		return PullResult{}, errors.New("selected backup destination is not an exclusive app-owned state path")
	}
	ref := "refs/mora/restore/" + opts.RunID
	if _, err := opts.Runner(ctx, opts.Vault, "git", "fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", opts.Target.CanonicalURL, opts.RemoteRef+":"+ref); err != nil {
		return PullResult{}, fmt.Errorf("selected backup pull fetch failed: %w", err)
	}
	entries, err := gitBytes(ctx, opts, "ls-tree", "-r", "-l", "-z", "--full-tree", ref)
	if err != nil {
		return PullResult{}, fmt.Errorf("selected backup pull tree failed: %w", err)
	}
	type treeFile struct {
		path string
		size int64
	}
	var files []treeFile
	var total int64
	for _, entry := range strings.Split(string(entries), "\x00") {
		if entry == "" {
			continue
		}
		mode, typ, path, size, ok := parseTreeEntrySized(entry)
		if !ok || typ != "blob" || mode != "100644" || !safeTreePath(path) || sensitivePath(path) || size > 8<<20 || len(files) >= 10000 || total+size > 64<<20 {
			return PullResult{}, errors.New("selected backup pull contains unsafe content")
		}
		total += size
		files = append(files, treeFile{path, size})
	}
	stage := destination + ".staging"
	if err := emptyPrivateDir(stage); err != nil {
		return PullResult{}, err
	}
	for _, file := range files {
		body, err := gitBytes(ctx, opts, "show", ref+":"+file.path)
		if err != nil || int64(len(body)) != file.size {
			return PullResult{}, errors.New("selected backup pull content is unavailable or too large")
		}
		out := filepath.Join(stage, filepath.FromSlash(file.path))
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
	if err := os.Rename(stage, destination); err != nil {
		return PullResult{}, err
	}
	if err := atomicio.SyncDir(filepath.Dir(destination)); err != nil {
		return PullResult{}, err
	}
	return PullResult{Commit: commit, Destination: destination}, nil
}

func authorize(ctx context.Context, opts BackupOptions) error {
	if opts.Runner == nil || opts.Verify == nil || !validTargetIdentity(opts.Target) {
		return errors.New("selected backup authorization is unavailable")
	}
	if opts.TestTransport {
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
	if err != nil || pushErr != nil || fetch != opts.Target.CanonicalURL || push != opts.Target.CanonicalURL || (rewriteErr == nil && rewrites != "") {
		return errors.New("selected backup origin does not match verified target")
	}
	return nil
}
func gitOutput(ctx context.Context, opts BackupOptions, args ...string) (string, error) {
	out, err := opts.Runner(ctx, opts.Vault, "git", args...)
	return strings.TrimSpace(out), err
}
func gitBytes(ctx context.Context, opts BackupOptions, args ...string) ([]byte, error) {
	return opts.BytesRunner(ctx, opts.Vault, "git", args...)
}
func validTargetIdentity(t SelectedTarget) bool {
	return safeRun(t.ID) && len(t.CanonicalURL) > 0 && len(t.CanonicalURL) <= 1024 && safeRun(t.RemoteDigest) && safeRun(t.PolicyDigest) && validReaders(t.AuthorizedReaderIDs)
}
func validGitHubURL(v string) bool {
	return len(v) > 0 && len(v) <= 1024 && (strings.HasPrefix(v, "https://github.com/") || strings.HasPrefix(v, "ssh://git@github.com/") || strings.HasPrefix(v, "git@github.com:")) && !strings.ContainsAny(v, "@? #")
}
func validReaders(v []string) bool {
	if len(v) == 0 || len(v) > 100 {
		return false
	}
	for _, id := range v {
		if !safeRun(id) {
			return false
		}
	}
	return true
}
func safeRun(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}
func validRef(v string) bool {
	return strings.HasPrefix(v, "refs/heads/") && len(v) <= 256 && !strings.ContainsAny(v, " ~^:?*[\\")
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
		if _, err := opts.Runner(ctx, opts.Vault, "git", "update-ref", localRef, result.LocalOID); err != nil {
			return fmt.Errorf("preserving local backup history failed: %w", err)
		}
	}
	if _, err := opts.Runner(ctx, opts.Vault, "git", "update-ref", remoteRef, result.RemoteOID); err != nil {
		return fmt.Errorf("preserving remote backup history failed: %w", err)
	}
	bundle := filepath.Join(root, "histories.bundle")
	bundleRefs := []string{"bundle", "create", bundle}
	if result.LocalOID != "" {
		bundleRefs = append(bundleRefs, localRef)
	}
	bundleRefs = append(bundleRefs, remoteRef)
	if _, err := opts.Runner(ctx, opts.Vault, "git", bundleRefs...); err != nil {
		return fmt.Errorf("preserving backup histories failed: %w", err)
	}
	f, err := os.Open(bundle)
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
	if _, err := opts.Runner(ctx, opts.Vault, "git", "bundle", "verify", bundle); err != nil {
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
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	if info, err := os.Lstat(root); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		return false
	}
	for current := root; ; {
		info, err := os.Lstat(current)
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || (current != path && !info.IsDir())) {
			return false
		}
		if err != nil && !os.IsNotExist(err) {
			return false
		}
		if current == path {
			return true
		}
		rest, _ := filepath.Rel(current, path)
		current = filepath.Join(current, strings.Split(rest, string(filepath.Separator))[0])
	}
}
func parseTreeEntry(v string) (mode, typ, path string, ok bool) {
	head, path, found := strings.Cut(v, "\t")
	if !found {
		return "", "", "", false
	}
	fields := strings.Fields(head)
	if len(fields) != 3 {
		return "", "", "", false
	}
	return fields[0], fields[1], path, true
}
func parseTreeEntrySized(v string) (mode, typ, path string, size int64, ok bool) {
	head, path, found := strings.Cut(v, "\t")
	if !found {
		return
	}
	fields := strings.Fields(head)
	if len(fields) != 4 {
		return
	}
	if _, err := fmt.Sscan(fields[3], &size); err != nil || size < 0 {
		return
	}
	return fields[0], fields[1], path, size, true
}
func safeTreePath(path string) bool {
	if path == "" || len(path) > 1024 || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
func sensitivePath(path string) bool {
	base := filepath.Base(path)
	return path == "index.db" || strings.HasSuffix(path, ".db") || strings.HasSuffix(path, ".db-shm") || strings.HasSuffix(path, ".db-wal") || strings.HasSuffix(path, ".token") || strings.HasPrefix(path, "tokens/") || strings.HasPrefix(path, "identity") || strings.HasPrefix(path, "share/") || base == ".git"
}
