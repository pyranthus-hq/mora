package integrations

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

func TestConnectWritesJSONIdempotentlyAndKeepsOtherKeys(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	writeFixture(t, path, `{"keep":{"nested":true},"mcpServers":{"other":{"command":"/o","args":[]}}}`)
	before := fixturePermissions(t, path, 0o600)
	bin := "/synthetic/Mora.app/Contents/MacOS/mora"
	seams := fsSeams(t, home)
	first, err := Connect(seams, Claude, bin)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Registered || !first.Changed || first.ConfigPath != path {
		t.Fatalf("first connect: %+v", first)
	}
	second, err := Connect(seams, Claude, bin)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Fatalf("second connect must be a no-op: %+v", second)
	}
	body, _ := os.ReadFile(path)
	text := string(body)
	for _, want := range []string{`"keep"`, `"nested": true`, `"other"`, `"type": "stdio"`, `"command": "` + bin + `"`, `"mcp"`, `"serve"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != before {
		t.Fatalf("mode changed to %o", info.Mode().Perm())
	}
}

func TestConnectCreatesMissingCursorConfig(t *testing.T) {
	home := t.TempDir()
	r, err := Connect(fsSeams(t, home), Cursor, "/x/mora")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(r.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"type"`) || !strings.Contains(string(body), `"command": "/x/mora"`) {
		t.Fatalf("cursor entry wrong:\n%s", body)
	}
}

func TestConnectRewritesOnlyTheCodexMoraTable(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	writeFixture(t, path, codexFixture)
	r, err := Connect(fsSeams(t, home), Codex, "/new/Mora.app/Contents/MacOS/mora")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Changed {
		t.Fatalf("expected a rewrite: %+v", r)
	}
	body, _ := os.ReadFile(path)
	text := string(body)
	if !strings.Contains(text, `command = "/new/Mora.app/Contents/MacOS/mora"`) || strings.Contains(text, "/old/path/mora") {
		t.Fatalf("command not rewritten:\n%s", text)
	}
	for _, keep := range []string{`model = "synthetic"`, `[mcp_servers.other]`, `command = "/usr/local/bin/other"`, `startup_timeout_sec = 120`, `[mcp_servers.mora.tools.search_memory]`, `approval_mode = "approve"`, `[features]`, `flag = true`} {
		if !strings.Contains(text, keep) {
			t.Fatalf("lost %q in\n%s", keep, text)
		}
	}
	if strings.Count(text, "[mcp_servers.mora]") != 1 {
		t.Fatalf("table duplicated:\n%s", text)
	}
}

