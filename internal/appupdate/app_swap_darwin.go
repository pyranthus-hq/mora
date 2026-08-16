//go:build darwin

package appupdate

import "golang.org/x/sys/unix"

// atomicSwapMoraAppDirectories uses the Darwin rename-swap primitive. Both
// directories continue to exist at every instant, and the old app lands at the
// staging path so a failed post-swap verification can use the same operation to
// roll back.
func atomicSwapMoraAppDirectories(installedApp, stagedApp string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, installedApp, unix.AT_FDCWD, stagedApp, unix.RENAME_SWAP)
}
