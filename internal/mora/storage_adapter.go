package mora

import "github.com/pyranthus-hq/mora/internal/storage"

const (
	storageTargetBytes  = storage.TargetBytes
	storageCeilingBytes = storage.CeilingBytes
)

// productStorageBytes keeps Config owned by the composition root.
func productStorageBytes(cfg Config) (int64, error) {
	return storage.ProductBytes(storage.Roots{
		VaultDir: cfg.VaultDir, ConfigDir: cfg.ConfigDir,
		DataDir: cfg.DataDir, StateDir: cfg.StateDir,
	})
}

func storageStatus(bytes int64) string { return storage.Status(bytes) }
func formatBytes(bytes int64) string   { return storage.FormatBytes(bytes) }
