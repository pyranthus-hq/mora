package mora

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// TestMCPSearchIMessage proves IMSG-10 end-to-end with no provider special-casing:
// an iMessage MappedMemory written via the shared writeMappedMemory boundary and
// indexed is returned by the same MCP search path as any other memory.
func TestMCPSearchIMessage(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	mm := memory.MappedMemory{
		StableID:   "imessage_chat/iMessage;-;+14155551234",
		Scope:      "personal",
		Type:       "imessage",
		Title:      "Neil Patel",
		Body:       "## 2026-05-30\nNeil Patel: are we still on for the zephyrlaunch demo?\nMe: yes, 3pm",
		Source:     "iMessage;-;+14155551234",
		CreatedAt:  "2026-05-30T16:00:00Z",
		Provider:   "imessage",
		ProviderID: "iMessage;-;+14155551234",
		Tags:       []string{"imessage"},
	}
	mm.ContentHash = memory.ContentHash(mm.Title, mm.Body)
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatalf("writeMappedMemory: %v", err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}

	// The distinctive term only appears in this iMessage memory.
	text, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"zephyrlaunch"}}}`)
	if isErr {
		t.Fatalf("search_memory unexpectedly isError; text=%s", text)
	}
	var payload struct {
		Results []Memory `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("decode CallToolResult text: %v\n%s", err, text)
	}
	hits := payload.Results
	if len(hits) != 1 || hits[0].Type != "imessage" {
		t.Fatalf("expected 1 iMessage hit via MCP search, got: %s", text)
	}
	if hits[0].Title != "Neil Patel" || !strings.Contains(hits[0].Text, "3pm") {
		t.Fatalf("MCP search returned wrong memory: %+v", hits[0])
	}
}

// TestSetupSkipsUnconfiguredGoogle proves the guided-setup Google placeholder
// detect-and-skip (E-7, P-7/P-8): with placeholder/unconfigured Google creds and
// iMessage selected, the decision skips Google (no browser/loopback — the helper only
// calls google.IsConfigured), emits the EXACT skip copy, and KEEPS iMessage so setup
// still completes. The pure helper is the testable seam runSetupMenu delegates to.
func TestSetupSkipsUnconfiguredGoogle(t *testing.T) {
	// Force the embedded placeholder client (no BYO creds) regardless of the host env.
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")

	remaining, skipMsg, skipped := googleSetupStep([]string{"gmail", "calendar", "imessage"})
	if !skipped {
		t.Fatal("expected Google to be skipped with placeholder creds")
	}
	want := "Skipping Google for now — set up creds later with `mora connectors enable gmail`. iMessage and filesystem need no creds."
	if skipMsg != want {
		t.Fatalf("skip message = %q, want %q", skipMsg, want)
	}
	// iMessage (and filesystem) survive — setup does not dead-end.
	if !containsType(remaining, "imessage") {
		t.Fatalf("iMessage must remain after the Google skip, got %v", remaining)
	}
	if containsType(remaining, "gmail") || containsType(remaining, "calendar") {
		t.Fatalf("google types must be removed, got %v", remaining)
	}

	// With no google types selected, the step is a no-op (nothing to skip).
	if _, _, s := googleSetupStep([]string{"imessage", "filesystem"}); s {
		t.Fatal("googleSetupStep should not skip when no google types are selected")
	}
}

// TestIMessageDispatchZeroNetwork asserts the iMessage connector path introduces no
// network dependency (IMSG-01): the internal/imessage package imports no net/http/rpc
// package. The mora dispatch arm only calls into that package, so a zero-network
// connector keeps the whole iMessage path zero-network.
func TestIMessageDispatchZeroNetwork(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "../imessage", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse internal/imessage: %v", err)
	}
	banned := map[string]bool{
		`"net"`: true, `"net/http"`: true, `"net/rpc"`: true, `"net/url"`: true,
		`"net/smtp"`: true, `"golang.org/x/net/http2"`: true,
	}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			for _, imp := range file.Imports {
				if banned[imp.Path.Value] {
					t.Errorf("%s imports a network package %s — iMessage must be zero-network (IMSG-01)", name, imp.Path.Value)
				}
			}
		}
	}
}
