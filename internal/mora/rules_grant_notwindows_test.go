//go:build !windows

package mora

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRulesSignersProtected(t *testing.T) {
	mine := filepath.Join(t.TempDir(), "allowed_signers")
	if err := os.WriteFile(mine, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rulesSignersProtected(mine) {
		t.Fatal("a file this account owns counted as protected")
	}
	if rulesSignersProtected(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("a missing file counted as protected")
	}
	if os.Geteuid() != 0 && !rulesSignersProtected("/etc/hosts") {
		t.Skip("/etc/hosts is not root-owned on this machine")
	}
}
