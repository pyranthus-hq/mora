package mora

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMainPrintsMoraErrorDocumentForJSONFailures(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	binary := filepath.Join(t.TempDir(), "mora")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "../../cmd/mora")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	home := lookupTestEnv(t).home
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{{Name: "applecalendar", Type: "applecalendar"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		code string
	}{
		{"coded", []string{"sync", "filesystem", "--bad-k17-flag", "--json"}, "usage.unknown_flag"},
		{"unclassified", []string{"unknown-k17-command", "--json"}, "unclassified"},
		{"human", []string{"unknown-k17-command"}, ""},
		{"receipt_then_error", []string{"sync", "applecalendar", "--json"}, "connector_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "receipt_then_error" && runtime.GOOS != "darwin" {
				t.Skip("Apple Calendar ingest is macOS-only")
			}
			cmd := exec.Command(binary, tc.args...)
			cmd.Env = append(os.Environ(), "MORA_CONFIG_DIR="+cfg.ConfigDir, "HOME="+home, "USERPROFILE="+home, "MORA_VAULT="+cfg.VaultDir)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("exit: %v", err)
			}
			if stderr.Len() == 0 {
				t.Fatal("missing stderr prose")
			}
			if tc.code == "" {
				if strings.Contains(stdout.String(), "mora.error") {
					t.Fatal(stdout.String())
				}
				return
			}
			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			var doc map[string]any
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &doc); err != nil {
				t.Fatalf("last stdout line is not mora.error: %q (%v)", stdout.String(), err)
			}
			if doc["schema"] != "mora.error" || doc["schema_version"] != float64(1) || doc["code"] != tc.code || doc["message"] == "" {
				t.Fatalf("document: %+v", doc)
			}
			if tc.name == "receipt_then_error" && !strings.Contains(stdout.String(), `"schema": "mora.sync.applecalendar"`) {
				t.Fatalf("missing preceding receipt: %s", stdout.String())
			}
			if tc.name == "coded" {
				assertK17Golden(t, "variants/mora.error.json", doc)
			}
			if tc.name == "unclassified" {
				if doc["message"] != "Operation failed; data may be incomplete. Retry the operation." || !strings.Contains(stderr.String(), "unknown-k17-command") {
					t.Fatalf("unclassified receipt must be safe while stderr keeps CLI guidance: %v / %s", doc, stderr.String())
				}
			} else if !strings.Contains(stderr.String(), doc["message"].(string)) {
				t.Fatalf("stderr lost prose: %s", stderr.String())
			}
		})
	}
}
