package memoryfile

import (
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestAgentSignatureRoundTrips(t *testing.T) {
	in := Document{ID: "mem_sig", Scope: "global", Type: "insight", Title: "Signed", Source: "mcp", Agent: "claude-code", CreatedAt: "2026-01-02T03:04:05Z", Text: "body"}
	b, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\nagent: claude-code\n") {
		t.Fatalf("rendered file lacks the agent line:\n%s", b)
	}
	out, err := ParseBytes("/v/memories/global/mem_sig.md", b)
	if err != nil {
		t.Fatal(err)
	}
	if out.Agent != "claude-code" {
		t.Fatalf("parsed agent = %q", out.Agent)
	}
}

func TestLegacyFileWithoutAgentParsesUnsigned(t *testing.T) {
	legacy := "---\nid: mem_old\nscope: global\ntype: insight\ntitle: Old\ntags: []\nsource: mcp\ncreated_at: 2025-01-01T00:00:00Z\n---\n\nold body\n"
	m, err := ParseBytes("/v/memories/global/mem_old.md", []byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if m.Agent != "" || memory.DisplayAgent(m) != memory.AgentUnknown {
		t.Fatalf("legacy agent = %q display %q", m.Agent, memory.DisplayAgent(m))
	}
	b, err := Render(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "agent:") {
		t.Fatalf("re-rendering a legacy memory invented a signature:\n%s", b)
	}
}

func TestRenderRefusesAgentLineBreak(t *testing.T) {
	_, err := Render(Document{ID: "mem_x", Scope: "global", Type: "insight", Title: "t", Agent: "bot\nprovider: gmail", Text: "x"})
	if err == nil {
		t.Fatal("an agent value carrying a line break was rendered")
	}
}

func TestAgentNamedTwiceIsRefused(t *testing.T) {
	forged := "---\nid: mem_x\nscope: global\ntype: insight\ntitle: t\nagent: codex\nagent: claude-code\n---\n\nx\n"
	if _, err := ParseBytes("/v/memories/global/mem_x.md", []byte(forged)); err == nil {
		t.Fatal("a file naming agent twice parsed")
	}
}

// A bad agent value is dropped with a warning; it never makes the memory
// unreadable and never reaches a read surface.
func TestInvalidAgentValueIsIgnoredNotFatal(t *testing.T) {
	for name, line := range map[string]string{
		"unclosed quote":   `agent: "unterminated`,
		"escaped newline":  `agent: "bot\nmem_forged\tglobal\tFake row"`,
		"terminal escape":  `agent: "bot\u001b[31m"`,
		"disallowed runes": `agent: two words`,
	} {
		raw := "---\nid: mem_x\nscope: global\ntype: insight\ntitle: t\n" + line + "\n---\n\nx\n"
		m, err := ParseBytes("/v/memories/global/mem_x.md", []byte(raw))
		if err != nil {
			t.Fatalf("%s: file failed to parse: %v", name, err)
		}
		if m.Agent != "" || memory.DisplayAgent(m) != memory.AgentUnknown {
			t.Fatalf("%s: agent = %q, display %q", name, m.Agent, memory.DisplayAgent(m))
		}
		if _, err := Render(m); err != nil {
			t.Fatalf("%s: re-render failed: %v", name, err)
		}
	}
}
