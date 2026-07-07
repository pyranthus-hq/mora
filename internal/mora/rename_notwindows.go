//go:build !windows

package mora

// renameReplaceRetryable is always false off Windows: POSIX rename(2) atomically
// replaces the target and never returns a transient sharing error, so the retry
// loop in atomicWrite executes os.Rename exactly once and returns its result —
// byte-identical to a bare os.Rename.
func renameReplaceRetryable(error) bool { return false }
