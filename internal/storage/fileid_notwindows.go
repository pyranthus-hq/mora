//go:build !windows

package storage

import (
	"fmt"
	"os"
	"syscall"
)

// fileIDKey uniquely identifies a file on POSIX by (device, inode) so a hard
// link counted under one product root is not double-charged under another.
type fileIDKey struct {
	dev uint64
	ino uint64
}

func fileIdentity(path string, info os.FileInfo) (fileIDKey, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIDKey{}, fmt.Errorf("storage accounting: cannot read file identity for %s", path)
	}
	return fileIDKey{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}
