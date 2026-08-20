// Package storage measures and reports Mora's on-disk footprint.
package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Roots names the product directories whose union forms Mora's footprint.
type Roots struct {
	VaultDir  string
	ConfigDir string
	DataDir   string
	StateDir  string
}

// The shipped storage budget is advisory: Mora reports it but never deletes data automatically.
const (
	TargetBytes  int64 = 3 * (1 << 30)
	CeilingBytes int64 = 15 * (1 << 30)
)

// ProductBytes returns the size of all regular files beneath roots. Canonical
// root overlaps and hard links are counted once. Walk and identity failures are
// returned rather than silently undercounting the footprint.
func ProductBytes(roots Roots) (int64, error) {
	rawRoots := []string{roots.VaultDir, roots.ConfigDir, roots.DataDir, roots.StateDir}
	canonicalRoots := make([]string, 0, len(rawRoots))
	for _, root := range rawRoots {
		if root != "" {
			canonicalRoots = append(canonicalRoots, resolveRealDeep(root))
		}
	}
	sort.Slice(canonicalRoots, func(i, j int) bool {
		if len(canonicalRoots[i]) == len(canonicalRoots[j]) {
			return canonicalRoots[i] < canonicalRoots[j]
		}
		return len(canonicalRoots[i]) < len(canonicalRoots[j])
	})
	rootsDeduped := canonicalRoots[:0]
	for _, candidate := range canonicalRoots {
		nested := false
		for _, seen := range rootsDeduped {
			if pathWithin(candidate, seen) {
				nested = true
				break
			}
		}
		if !nested {
			rootsDeduped = append(rootsDeduped, candidate)
		}
	}

	var total int64
	seen := map[fileIDKey]bool{}
	for _, root := range rootsDeduped {
		if _, err := os.Stat(root); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return 0, err
		}
		if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			key, err := fileIdentity(path, info)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if seen[key] {
				return nil
			}
			seen[key] = true
			size := info.Size()
			if size < 0 || total > math.MaxInt64-size {
				return fmt.Errorf("storage accounting overflow at %s", path)
			}
			total += size
			return nil
		}); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// FileIdentity returns a stable, platform-specific identity for a file.
// It is shared with the lease guard so symlink aliases use the same physical key.
func FileIdentity(path string, info os.FileInfo) (string, error) {
	key, err := fileIdentity(path, info)
	if err != nil {
		return "", err
	}
	return fmt.Sprint(key), nil
}

// dirBytes returns the best-effort size of regular files below root. It is used
// for the legacy vault-only report; ProductBytes is the fail-closed accountant.
func dirBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, infoErr := d.Info(); infoErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// vaultBytes returns the legacy vault-plus-index footprint without double-counting
// an index stored below the vault.
func vaultBytes(vaultDir, indexPath string) int64 {
	total := dirBytes(vaultDir)
	if info, err := os.Stat(indexPath); err == nil {
		vaultReal := resolveReal(vaultDir)
		if !strings.HasPrefix(resolveReal(indexPath), vaultReal+string(os.PathSeparator)) {
			total += info.Size()
		}
	}
	return total
}

// Status classifies a footprint against TargetBytes and CeilingBytes.
func Status(bytes int64) string {
	switch {
	case bytes > CeilingBytes:
		return "over"
	case bytes > TargetBytes:
		return "warn"
	default:
		return "ok"
	}
}

// FormatBytes renders a byte count as a human-readable binary unit.
func FormatBytes(n int64) string {
	const unit = 1 << 10
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func pathWithin(path, root string) bool {
	separator := string(os.PathSeparator)
	return path == root || strings.HasPrefix(path+separator, root+separator)
}

func resolveReal(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func resolveRealDeep(path string) string {
	current := filepath.Clean(path)
	rest := ""
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(path)
		}
		rest = filepath.Join(filepath.Base(current), rest)
		current = parent
	}
}
