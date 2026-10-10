package leasefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestIsContendedOnlyForAHeldLock: a second TryLock on a held lock is
// contention; any other error (no locking support, bad fd) is not.
func TestIsContendedOnlyForAHeldLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := TryLock(a); err != nil {
		t.Fatal(err)
	}
	err = TryLock(b)
	if err == nil || !IsContended(err) {
		t.Fatalf("second TryLock = %v; want a contention error", err)
	}
	if IsContended(errors.New("locking not supported")) || IsContended(nil) {
		t.Fatal("only lock contention may count as contended")
	}
}
