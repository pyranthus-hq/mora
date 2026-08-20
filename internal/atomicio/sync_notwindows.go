//go:build !windows

package atomicio

import "os"

// SyncDir fsyncs a directory so a just-renamed directory entry inside it is
// durable — the second half of WriteDurable's crash-ordering guarantee. On
// POSIX os.Rename gives no directory-entry durability on its own
// (rename_notwindows.go), so without this a power loss between a marker rename
// and a later publish can lose the marker while keeping the publish — the
// forbidden false-clean. On darwin File.Sync is fcntl(F_FULLFSYNC), a real device
// barrier for the primary dev platform, free here. Mirrors the build-tag split the
// package already ships (rename_notwindows.go / rename_windows.go).
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
