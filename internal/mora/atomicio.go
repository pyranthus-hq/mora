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
