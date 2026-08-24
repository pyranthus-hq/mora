package mora

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	protectedsyncpkg "github.com/pyranthus-hq/mora/internal/protectedsync"
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

func TestProtectedSyncRelayPreservesJSONContract(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	origGOOS := runtimeGOOS
	origExecutable := protectedSyncExecutable
	origRunOpen := protectedSyncRunOpen
	t.Cleanup(func() {
		runtimeGOOS = origGOOS
		protectedSyncExecutable = origExecutable
		protectedSyncRunOpen = origRunOpen
	})
	runtimeGOOS = func() string { return "darwin" }
	app := filepath.Join(t.TempDir(), moraAppName)
	protectedSyncExecutable = func() (string, error) {
		return filepath.Join(app, "Contents", "MacOS", "mora"), nil
	}
	protectedSyncRunOpen = func(_ context.Context, args ...string) error {
		token := args[len(args)-1]
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return protectedsyncpkg.WriteReceipt(cfg.StateDir, protectedsyncpkg.Receipt{
			Token: token, Source: "imessage", Items: 17, CompletedAt: "2026-08-24T00:00:00Z",
		})
	}

	stdout, stderr, err := runSplit(t, "sync", "imessage", "--json")
	if err != nil {
		t.Fatalf("sync imessage --json: %v (stderr: %s)", err, stderr)
	}
	var got struct {
		Schema        string `json:"schema"`
		SchemaVersion int    `json:"schema_version"`
		Source        string `json:"source"`
		Items         int    `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("relay stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if got.Schema != "mora.sync.imessage" || got.SchemaVersion != 1 || got.Source != "imessage" || got.Items != 17 {
		t.Fatalf("relay receipt = %+v", got)
	}
	if strings.Contains(stdout, "via Mora.app") {
		t.Fatalf("relay mixed prose into JSON stdout: %q", stdout)
	}
}

func TestProtectedSyncReceiptRejectsUnprotectedSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	err := Run(context.Background(), []string{"sync", "google", protectedSyncReceiptFlag, protectedSyncTestToken()}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "only valid") {
		t.Fatalf("unprotected receipt error = %v", err)
	}
}