func TestConnectAppendsCodexTableWhenAbsent(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	writeFixture(t, path, "model = \"synthetic\"\n")
	if _, err := Connect(fsSeams(t, home), Codex, "/x/mora"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	want := "model = \"synthetic\"\n\n[mcp_servers.mora]\ncommand = \"/x/mora\"\nargs = [\"mcp\", \"serve\"]\nstartup_timeout_sec = 120\n"
	if string(body) != want {
		t.Fatalf("got\n%s\nwant\n%s", body, want)
	}
}

func TestDisconnectRemovesEntriesIdempotently(t *testing.T) {
	home := t.TempDir()
	writeFixture(t, filepath.Join(home, ".codex", "config.toml"), codexFixture)
	writeFixture(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"mora":{"command":"/x"},"other":{"command":"/o"}},"keep":1}`)
	seams := fsSeams(t, home)
	r, err := Disconnect(seams, Codex)
	if err != nil || !r.Changed || r.Registered {
		t.Fatalf("codex disconnect: %+v %v", r, err)
	}
	body, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if strings.Contains(string(body), "mcp_servers.mora") || !strings.Contains(string(body), "[mcp_servers.other]") || !strings.Contains(string(body), "[features]") {
		t.Fatalf("codex disconnect wrong:\n%s", body)
	}
	r, err = Disconnect(seams, Claude)
	if err != nil || !r.Changed {
		t.Fatalf("claude disconnect: %+v %v", r, err)
	}
	body, _ = os.ReadFile(filepath.Join(home, ".claude.json"))
	if strings.Contains(string(body), `"mora"`) || !strings.Contains(string(body), `"other"`) || !strings.Contains(string(body), `"keep": 1`) {
		t.Fatalf("claude disconnect wrong:\n%s", body)
	}
	again, err := Disconnect(seams, Claude)
	if err != nil || again.Changed {
		t.Fatalf("second disconnect must be a no-op: %+v %v", again, err)
	}
	missing, err := Disconnect(seams, Cursor)
	if err != nil || missing.Changed {
		t.Fatalf("disconnect without a file must be a no-op: %+v %v", missing, err)
	}
}

func TestConnectPreservesEntryExtrasAndMode(t *testing.T) {
	for _, client := range []Client{Claude, Cursor, ClaudeDesktop} {
		t.Run(string(client), func(t *testing.T) {
			home := t.TempDir()
			path := ConfigPath(home, client)
			writeFixture(t, path, `{"large":9007199254740993,"mcpServers":{"mora":{"command":"/old","env":{"KEEP":"yes"},"extra":{"n":9007199254740993}}}}`)
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
			before := fixturePermissions(t, path, 0o640)
			s := fsSeams(t, home)
			if _, err := Connect(s, client, "/new"); err != nil {
				t.Fatal(err)
			}
			body, _ := os.ReadFile(path)
			info, _ := os.Stat(path)
			if strings.Count(string(body), "9007199254740993") != 2 || !strings.Contains(string(body), `"KEEP": "yes"`) || info.Mode().Perm() != before {
				t.Fatalf("lost extras/mode: %s %v", body, info.Mode())
			}
			r, err := Connect(s, client, "/new")
			if err != nil || r.Changed {
				t.Fatalf("not idempotent: %+v %v", r, err)
			}
		})
	}
}

func TestConnectRejectsMalformedJSONWithoutWriting(t *testing.T) {
	for _, body := range []string{`{`, `null`, `[]`, `{"mcpServers":null}`, `{"mcpServers":{"mora":null}}`} {
		t.Run(body, func(t *testing.T) {
			home := t.TempDir()
			path := ConfigPath(home, Claude)
			writeFixture(t, path, body)
			if _, err := Connect(fsSeams(t, home), Claude, "/x"); err == nil {
				t.Fatal("accepted malformed config")
			}
			after, _ := os.ReadFile(path)
			if string(after) != body {
				t.Fatal("modified malformed config")
			}
		})
	}
}

func TestConnectCodexIdempotentAndMultilineArgs(t *testing.T) {
	home := t.TempDir()
	path := ConfigPath(home, Codex)
	writeFixture(t, path, "[mcp_servers.mora]\ncommand = \"/old\"\nargs = [\n  \"old\",\n]\n\n[features]\nflag = true\n")
	s := fsSeams(t, home)
	if _, err := Connect(s, Codex, "/new"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), `"old"`) || !strings.Contains(string(body), "[features]\nflag = true") {
		t.Fatalf("bad rewrite: %s", body)
	}
	r, err := Connect(s, Codex, "/new")
	if err != nil || r.Changed {
		t.Fatalf("not idempotent: %+v %v", r, err)
	}
}

func TestConnectClaudePreservesForeignJSON(t *testing.T) {
	fixture := `{"numStartups":42,"theme":"dark","shell":"a && b < c > d","unicode":"\u263a","large":9007199254740993123,"float":1.2300e+10,"projects":{"/tmp/demo":{"allowedTools":[],"unknown":{"flag":true,"items":[null,2,"x"]}}},"oauthAccount":{"displayName":"Synthetic"},"mcpServers":{"other":{"command":"a && b < c > d","unknown":{"n":9007199254740993}},"mora":{"command":"/old"}}}`
	canonical := func(body []byte) []byte {
		t.Helper()
		var doc map[string]any
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			t.Fatal(err)
		}
		delete(doc["mcpServers"].(map[string]any), "mora")
		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(doc); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	home := t.TempDir()
	path := ConfigPath(home, Claude)
	writeFixture(t, path, fixture)
	seams := fsSeams(t, home)
	for _, op := range []string{"connect", "disconnect"} {
		var err error
		if op == "connect" {
			_, err = Connect(seams, Claude, "/new")
		} else {
			_, err = Disconnect(seams, Claude)
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(canonical([]byte(fixture)), canonical(body)) {
			t.Errorf("%s changed foreign JSON", op)
		}
		if !bytes.Contains(body, []byte("a && b < c > d")) {
			t.Errorf("%s HTML-escaped foreign strings", op)
		}
	}
}

func TestConnectSymlinkWritesThrough(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "dotfiles", "claude.json")
	writeFixture(t, target, `{"keep":true}`)
	path := ConfigPath(home, Claude)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	seams := fsSeams(t, home)
	for _, op := range []string{"connect", "disconnect"} {
		var err error
		if op == "connect" {
			_, err = Connect(seams, Claude, "/new")
		} else {
			_, err = Disconnect(seams, Claude)
		}
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("config symlink replaced")
		}
		body, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		e, err := readJSONEntry(body)
		if err != nil {
			t.Fatal(err)
		}
		if (e != nil) != (op == "connect") {
			t.Fatalf("%s did not write target", op)
		}
	}
}

func TestConnectCodexHeaderVariants(t *testing.T) {
	for _, header := range []string{`[mcp_servers.mora] # bundled`, `[ mcp_servers.mora ]`, `[mcp_servers."mora"]`, `[ mcp_servers . "mora" ] # bundled`} {
		t.Run(header, func(t *testing.T) {
			home := t.TempDir()
			path := ConfigPath(home, Codex)
			fixture := header + "\ncommand = \"/old#path\" # bundled\nargs = [\"mcp\", \"serve\"]\n"
			writeFixture(t, path, fixture)
			seams := fsSeams(t, home)
			rows, err := List(seams, "/old#path")
			if err != nil {
				t.Fatal(err)
			}
			if !rows[1].Registered || !rows[1].MatchesBinary {
				t.Errorf("commented entry unreadable: %+v", rows[1])
			}
			if _, err := Connect(seams, Codex, "/new#path"); err != nil {
				t.Fatal(err)
			}
			body, _ := os.ReadFile(path)
			want := strings.Replace(fixture, `"/old#path"`, `"/new#path"`, 1)
			if string(body) != want {
				t.Fatalf("not replaced in place: %q", body)
			}
		})
	}
}

