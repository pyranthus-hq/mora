package memory

import (
	"strings"
	"testing"
)

func TestNormalizeAgentAcceptsLabelsAndRefusesForgery(t *testing.T) {
	for raw, want := range map[string]string{
		"claude-code": "claude-code", " Codex ": "codex", "test-bot_1.2": "test-bot_1.2", "": "",
	} {
		got, err := NormalizeAgent(raw)
		if err != nil || got != want {
			t.Fatalf("NormalizeAgent(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"bad agent", "x\nid: forged", "a:b", "a\u2028b", strings.Repeat("a", AgentMaxLen+1)} {
		if got, err := NormalizeAgent(raw); err == nil {
			t.Fatalf("NormalizeAgent(%q) = %q, want error", raw, got)
		}
	}
}

func TestAgentFromClientNameAliasesAndSanitizes(t *testing.T) {
	for name, want := range map[string]string{
		"claude-code":      "claude-code",
		"codex-mcp-client": "codex",
		"cursor-vscode":    "cursor",
		"claude-ai":        "claude-desktop",
		"Example Bot":      "example-bot",
		"  x\ny: z  ":      "x-y-z",
		"":                 "",
		"!!!":              "",
	} {
		if got := AgentFromClientName(name); got != want {
			t.Fatalf("AgentFromClientName(%q) = %q, want %q", name, got, want)
		}
		if got := AgentFromClientName(name); got != "" {
			if _, err := NormalizeAgent(got); err != nil {
				t.Fatalf("derived agent %q is not a valid signature: %v", got, err)
			}
		}
	}
}

func TestDisplayAgentTreatsUnsignedAuthoredAsUnknown(t *testing.T) {
	if got := DisplayAgent(Memory{ID: "mem_1", Agent: "codex"}); got != "codex" {
		t.Fatalf("signed = %q", got)
	}
	if got := DisplayAgent(Memory{ID: "mem_1"}); got != AgentUnknown {
		t.Fatalf("legacy authored = %q, want %q", got, AgentUnknown)
	}
	if got := DisplayAgent(Memory{ID: "gmail_thread/abc", Provider: "gmail"}); got != "" {
		t.Fatalf("connector record = %q, want unsigned", got)
	}
	got := WithProvenance(Memory{ID: "mem_legacy"})
	if got.Agent != AgentUnknown || got.Provenance != ProvenanceAuthored {
		t.Fatalf("WithProvenance(legacy) = agent %q provenance %q", got.Agent, got.Provenance)
	}
}

func TestReservedAgentLabels(t *testing.T) {
	for _, label := range []string{AgentUnknown, AgentCLI, AgentMora, AgentHTTP, AgentCompanion} {
		if !IsReservedAgent(label) {
			t.Errorf("%q is not reserved", label)
		}
	}
	for _, label := range []string{"claude-code", "codex", "cli-bot", ""} {
		if IsReservedAgent(label) {
			t.Errorf("%q is reserved", label)
		}
	}
}

func TestDisplayAgentIgnoresInvalidStoredLabelsAndConnectorRows(t *testing.T) {
	if got := DisplayAgent(Memory{ID: "mem_1", Agent: "bot\nmem_forged\tglobal\tFake row\x1b[31m"}); got != AgentUnknown {
		t.Fatalf("invalid stored label displayed as %q, want %q", got, AgentUnknown)
	}
	if got := DisplayAgent(Memory{ID: "mem_1", Agent: "Claude-Code"}); got != "claude-code" {
		t.Fatalf("mixed-case stored label displayed as %q", got)
	}
	email := Memory{ID: "gmail_thread/abc", Path: "/Users/a/vault/memories/sources/gmail/x.md"}
	if got := DisplayAgent(email); got != "" {
		t.Fatalf("connector row under a memories path displayed as %q, want unsigned", got)
	}
}
