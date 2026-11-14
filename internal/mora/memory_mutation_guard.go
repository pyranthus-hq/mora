package mora

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"github.com/pyranthus-hq/mora/internal/leasefile"
)

// withMemoryMutationGuard serializes all supported changes to one saved memory.
// The expected-revision checks must run inside this guard to be real CAS checks.
func withMemoryMutationGuard(cfg Config, id string, fn func() error) error {
	sum := sha256.Sum256([]byte(id))
	path := filepath.Join(cfg.StateDir, "memory-mutation-locks", hex.EncodeToString(sum[:])+".lock")
	return leasefile.WithGuard(path, fn)
}
