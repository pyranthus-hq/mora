package mora

import (
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"os"
)

func leaseGuardPath(lockPath string) string { return leasefile.GuardPath(lockPath) }
func withLeaseFileGuard(lockPath string, fn func() error) error {
	return leasefile.WithGuard(lockPath, fn)
}
func unlockLeaseGuard(f *os.File) error  { return leasefile.Unlock(f) }
func tryLockLeaseGuard(f *os.File) error { return leasefile.TryLock(f) }
