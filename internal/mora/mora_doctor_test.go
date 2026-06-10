package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorFlagsTokenDirInsideVault(t *testing.T) {
	if disjointRealPaths("/a/vault", "/a/vault/tokens") {
		t.Fatal("token dir inside vault must NOT be disjoint")
	}
	if !disjointRealPaths("/a/vault", "/b/config/tokens") {
		t.Fatal("separate dirs must be disjoint")
	}
}

func TestDoctorFlagsSymlinkedTokenDirInsideVault(t *testing.T) {
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	tokens := filepath.Join(vault, "tokens")
	link := filepath.Join(root, "token-link")

	if err := os.MkdirAll(tokens, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tokens, link); err != nil {
		t.Fatal(err)
	}

	if disjointRealPaths(vault, link) {
		t.Fatal("token dir symlink resolving inside vault must NOT be disjoint")
	}
}

func TestDoctorWarnsSyncedRoot(t *testing.T) {
	if !looksSynced("/Users/x/Library/Mobile Documents/com~apple~CloudDocs/mora/tokens") {
		t.Fatal("iCloud path should be flagged as synced")
	}
	if looksSynced("/Users/x/.config/mora/tokens") {
		t.Fatal("plain config path should not be flagged")
	}
	_ = strings.TrimSpace
}
