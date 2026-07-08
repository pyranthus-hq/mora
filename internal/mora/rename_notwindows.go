//go:build !windows

package mora

// renameReplaceRetryable / sharingViolationRetryable are always false off Windows:
// POSIX open(2)/link(2)/unlink(2)/rename(2) never return a transient
// sharing-violation, so the retry loops (atomicWrite's rename, acquireSourcesLock's
// lock open/read/reap) execute their file op exactly once and return its result —
// byte-identical to the bare stdlib call.
func renameReplaceRetryable(error) bool    { return false }
func sharingViolationRetryable(error) bool { return false }
