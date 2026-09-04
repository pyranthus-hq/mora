//go:build !windows

package companion

// The registry's cross-process write lock on everything that is not Windows.
//
// It is an advisory flock held on an open descriptor, not an O_EXCL sentinel
// file, and the difference is the whole point. An O_EXCL lock file has to be
// removed by somebody, so it needs a staleness rule for the case where its
// holder crashed — and a staleness rule is a second process deciding, from the
// outside, that a lock may be taken away. That decision cannot be made safely:
// between "this lock looks stale" and "I removed it" the original holder may
// wake up and write, and between a holder's "do I still hold it?" check and its
// write, a sweeper may have handed the lock to somebody else. Both are
// check-then-use races and neither is fixable with more checking.
//
// flock has no staleness rule because it needs none: the kernel drops the lock
// when the descriptor closes, including when the process dies. Nothing ever
// removes the lock file, so there is no window in which two holders exist.

import (
	"os"
	"syscall"
	"time"
)

// lockFile is an exclusive lock the kernel holds on behalf of an open
// descriptor.
type lockFile struct {
	fh   *os.File
	path string
}

// acquireLock blocks up to timeout for the exclusive lock on path, creating the
// lock file if it does not exist. The file is never removed — a zero-byte
// `.lock` that outlives every process is the correct steady state, and removing
// it is what reintroduces the two-holders window.
func acquireLock(path string, mode os.FileMode, timeout, poll time.Duration) (*lockFile, error) {
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, mode)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &lockFile{fh: fh, path: path}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
			fh.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			fh.Close()
			return nil, ErrLocked
		}
		time.Sleep(poll)
	}
}

// stillOwns reports whether the lock this holder took is still the lock that
// path names.
//
// # Threat model, stated plainly
//
// This is BEST-EFFORT DETECTION, not a guarantee, and the distinction matters
// enough to spell out.
//
// Nothing in this package ever unlinks a lock file. The case being detected is
// therefore external: an operator, a backup restore, a `rm -rf` on a cache
// directory, a cleaner script. Because the file lives under the user's own
// ConfigDir, that actor runs as the same user and could equally well rewrite
// devices.json directly — so this is not, and cannot be, a defense against a
// hostile local process. It exists because a flock follows the INODE: after an
// unlink the old holder still holds a lock on a file nobody can reach while the
// next caller creates a fresh inode and locks that instead, and the result is
// two writers with no error anywhere. Comparing the descriptor against the path
// turns that silent case into a refusal.
//
// What it does not do is close the window. The check happens immediately before
// the rename, which is as late as it can be, and an unlink landing between that
// check and the rename syscall is still undetected. That residue is accepted:
// shrinking it further requires a lock the filesystem enforces on the rename
// itself, which no portable API offers.
func (l *lockFile) stillOwns() bool {
	held, err := l.fh.Stat()
	if err != nil {
		return false
	}
	named, err := os.Stat(l.path)
	if err != nil {
		return false
	}
	return os.SameFile(held, named)
}

// release drops the lock by closing the descriptor. The kernel would do this
// anyway on exit; doing it explicitly is what lets a second caller in the same
// process proceed.
//
// It returns nothing on purpose. Nothing is ever written to the lock file, so a
// close error carries no lost data, and the lock is released by the kernel
// whether or not close reports success — there is no failure here a caller
// could act on.
func (l *lockFile) release() { l.fh.Close() }

// syncDir flushes a directory entry so a rename survives a crash. Without it an
// atomic rename is only atomic with respect to other readers, not with respect
// to power loss: the new file's contents are on disk and the directory entry
// pointing at them may not be.
func syncDir(dir string) error {
	fh, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = fh.Sync()
	if closeErr := fh.Close(); err == nil {
		err = closeErr
	}
	return err
}
