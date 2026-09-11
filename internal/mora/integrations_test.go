package mora

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationsListJSONOnEmptyHome(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	stdout, _, err := runSplit(t, "integrations", "list", "--binary", "/synthetic/mora", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Schema  string `json:"schema"`
		Binary  string `json:"binary"`
		Clients []struct {
			Client     string `json:"client"`
			Registered bool   `json:"registered"`
		} `json:"clients"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if doc.Schema != "mora.integrations.list" || doc.Binary != "/synthetic/mora" || len(doc.Clients) != 4 {
		t.Fatalf("list wrong: %+v", doc)
	}
}

func TestIntegrationsConnectAndDisconnectCursor(t *testing.T) {
	withTempHomeSetenv(t)
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	home := cfg.HomeDir()
	run(t, "init")
	stdout, _, err := runSplit(t, "integrations", "connect", "--client", "cursor", "--binary", "/synthetic/mora", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal([]byte(stdout), &receipt); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if receipt["schema"] != "mora.integrations.connect" || receipt["registered"] != true || receipt["changed"] != true || receipt["hook"] != "not_supported" {
		t.Fatalf("connect receipt: %v", receipt)
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor", "mcp.json")); err != nil {
		t.Fatalf("cursor config not written: %v", err)
	}
	stdout, _, err = runSplit(t, "integrations", "disconnect", "--client", "cursor", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var gone map[string]any
	if err := json.Unmarshal([]byte(stdout), &gone); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if gone["schema"] != "mora.integrations.disconnect" || gone["registered"] != false || gone["changed"] != true {
		t.Fatalf("disconnect receipt: %v", gone)
	}
}

func TestIntegrationsConnectClaudeQuotesTheHookExecutable(t *testing.T) {
	withTempHomeSetenv(t)
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	home := cfg.HomeDir()
	run(t, "init")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "Applications", "Mora Dogfood.app", "Contents", "MacOS", "mora")
	stdout, _, err := runSplit(t, "integrations", "connect", "--client", "claude", "--binary", bin, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal([]byte(stdout), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt["hook"] != "installed" {
		t.Fatalf("claude connect must install the hooks: %v", receipt)
	}
	settings, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(settings, &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, group := range doc.Hooks["SessionStart"] {
		for _, h := range group.Hooks {
			if strings.HasPrefix(h.Command, "'"+bin+"' hook session-start") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("a path with a space must be single-quoted in the hook command: %+v", doc.Hooks["SessionStart"])
	}
}

func TestIntegrationsConnectRefusesRelativeBinaryAndUnknownClient(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	if _, err := runErr(t, "integrations", "connect", "--client", "cursor", "--binary", "mora", "--json"); err == nil {
		t.Fatal("relative --binary must fail")
	}
	if _, err := runErr(t, "integrations", "connect", "--client", "vim", "--binary", "/x/mora", "--json"); err == nil {
		t.Fatal("unknown client must fail")
	}
}

func TestIntegrationsClaudeListAndDisconnectHooks(t *testing.T) {
	withTempHomeSetenv(t)
	run(t, "integrations", "connect", "--client", "claude", "--binary", "/synthetic/mora", "--json")
	stdout, _, err := runSplit(t, "integrations", "list", "--binary", "/synthetic/mora", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Clients []struct {
			Hook string `json:"hook"`
		} `json:"clients"`
	}
	if err := json.Unmarshal([]byte(stdout), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Clients[0].Hook != "installed" {
		t.Fatalf("unobserved hooks: %s", stdout)
	}
	stdout, _, err = runSplit(t, "integrations", "disconnect", "--client", "claude", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"hook": "not_installed"`) {
		t.Fatal(stdout)
	}
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(cfg.HomeDir(), ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "#mora-managed") {
		t.Fatalf("hooks retained: %s", body)
	}
}

func TestIntegrationsInvalidArgumentsDoNotWrite(t *testing.T) {
	withTempHomeSetenv(t)
	for _, args := range [][]string{
		{"list", "--binary", "relative", "--json"},
		{"connect", "--client", "cursor", "--json"},
		{"connect", "--client", "cursor", "--binary", "/x", "unexpected", "--json"},
		{"disconnect", "--client", "vim", "--json"},
	} {
		stdout, _, err := runSplit(t, append([]string{"integrations"}, args...)...)
		if err == nil || stdout != "" {
			t.Fatalf("args %v: stdout=%q err=%v", args, stdout, err)
		}
	}
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.HomeDir(), ".cursor", "mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected config: %v", err)
	}
}
