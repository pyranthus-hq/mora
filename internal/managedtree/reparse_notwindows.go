//go:build !windows

package managedtree

import (
	"os"
	"syscall"
)

const readFlags = syscall.O_NONBLOCK | syscall.O_NOFOLLOW

func unsafeReparse(string, os.FileInfo) (bool, error) { return false, nil }
