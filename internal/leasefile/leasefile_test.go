package leasefile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

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
	p1 := GuardPath(filepath.Join(nfcDir, "import.lock"))
	p2 := GuardPath(filepath.Join(nfdDir, "import.lock"))
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
	preferredBefore := GuardPath(lockPath)
	if err := os.RemoveAll(subRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(subRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := GuardPath(lockPath); got != preferredBefore {
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
	if got, want := GuardPath(throughAlias), GuardPath(direct); got != want {
		t.Fatalf("symlinked lock directory selected a different guard: %q vs %q", got, want)
	}
}

func TestWithGuardSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		if err := WithGuard(path, func() error { close(entered); <-release; return nil }); err != nil {
			panic(err)
		}
	}()
	<-entered
	go func() {
		if err := WithGuard(path, func() error { close(done); return nil }); err != nil {
			panic(err)
		}
	}()
	select {
	case <-done:
		t.Fatal("second guard entered early")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second guard did not enter")
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := WithGuard(path, func() error { return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestGuardEdges(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err == nil {
		got := resolveRealDeep(filepath.Join(alias, "missing", "lock"))
		physical, err := filepath.EvalSymlinks(real)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(physical, "missing", "lock")
		if got != want {
			t.Fatalf("resolved=%q want %q", got, want)
		}
	}
	if got := resolveRealDeep(filepath.Join(root, "absent", "lock")); got == "" {
		t.Fatal("empty fallback")
	}
	guard := GuardPath(filepath.Join(real, "x.lock"))
	if err := os.MkdirAll(filepath.Dir(guard), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(guard, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := TryLock(f); err != nil {
		t.Fatal(err)
	}
	if err := Unlock(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("callback")
	if err := WithGuard(filepath.Join(real, "callback.lock"), func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("callback err=%v", err)
	}
	blocker := filepath.Join(root, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WithGuard(filepath.Join(blocker, "x.lock"), func() error { return nil }); err == nil {
		t.Fatal("invalid parent accepted")
	}
}
