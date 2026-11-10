//go:build !windows

package mora

import "syscall"

// givenOpenFlags keep an append from following a symlink or blocking on a FIFO.
const givenOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// givenRetry runs op once; only Windows needs a retry for shared-file races.
func givenRetry(op func() error) error { return op() }
