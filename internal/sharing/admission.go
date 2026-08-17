package sharing

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/storage"
)

// StorageLimit is the durable whole-product admission decision.
type StorageLimit struct {
	Bytes     int64  `json:"bytes"`
	UpdatedAt string `json:"updated_at"`
}

// StorageLimitPath returns ConfigDir/share/storage-limit.json.
func StorageLimitPath(configDir string) string {
	return filepath.Join(configDir, "share", "storage-limit.json")
}

// LoadStorageLimit loads and validates the durable decision, or returns defaultBytes when absent.
func LoadStorageLimit(configDir string, defaultBytes int64) (int64, error) {
	path := StorageLimitPath(configDir)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultBytes, nil
		}
		return 0, err
	}
	var lim StorageLimit
	if err := json.Unmarshal(b, &lim); err != nil {
		return 0, fmt.Errorf("%s is corrupt: %w", path, err)
	}
	if lim.Bytes <= 0 {
		return 0, fmt.Errorf("%s is corrupt: bytes must be positive", path)
	}
	return lim.Bytes, nil
}

// WriteStorageLimit durably replaces the admission decision with canonical JSON.
func WriteStorageLimit(configDir string, bytes int64, updatedAt time.Time) error {
	lim := StorageLimit{Bytes: bytes, UpdatedAt: updatedAt.UTC().Format(time.RFC3339)}
	body, err := json.MarshalIndent(lim, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(StorageLimitPath(configDir), append(body, '\n'), 0o600)
}

// StorageAdmission re-accounts the caller's complete product roots against one stable limit.
type StorageAdmission struct {
	roots storage.Roots
	name  string
	limit int64
}

// NewStorageAdmission loads one stable limit for subsequent checks.
func NewStorageAdmission(roots storage.Roots, configDir, name string, defaultBytes int64) (*StorageAdmission, error) {
	limit, err := LoadStorageLimit(configDir, defaultBytes)
	if err != nil {
		return nil, err
	}
	return NewStorageAdmissionWithLimit(roots, name, limit), nil
}

// NewStorageAdmissionWithLimit constructs an admission check from a caller-established decision.
func NewStorageAdmissionWithLimit(roots storage.Roots, name string, limit int64) *StorageAdmission {
	return &StorageAdmission{roots: roots, name: name, limit: limit}
}

func (a *StorageAdmission) used() (int64, error) {
	used, err := storage.ProductBytes(a.roots)
	if err != nil {
		return 0, fmt.Errorf("share %q: storage accounting failed (fail-closed): %w", a.name, err)
	}
	return used, nil
}

// CheckAdditional verifies the current footprint plus next bytes.
func (a *StorageAdmission) CheckAdditional(next int64) error {
	if next < 0 {
		return fmt.Errorf("share %q: invalid negative storage reservation %d", a.name, next)
	}
	used, err := a.used()
	if err != nil {
		return err
	}
	if used > math.MaxInt64-next {
		return fmt.Errorf("share %q: storage reservation overflows int64", a.name)
	}
	return a.checkNeeded(used + next)
}

// CheckCurrent verifies the current footprint.
func (a *StorageAdmission) CheckCurrent() error { return a.CheckAdditional(0) }

// Remaining returns admissible bytes after fail-closed accounting.
func (a *StorageAdmission) Remaining() (int64, error) {
	used, err := a.used()
	if err != nil {
		return 0, err
	}
	if err := a.checkNeeded(used); err != nil {
		return 0, err
	}
	return a.limit - used, nil
}
func (a *StorageAdmission) checkNeeded(needed int64) error {
	if needed <= a.limit {
		return nil
	}
	const decisionFileHeadroom = int64(4 << 10)
	required := needed
	if required <= math.MaxInt64-decisionFileHeadroom {
		required += decisionFileHeadroom
	}
	return fmt.Errorf("share %q needs at least %d whole-product bytes; configured limit is %d (doctor ceiling 15 GiB). Run 'mora share storage-limit %d' to opt in, free space/run 'mora share gc', or unsubscribe.", a.name, required, a.limit, required)
}

// AdmitGenerationBytes reserves conservative corpus, index, page, and per-row expansion.
func AdmitGenerationBytes(roots storage.Roots, configDir, name string, defaultBytes, corpusBytes int64, entries int) error {
	const (
		expansion = int64(8)
		base      = int64(64 << 10)
		row       = int64(4 << 10)
	)
	if corpusBytes < 0 || entries < 0 || corpusBytes > (math.MaxInt64-base)/expansion {
		return fmt.Errorf("share %q: generation reservation overflows int64", name)
	}
	count := int64(entries)
	reserve := corpusBytes*expansion + base
	if count > (math.MaxInt64-reserve)/row {
		return fmt.Errorf("share %q: generation reservation overflows int64", name)
	}
	reserve += count * row
	a, err := NewStorageAdmission(roots, configDir, name, defaultBytes)
	if err != nil {
		return err
	}
	return a.CheckAdditional(reserve)
}

// ParseByteSize accepts bytes or a case-insensitive binary-unit suffix.
func ParseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	up := strings.ToUpper(s)
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(up, u.suf) {
			mult = u.m
			up = strings.TrimSpace(strings.TrimSuffix(up, u.suf))
			break
		}
	}
	n, err := strconv.ParseInt(up, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q — use a byte count or a binary unit like 15GiB", s)
	}
	if n < 0 || (mult != 0 && n > math.MaxInt64/mult) {
		return 0, fmt.Errorf("invalid size %q — value is out of range", s)
	}
	return n * mult, nil
}
