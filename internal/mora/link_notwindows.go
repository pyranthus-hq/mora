//go:build !windows

package mora

import (
	"errors"
	"syscall"
)

// linkUnsupported reports whether a failed os.Link is the filesystem refusing hard
// links ALTOGETHER (exFAT/FAT32 USB sticks, some SMB/NFS mounts) rather than a
// collision (handled separately as os.ErrExist) or a real IO/permission fault.
// link(2) on a regular file returns EPERM on a filesystem that does not support
// hard links (the documented meaning when the source is not a directory); some
// backends report ENOTSUP/EOPNOTSUPP. os.Link wraps the errno in an *os.LinkError,
// so errors.Is unwraps to match.
//
// The set is deliberately CONSERVATIVE: EACCES, ENOSPC, EROFS, EIO, etc. are NOT
// included, so a genuine fault still surfaces as a hard error. Even if EPERM were
// occasionally returned for another reason, the fallback it selects (O_CREATE|
// O_EXCL claim + rename) is itself safe — it preserves no-clobber and surfaces any
// real failure from those operations rather than masking it. Mirrors
// renameReplaceRetryable's build-tag split.
func linkUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, errors.ErrUnsupported)
}
