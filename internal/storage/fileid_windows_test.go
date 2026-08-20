//go:build windows

package storage

import (
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestFileIdentityRetriesTransientSharingViolation(t *testing.T) {
	path := t.TempDir() + `\identity.db-wal`
	if err := os.WriteFile(path, []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	original := fileIdentityCreateFile
	calls := 0
	fileIdentityCreateFile = func(
		name *uint16,
		access uint32,
		mode uint32,
		sa *windows.SecurityAttributes,
		createMode uint32,
		attrs uint32,
		template windows.Handle,
	) (windows.Handle, error) {
		calls++
		if calls == 1 {
			return 0, syscall.ERROR_ACCESS_DENIED
		}
		return original(name, access, mode, sa, createMode, attrs, template)
	}
	t.Cleanup(func() { fileIdentityCreateFile = original })

	if _, err := fileIdentity(path, info); err != nil {
		t.Fatalf("fileIdentity did not recover from transient ACCESS_DENIED: %v", err)
	}
	if calls < 2 {
		t.Fatalf("CreateFile calls = %d; want a retry", calls)
	}
}
