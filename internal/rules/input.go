package rules

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

const (
	HarnessClaude = "claude"
	HarnessCodex  = "codex"
)

// ParsePreTool reads the PreToolUse hook stdin. ok is false when the input is
// not a usable Bash call; the hook then prints nothing and logs nothing.
//
// Grok also reads ~/.claude/settings.json but sends camelCase keys such as
// toolName. Warnings for Grok are out of scope, so any toolName key gives
// ok=false. Codex sends turn_id, which is how the harness is told apart.
func ParsePreTool(raw []byte) (a Action, harness string, ok bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return Action{}, "", false
	}
	if _, grok := m["toolName"]; grok {
		return Action{}, "", false
	}
	harness = HarnessClaude
	if _, codex := m["turn_id"]; codex {
		harness = HarnessCodex
	}
	if ev, present := m["hook_event_name"]; present && string(ev) != "null" {
		if str(ev) != "PreToolUse" {
			return Action{}, "", false
		}
	}
	if str(m["tool_name"]) != "Bash" {
		return Action{}, "", false
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(m["tool_input"], &input); err != nil {
		return Action{}, "", false
	}
	a.Command = str(input["command"])
	if a.Command == "" {
		return Action{}, "", false
	}
	a.SessionID = str(m["session_id"])
	if a.SessionID == "" {
		a.SessionID = str(m["sessionId"])
	}
	a.AgentID = str(m["agent_id"])
	a.ToolUseID = str(m["tool_use_id"])
	session, agent := transcriptIDs(str(m["transcript_path"]))
	if a.AgentID == "" {
		a.AgentID = agent
	}
	if a.SessionID == "" {
		a.SessionID = session
	}
	return a, harness, true
}

// str decodes a JSON string and returns "" for anything else.
func str(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// transcriptIDs is the steward_hook.py fallback. A subagent transcript is
// <project>/<session>/subagents/agent-<id>.jsonl; a main one is
// <project>/<session>.jsonl.
func transcriptIDs(path string) (session, agent string) {
	if path == "" {
		return "", ""
	}
	var parts []string
	for _, p := range strings.Split(filepath.ToSlash(path), "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "", ""
	}
	name := parts[len(parts)-1]
	stem := name
	if ext := filepath.Ext(name); ext != name {
		stem = strings.TrimSuffix(name, ext)
	}
	if strings.HasPrefix(name, "agent-") && strings.HasSuffix(name, ".jsonl") {
		agent = strings.TrimPrefix(stem, "agent-")
	}
	session = stem
	for i, p := range parts {
		if p == "subagents" {
			if i > 0 {
				session = parts[i-1]
			}
			break
		}
	}
	return session, agent
}
