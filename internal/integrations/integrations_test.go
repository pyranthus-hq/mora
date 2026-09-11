package integrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const codexFixture = `model = "synthetic"

[mcp_servers.other]
command = "/usr/local/bin/other"
args = ["serve"]

[mcp_servers.mora]
command = "/old/path/mora"
args = ["mcp", "serve"]
startup_timeout_sec = 120

[mcp_servers.mora.tools.search_memory]
approval_mode = "approve"

[features]
flag = true
`

func fsSeams(t *testing.T, home string) Seams {
	t.Helper()
	s := DefaultSeams(home)
	s.HookStatus = func(string) (bool, bool) { return false, true }
	return s
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestListOnEmptyHomeDetectsNothing(t *testing.T) {
	home := t.TempDir()
	list, err := List(fsSeams(t, home), "/synthetic/Mora.app/Contents/MacOS/mora")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("want 4 rows, got %d", len(list))
	}
	for _, row := range list {
		if row.Detected || row.Registered || row.MatchesBinary {
			t.Fatalf("empty home must detect nothing: %+v", row)
		}
		if row.ConfigPath == "" {
			t.Fatalf("config_path must always be named: %+v", row)
		}
	}
	if list[0].Client != "claude" || list[3].Client != "claude-desktop" {
		t.Fatalf("order wrong: %+v", list)
	}
}

func TestListReadsJSONAndTOMLRegistrations(t *testing.T) {
	home := t.TempDir()
	bin := "/synthetic/Mora.app/Contents/MacOS/mora"
	writeFixture(t, filepath.Join(home, ".claude.json"), `{"other":1,"mcpServers":{"mora":{"type":"stdio","command":"`+bin+`","args":["mcp","serve"],"env":{}}}}`)
	writeFixture(t, filepath.Join(home, ".claude", "settings.json"), `{}`)
	writeFixture(t, filepath.Join(home, ".codex", "config.toml"), codexFixture)
	writeFixture(t, filepath.Join(home, ".cursor", "mcp.json"), `{"mcpServers":{}}`)
	seams := fsSeams(t, home)
	seams.HookStatus = func(string) (bool, bool) { return true, true }
	list, err := List(seams, bin)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Status{}
	for _, row := range list {
		by[row.Client] = row
	}
	if c := by["claude"]; !c.Detected || !c.Registered || !c.MatchesBinary || c.Hook != "installed" {
		t.Fatalf("claude: %+v", c)
	}
	if c := by["codex"]; !c.Detected || !c.Registered || c.MatchesBinary || c.Command != "/old/path/mora" || c.Hook != "not_supported" {
		t.Fatalf("codex: %+v", c)
	}
	if c := by["cursor"]; !c.Detected || c.Registered {
		t.Fatalf("cursor: %+v", c)
	}
	if c := by["claude-desktop"]; c.Detected || c.Registered {
		t.Fatalf("claude-desktop: %+v", c)
	}
	if err := os.MkdirAll(filepath.Join(home, "Library", "Application Support", "Claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	list, _ = List(seams, bin)
	if !list[3].Detected {
		t.Fatalf("claude-desktop must be detected from its config directory: %+v", list[3])
	}
}

func TestListNeverReturnsOtherKeys(t *testing.T) {
	home := t.TempDir()
	writeFixture(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"synthetic":"never-surfaced"},"mcpServers":{"mora":{"command":"/x/mora","args":["mcp","serve"]}}}`)
	list, err := List(fsSeams(t, home), "/x/mora")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range list {
		blob := strings.ToLower(row.Client + row.ConfigPath + row.Command + strings.Join(row.Args, " ") + row.Hook)
		if strings.Contains(blob, "never-surfaced") || strings.Contains(blob, "oauth") {
			t.Fatalf("a Status row carried foreign config content: %+v", row)
		}
	}
}
