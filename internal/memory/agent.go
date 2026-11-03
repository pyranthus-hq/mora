package memory

import (
	"fmt"
	"strings"
)

// Agent signatures for writers that are not MCP clients.
const (
	// AgentUnknown is the read-time signature of an authored memory that has
	// no agent line, such as one written before signatures existed. It is
	// never persisted.
	AgentUnknown = "unknown"
	// AgentCLI signs memories written through `mora write` and `mora teach`.
	AgentCLI = "cli"
	// AgentMora signs memories Mora itself synthesizes, such as retention
	// compactions.
	AgentMora = "mora"
	// AgentHTTP signs loopback HTTP writes whose caller named no agent.
	AgentHTTP = "http"
	// AgentCompanion signs captures from a paired companion device.
	AgentCompanion = "companion"
)

// AgentMaxLen bounds a signature so it stays a label, not a payload.
const AgentMaxLen = 64

// reservedAgents are the labels Mora's own writers use. A caller that names
// its own agent may not claim one: "(by cli)" at the approval step reads as
// the owner, and "mora" as Mora itself. Every label stays self-reported.
var reservedAgents = map[string]bool{
	AgentUnknown:   true,
	AgentCLI:       true,
	AgentMora:      true,
	AgentHTTP:      true,
	AgentCompanion: true,
}

// IsReservedAgent reports whether a normalized label belongs to one of Mora's
// own writers.
func IsReservedAgent(agent string) bool { return reservedAgents[agent] }

// clientAliases maps the clientInfo.name values well-known MCP clients send
// in initialize to the short signature a reader recognizes. Any other name is
// kept, normalized.
var clientAliases = map[string]string{
	"claude-code":      "claude-code",
	"codex-mcp-client": "codex",
	"codex":            "codex",
	"cursor-vscode":    "cursor",
	"claude-ai":        "claude-desktop",
}

// NormalizeAgent validates an explicitly supplied signature. It lowercases and
// trims the value and accepts only [a-z0-9._-], so a signature can never carry
// a line break into frontmatter or impersonate another field.
func NormalizeAgent(raw string) (string, error) {
	agent := strings.ToLower(strings.TrimSpace(raw))
	if agent == "" {
		return "", nil
	}
	if len(agent) > AgentMaxLen {
		return "", fmt.Errorf("agent must be at most %d characters", AgentMaxLen)
	}
	for _, r := range agent {
		if !agentRune(r) {
			return "", fmt.Errorf("agent %q may contain only a-z, 0-9, '.', '_' and '-'", raw)
		}
	}
	return agent, nil
}

// AgentFromClientName derives a signature from an MCP clientInfo.name. Unlike
// NormalizeAgent it never fails: the client chose the name, not the user, so a
// disallowed rune becomes '-' and an unusable name yields "".
func AgentFromClientName(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	if alias, ok := clientAliases[lower]; ok {
		return alias
	}
	var b strings.Builder
	lastDash := false
	for _, r := range lower {
		if !agentRune(r) {
			r = '-'
		}
		if r == '-' && (lastDash || b.Len() == 0) {
			continue
		}
		lastDash = r == '-'
		b.WriteRune(r)
		if b.Len() >= AgentMaxLen {
			break
		}
	}
	return strings.TrimRight(b.String(), "-")
}

func agentRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
}

// DisplayAgent returns the signature a read surface shows: the persisted one,
// AgentUnknown for an authored memory written without one (or with a value
// that is not a valid label, such as a hand edit), and "" for records a
// connector or the filesystem produced. The connector id check comes first,
// as in DeriveProvenance, so an email never reads "by unknown".
func DisplayAgent(m Memory) string {
	if hasConnectorID(m) || !isAgentMemory(m) {
		return ""
	}
	if agent, err := NormalizeAgent(m.Agent); err == nil && agent != "" {
		return agent
	}
	return AgentUnknown
}
