package mora

import (
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

func leaseGuardPath(lockPath string) string { return leasefile.GuardPath(lockPath) }
func withLeaseFileGuard(lockPath string, fn func() error) error {
	return leasefile.WithGuard(lockPath, fn)
}
