// Package leasefile owns cross-process path-keyed guard locks.
package leasefile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pyranthus-hq/mora/internal/storage"

	"golang.org/x/text/unicode/norm"
)

// leaseGuardPath returns one deterministic guard location within the writable
// Mora root. Removable import/loop roots use their stable parent; other locks use
// their own containing root. Every guard ends in .lock, so vault Git ignores it.
// The canonical path hash also makes filesystem aliases share one guard.
func GuardPath(lockPath string) string {
	resolved, identity := Identity(lockPath)
	sum := sha256.Sum256([]byte(identity))
	name := hex.EncodeToString(sum[:]) + ".guard.lock"
	lockDir := filepath.Dir(resolved)
	guardParent := lockDir
	// These lock directories are explicitly removable. Put their persistent
	// guard one level up so RemoveAll cannot create a second guard inode.
	if filepath.Base(resolved) == "import.lock" ||
		(filepath.Base(resolved) == ".lock" && filepath.Base(filepath.Dir(lockDir)) == "loops") {
		guardParent = filepath.Dir(lockDir)
	}
	return filepath.Join(guardParent, ".mora-lease-guards", name)
}

// leaseGuardIdentity binds the guard to the filesystem identity of the deepest
// existing ancestor plus a normalized missing tail. Path-string canonicalizing
// alone is insufficient on APFS: NFC/NFD spellings can name one inode while Go
// preserves two different strings.
func Identity(lockPath string) (resolved, identity string) {
	resolved = filepath.Clean(lockPath)
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	// Stable roots (ConfigDir, VaultDir, share/) resolve the existing lockDir
	// itself so a symlinked Mora root and its direct spelling converge. Explicitly
	// removable subscription/loop dirs instead anchor at their resolved parent and
	// keep the removable basename in the normalized tail, so delete+recreate does
	// not change the identity.
	originalLockDir := filepath.Dir(resolved)
	removable := filepath.Base(resolved) == "import.lock" ||
		(filepath.Base(resolved) == ".lock" && filepath.Base(filepath.Dir(originalLockDir)) == "loops")
	var cur string
	var reverseTail []string
	if removable {
		stableParent := filepath.Dir(originalLockDir)
		if evaluated, err := filepath.EvalSymlinks(stableParent); err == nil {
			stableParent = evaluated
		}
		resolved = filepath.Join(stableParent, filepath.Base(originalLockDir), filepath.Base(resolved))
		cur = stableParent
		reverseTail = []string{filepath.Base(resolved), filepath.Base(originalLockDir)}
	} else if realLockDir, err := filepath.EvalSymlinks(originalLockDir); err == nil {
		resolved = filepath.Join(realLockDir, filepath.Base(resolved))
		cur = filepath.Dir(realLockDir)
		reverseTail = []string{filepath.Base(resolved)}
		if cur != realLockDir {
			reverseTail = append(reverseTail, filepath.Base(realLockDir))
		} else {
			cur = realLockDir
		}
	} else {
		cur = filepath.Dir(originalLockDir)
		reverseTail = []string{filepath.Base(resolved)}
		if cur != originalLockDir {
			reverseTail = append(reverseTail, filepath.Base(originalLockDir))
		} else {
			cur = originalLockDir
		}
	}
	for {
		info, err := os.Stat(cur)
		if err == nil {
			real := cur
			if evaluated, evalErr := filepath.EvalSymlinks(cur); evalErr == nil {
				real = evaluated
				if evaluatedInfo, statErr := os.Stat(real); statErr == nil {
					info = evaluatedInfo
				}
			}
			for i, j := 0, len(reverseTail)-1; i < j; i, j = i+1, j-1 {
				reverseTail[i], reverseTail[j] = reverseTail[j], reverseTail[i]
			}
			physicalTail := filepath.Join(reverseTail...)
			tail := norm.NFC.String(physicalTail)
			if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
				tail = strings.ToLower(tail)
			}
			// Normalization/case folding belongs only to the identity hash. Keep the
			// actual filesystem spelling for guard placement on case-sensitive or
			// normalization-sensitive volumes.
			resolved = filepath.Join(real, physicalTail)
			if key, keyErr := storage.FileIdentity(real, info); keyErr == nil {
				return resolved, fmt.Sprintf("%v|%s", key, tail)
			}
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		reverseTail = append(reverseTail, filepath.Base(cur))
		cur = parent
	}
	identity = norm.NFC.String(resolveRealDeep(resolved))
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		identity = strings.ToLower(identity)
	}
	return resolved, identity
}

// withLeaseFileGuard serializes every publish/reap/release/heartbeat transition
// for one path across processes. A crash releases the OS lock automatically, so
// the guard needs no second TTL protocol.
func WithGuard(lockPath string, fn func() error) (err error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	guardPath := GuardPath(lockPath)
	if err := os.MkdirAll(filepath.Dir(guardPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(guardPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err // fail closed; never switch this lease to a second guard domain
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if err := lock(f); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, Unlock(f)) }()
	return fn()
}

func resolveRealDeep(p string) string {
	cur := filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(p)
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
