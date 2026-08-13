// Package atomicio provides atomic (torn-write-safe) file write primitives:
// rename-based publish, an OPT-IN crash-durability barrier (WriteDurable) for
// markers that must survive a power loss before the next step becomes
// visible, and append. Write and AppendFile are atomic but NOT
// crash-durable on their own — a rename lands or it doesn't, but the OS is
// free to delay flushing it to stable storage. Only WriteDurable pays for the
// fsync + parent-dir-sync barriers; see its doc for which writes need that.
// Leaf package: no dependency on any other internal package.
package atomicio

import (
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

// Write atomically replaces path's content with body. It stages through a
// unique temp file (not a fixed `<path>.tmp`) so two processes writing the
// same target never share, truncate, or rename each other's in-flight temp.
// The temp stays in the target dir so the final os.Rename remains atomic on
// the same filesystem. NOTE: this does not by itself fix a higher-level
// read-modify-write lost-update on a shared file (two writers each load →
// mutate → save); that serialization is the caller's responsibility (e.g. a
// lease held around the whole load → mutate → save cycle).
func Write(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Remove the temp on any failure path; a no-op once the rename succeeds.
	defer os.Remove(tmp)
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// CreateTemp opens at 0600; raise to the caller's requested mode.
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return RenameReplaceWithRetry(tmp, path)
}

// MarkerSyncFn and SyncDirFn are injectable seams over the two crash-durability
// barriers WriteDurable applies: the temp file's own data sync (f.Sync) and the
// parent-directory sync (SyncDir). Production always uses the real barriers;
// callers' durable-marker call-trace tests install a RECORDING wrapper so they
// can assert the call ORDER — an fsync is unobservable from userspace (a
// process crash preserves page-cache data), so only a call-trace can
// distinguish a synced write from an unsynced one.
var (
	MarkerSyncFn = (*os.File).Sync
	SyncDirFn    = SyncDir
)

// WriteDurable is Write with two added crash-durability barriers, for the ONE
// class of write whose loss is a forbidden false-clean: a pending-op marker
// (or ingest journal header) that must survive a power loss so a crash
// between the marker landing and the caller's next publish leaves the
// dependent state readable as DIRTY, never fresh-and-missing. Over plain
// Write it adds:
//
//   - f.Sync() BEFORE the rename, so the marker's bytes reach stable storage
//     before the directory entry that names them (ordering, not just presence).
//   - SyncDir(dir) after the rename, so the directory entry itself is durable
//     (a bare rename gives neither on POSIX — see rename_notwindows.go).
//
// Both barriers propagate their errors (a swallowed fsync reproduces the exact
// silent-degradation class this exists to kill). Deliberately NOT applied by
// Write's non-durability-critical callers: an F_FULLFSYNC per write (tens of
// ms on macOS/darwin, where File.Sync is fcntl(F_FULLFSYNC)) would be a real
// throughput regression for high-volume writes. The temp is staged inside
// filepath.Dir(path), so the rename never crosses a filesystem and EXDEV is
// not a risk.
func WriteDurable(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds; cleanup on any error path
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	// Data sync MUST precede Close+rename: the bytes go to stable storage before
	// the directory entry that names them, so a crash cannot leave a named-but-
	// empty marker.
	if err := MarkerSyncFn(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := RenameReplaceWithRetry(tmp, path); err != nil {
		return err
	}
	// Parent-directory sync so the renamed directory entry is itself durable
	// before this call returns (and thus before any caller-visible publish
	// begins).
	return SyncDirFn(dir)
}

// AppendFile appends line to the file at path, creating the parent directory
// and the file if needed.
func AppendFile(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

// RenameReplaceWithRetry publishes tmp onto path via os.Rename, replacing any
// existing target. On POSIX this is a single atomic rename(2) and the loop runs
// exactly once. On Windows, os.Rename maps to MoveFileEx(MOVEFILE_REPLACE_
// EXISTING): replacing an existing target requires deleting it, so concurrent
// writers racing to rename onto the SAME target transiently fail with
// ERROR_ACCESS_DENIED / ERROR_SHARING_VIOLATION. Retry those with JITTERED, capped
// backoff — deterministic backoff makes racing writers retry in lockstep and keep
// colliding, so the jitter is what lets them de-correlate — up to a deadline. Only
// the error path pays this; a permanent error (or any non-Windows error) surfaces
// on the first attempt.
func RenameReplaceWithRetry(tmp, path string) error {
	var deadline time.Time
	for attempt := 0; ; attempt++ {
		rerr := os.Rename(tmp, path)
		if rerr == nil {
			return nil
		}
		if !renameReplaceRetryable(rerr) {
			return rerr
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(5 * time.Second)
		} else if !time.Now().Before(deadline) {
			return rerr
		}
		capMs := 1 << min(attempt, 5) // backoff ceiling grows 1,2,4,8,16,32,32… ms
		time.Sleep(time.Duration(1+mrand.IntN(capMs)) * time.Millisecond)
	}
}
