//go:build !windows

package atomicio

// renameReplaceRetryable / SharingViolationRetryable are always false off
// Windows: POSIX open(2)/link(2)/unlink(2)/rename(2) never return a transient
// sharing-violation, so the retry loops (RenameReplaceWithRetry, a caller's
// lock open/read/reap) execute their file op exactly once and return its
// result — byte-identical to the bare stdlib call.
func renameReplaceRetryable(error) bool    { return false }
func SharingViolationRetryable(error) bool { return false }
