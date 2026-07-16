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

func TestLeaseGuardConvergesUnicodeNormalizationAliases(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("APFS Unicode normalization alias regression")
	}
	root := t.TempDir()
	nfcDir := filepath.Join(root, "caf\u00e9")
	if err := os.MkdirAll(nfcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	nfdDir := filepath.Join(root, "cafe\u0301")
	nfcInfo, err1 := os.Stat(nfcDir)
	nfdInfo, err2 := os.Stat(nfdDir)
	if err1 != nil || err2 != nil || !os.SameFile(nfcInfo, nfdInfo) {
		t.Skip("filesystem does not alias NFC/NFD names")
	}
	p1 := leaseGuardPath(filepath.Join(nfcDir, "import.lock"))
	p2 := leaseGuardPath(filepath.Join(nfdDir, "import.lock"))
	for _, path := range []string{p1, p2} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	i1, err1 := os.Stat(p1)
	i2, err2 := os.Stat(p2)
	if err1 != nil || err2 != nil || !os.SameFile(i1, i2) {
		t.Fatalf("NFC/NFD aliases selected distinct guards: %q vs %q", p1, p2)
	}
}

func TestLeaseGuardIdentitySurvivesRemovableRootRecreation(t *testing.T) {
	root := t.TempDir()
	subRoot := filepath.Join(root, "share", "subs", "neil")
	if err := os.MkdirAll(subRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(subRoot, "import.lock")
	preferredBefore := leaseGuardPath(lockPath)
	if err := os.RemoveAll(subRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(subRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := leaseGuardPath(lockPath); got != preferredBefore {
		t.Fatalf("removable-root recreation changed preferred guard: %q -> %q", preferredBefore, got)
	}
}

func TestLeaseGuardConvergesSymlinkedLockDirectory(t *testing.T) {
	root := t.TempDir()
	realConfig := filepath.Join(root, "real", "config")
	if err := os.MkdirAll(realConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "config-link")
	if err := os.Symlink(realConfig, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	direct := filepath.Join(realConfig, "sources.json.lock")
	throughAlias := filepath.Join(alias, "sources.json.lock")
	if got, want := leaseGuardPath(throughAlias), leaseGuardPath(direct); got != want {
		t.Fatalf("symlinked lock directory selected a different guard: %q vs %q", got, want)
	}
}
