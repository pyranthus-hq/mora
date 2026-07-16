//go:build !windows

package mora

import (
	"os"
	"syscall"
)

// fileIDKey uniquely identifies a file on POSIX by (device, inode) so a hard
// link counted under one product root is not double-charged under another.
type fileIDKey struct {
	dev uint64
	ino uint64
}

func fileIdentity(info os.FileInfo) (fileIDKey, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIDKey{}, false
	}
	return fileIDKey{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true
}
