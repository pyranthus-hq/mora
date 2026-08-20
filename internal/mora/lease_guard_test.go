package mora

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestLeaseGuardStaysInsideWritableRootWhenParentIsUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not model an unwritable parent on Windows")
	}
	outer := t.TempDir()
	parent := filepath.Join(outer, "mount-parent")
	root := filepath.Join(parent, "valid-root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	lockPath := filepath.Join(root, "sources.json.lock")
	body, _ := json.Marshal(loopLockBody{RunID: "owner", PID: os.Getpid(), AcquiredAt: time.Now().UTC().Format(time.RFC3339Nano)})
	published, err := publishLockFile(lockPath, body)
	if err != nil || !published {
		t.Fatalf("publish under writable root with unwritable parent = %v, %v", published, err)
	}
	guardPath := leaseGuardPath(lockPath)
	guardRootInfo, guardErr := os.Stat(filepath.Dir(filepath.Dir(guardPath)))
	rootInfo, rootErr := os.Stat(root)
	if guardErr != nil || rootErr != nil || !os.SameFile(guardRootInfo, rootInfo) {
		t.Fatalf("guard escaped writable root: %s", guardPath)
	}
	if _, err := os.Stat(guardPath); err != nil {
		t.Fatalf("in-root guard was not used: %v", err)
	}
	if !heartbeatLockFileFor(lockPath, "owner", time.Now()) {
		t.Fatal("guard could not serialize heartbeat")
	}
	releaseLockFileFor(lockPath, "owner")
}

func TestLeaseGuardPreservesPhysicalUnicodeSpelling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not model an unwritable parent on Windows")
	}
	outer := t.TempDir()
	parent := filepath.Join(outer, "mount-parent")
	// Uppercase + NFD catches both accidental case folding and NFC rewriting of
	// the physical path on normalization-sensitive filesystems.
	root := filepath.Join(parent, "Cafe\u0301")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	lockPath := filepath.Join(root, "sources.json.lock")
	body, _ := json.Marshal(loopLockBody{RunID: "unicode-owner", PID: os.Getpid(), AcquiredAt: time.Now().UTC().Format(time.RFC3339Nano)})
	published, err := publishLockFile(lockPath, body)
	if err != nil || !published {
		t.Fatalf("Unicode root lock publish = %v, %v", published, err)
	}
	guardRoot, guardErr := os.Stat(filepath.Dir(filepath.Dir(leaseGuardPath(lockPath))))
	rootInfo, rootErr := os.Stat(root)
	if guardErr != nil || rootErr != nil || !os.SameFile(guardRoot, rootInfo) {
		t.Fatalf("guard path did not preserve the configured root spelling: %s", leaseGuardPath(lockPath))
	}
	releaseLockFileFor(lockPath, "unicode-owner")
}
