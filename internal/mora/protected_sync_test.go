package mora

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func protectedSyncTestToken() string { return "0123456789abcdef" + "0123456789abcdef" }

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProtectedSyncReceiptRejectsUnprotectedSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	err := Run(context.Background(), []string{"sync", "google", protectedSyncReceiptFlag, protectedSyncTestToken()}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "only valid") {
		t.Fatalf("unprotected receipt error = %v", err)
	}
}

func TestProtectedSyncAppRootUsesInstalledAppForStandaloneCLI(t *testing.T) {
	home := t.TempDir()
	app := filepath.Join(home, "Applications", moraAppName)
	inner := filepath.Join(app, "Contents", "MacOS", "mora")
	if err := os.MkdirAll(filepath.Dir(inner), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inner, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldHome := protectedSyncUserHomeDir
	protectedSyncUserHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { protectedSyncUserHomeDir = oldHome })

	got, ok := protectedSyncAppRoot(filepath.Join(home, "bin", "mora"))
	if !ok || got != app {
		t.Fatalf("protectedSyncAppRoot standalone = %q, %v; want %q, true", got, ok, app)
	}
}

func TestProtectedSyncAppRootPrefersRunningBundle(t *testing.T) {
	app := filepath.Join(t.TempDir(), moraAppName)
	inner := filepath.Join(app, "Contents", "MacOS", "mora")
	got, ok := protectedSyncAppRoot(inner)
	if !ok || got != app {
		t.Fatalf("protectedSyncAppRoot bundle = %q, %v; want %q, true", got, ok, app)
	}
}
