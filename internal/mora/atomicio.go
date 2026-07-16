package mora

import (
	"os"
	"path/filepath"
)

func atomicWrite(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Stage through a unique temp file (not a fixed `<path>.tmp`) so two
	// processes writing the same target never share, truncate, or rename each
	// other's in-flight temp. The temp stays in
	// the target dir so the final os.Rename remains atomic on the same
	// filesystem. NOTE: this does not by itself fix the higher-level
	// read-modify-write lost-update on sources.json (two writers each load →
	// mutate → save); that serialization is provided by mutateSources /
	// acquireSourcesLock (sources_lock.go), which hold a lease around the whole
	// load → mutate → save cycle.
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
	return renameReplaceWithRetry(tmp, path)
}

// markerSyncFn and syncDirFn are injectable seams over the two crash-durability
// barriers atomicWriteDurable applies: the temp file's own data sync (f.Sync)
// and the parent-directory sync (syncDir). Production always uses the real
// barriers; the durable-marker call-trace tests (TestDurableMarkerFsyncsBeforeRename /
// TestDurableMarkerSyncsDirBeforeReturn) install a RECORDING wrapper so they can
// assert the call ORDER — an fsync is unobservable from userspace (a process
// crash preserves page-cache data), so only a call-trace can distinguish a synced
// write from an unsynced one. The mutation-matrix mutation is DELETING the
// production call in atomicWriteDurable's body, never flipping these seams to a
// no-op (which would collide with the test's own wrapper and prove nothing).
var (
	markerSyncFn = (*os.File).Sync
	syncDirFn    = syncDir
)

// testHookPostMarkerWrite, when non-nil (tests only), fires inside markIndexDirty
// AFTER atomicWriteDurable has fully returned — i.e. the pending marker is on
// stable storage (file + parent dir synced) and BEFORE the caller publishes the
// vault file. It is the seam TestMarkerSurvivesCrashBeforeVaultPublish uses to
// simulate a crash in exactly the mark-before-visible window. Nil in production.
var testHookPostMarkerWrite func()

// atomicWriteDurable is atomicWrite with two added crash-durability barriers, for
// the ONE class of write whose loss is a forbidden false-clean: a pending-op
// marker (or ingest journal header) that must survive a power loss so a crash
// between the marker landing and the vault publish leaves the index readable as
// DIRTY, never fresh-and-missing. Over plain atomicWrite it adds:
//
//   - f.Sync() BEFORE the rename, so the marker's bytes reach stable storage
//     before the directory entry that names them (ordering, not just presence).
//   - syncDir(dir) after the rename, so the directory entry itself is durable
//     (a bare rename gives neither on POSIX — see rename_notwindows.go).
//
// Both barriers propagate their errors (a swallowed fsync reproduces the exact
// silent-degradation class this gate exists to kill). Deliberately NOT applied to
// atomicWrite's 33 non-durability-critical call sites: an F_FULLFSYNC per ingest
// write (tens of ms on macOS/darwin, where File.Sync is fcntl(F_FULLFSYNC)) would
// be a real throughput regression. The temp is staged inside filepath.Dir(path),
// so the rename never crosses a filesystem and EXDEV is not a risk.
func atomicWriteDurable(path string, body []byte, mode os.FileMode) error {
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
	if err := markerSyncFn(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := renameReplaceWithRetry(tmp, path); err != nil {
		return err
	}
	// Parent-directory sync so the renamed directory entry is itself durable
	// before this call returns (and thus before any vault publish begins).
	return syncDirFn(dir)
}
func appendFile(path, line string) error {
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