func TestConnectDisconnectCodexInlineRefused(t *testing.T) {
	for _, fixture := range []string{
		`mcp_servers.mora = { command = "/old" }`,
		`mcp_servers."mora".command = "/old"`,
		"[mcp_servers]\nmora = { command = \"/old\" }\n",
		"[ mcp_servers ] # bundled\nmora.command = \"/old\"\n",
	} {
		t.Run(fixture, func(t *testing.T) {
			home := t.TempDir()
			path := ConfigPath(home, Codex)
			writeFixture(t, path, fixture)
			seams := fsSeams(t, home)
			rows, err := List(seams, "/new")
			if err != nil {
				t.Fatal(err)
			}
			if !rows[1].Registered || rows[1].Hook != "unknown" {
				t.Errorf("unsafe inline status: %+v", rows[1])
			}
			for _, op := range []string{"connect", "disconnect"} {
				if op == "connect" {
					_, err = Connect(seams, Codex, "/new")
				} else {
					_, err = Disconnect(seams, Codex)
				}
				if err == nil || err.Error() != "codex config declares mcp_servers.mora inline; edit ~/.codex/config.toml by hand" {
					t.Errorf("%s error = %v", op, err)
				}
				body, _ := os.ReadFile(path)
				if string(body) != fixture {
					t.Fatalf("%s changed inline config", op)
				}
			}
		})
	}
}

func TestDisconnectCodexLineEndingsAndSubtables(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n"} {
		for _, trailing := range []bool{false, true} {
			fixture := strings.TrimSuffix(codexFixture, "\n")
			// Non-adjacent subtable at EOF must also disappear.
			fixture += "\n[mcp_servers.mora.tools.other]\nenabled = true"
			if trailing {
				fixture += "\n"
			}
			fixture = strings.ReplaceAll(fixture, "\n", ending)
			home := t.TempDir()
			path := ConfigPath(home, Codex)
			writeFixture(t, path, fixture)
			seams := fsSeams(t, home)
			if _, err := Connect(seams, Codex, "/new"); err != nil {
				t.Fatal(err)
			}
			body, _ := os.ReadFile(path)
			if ending == "\r\n" && strings.Contains(strings.ReplaceAll(string(body), "\r\n", ""), "\n") {
				t.Error("connect introduced bare LF")
			}
			if _, err := Disconnect(seams, Codex); err != nil {
				t.Fatal(err)
			}
			body, _ = os.ReadFile(path)
			if strings.Contains(string(body), "mcp_servers.mora") {
				t.Error("disconnect retained Mora subtable")
			}
			if strings.HasSuffix(string(body), ending) != trailing {
				t.Error("disconnect changed trailing newline state")
			}
			if ending == "\r\n" && strings.Contains(strings.ReplaceAll(string(body), "\r\n", ""), "\n") {
				t.Error("disconnect introduced bare LF")
			}
			rows, err := List(seams, "/new")
			if err != nil {
				t.Fatal(err)
			}
			if rows[1].Registered {
				t.Error("still registered after disconnect")
			}
		}
	}
}

func TestIntegrationsRejectEmptyHomeBeforeIO(t *testing.T) {
	seams := Seams{}
	seams.Stat = func(string) (os.FileInfo, error) { t.Fatal("stat with empty home"); return nil, nil }
	seams.ReadFile = func(string) ([]byte, error) { t.Fatal("read with empty home"); return nil, nil }
	if _, err := List(seams, "/new"); err == nil {
		t.Error("List accepted empty home")
	}
	if _, err := Connect(seams, Claude, "/new"); err == nil {
		t.Error("Connect accepted empty home")
	}
	if _, err := Disconnect(seams, Claude); err == nil {
		t.Error("Disconnect accepted empty home")
	}
}

func TestConnectKeepsEntryInternalsUnescaped(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".cursor", "mcp.json")
	writeFixture(t, path, `{"mcpServers":{"mora":{"command":"/old","args":[],"env":{"FLAG":"p && q <r>"}}}}`)
	if _, err := Connect(fsSeams(t, home), Cursor, "/new/mora"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"p && q <r>"`) {
		t.Fatalf("mora entry internals were escaped:\n%s", body)
	}
}

// POSIX hosts must honor the requested bits; Windows reports its native file
// attributes instead. On every host the mutation must preserve observed mode.
func fixturePermissions(t *testing.T, path string, requested os.FileMode) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != requested {
		t.Fatalf("fixture mode=%o want=%o", info.Mode().Perm(), requested)
	}
	return info.Mode().Perm()
}
