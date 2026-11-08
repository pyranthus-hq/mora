package rules

import "testing"

func TestParsePreTool(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		ok      bool
		harness string
		want    Action
	}{
		{
			name:    "claude main",
			raw:     `{"session_id":"s1","transcript_path":"/home/u/.claude/projects/x/s1.jsonl","cwd":"/tmp","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"toolu_1","tool_input":{"command":"cd /tmp && ls","description":"list"}}`,
			ok:      true,
			harness: HarnessClaude,
			want:    Action{SessionID: "s1", ToolUseID: "toolu_1", Command: "cd /tmp && ls"},
		},
		{
			name:    "claude subagent",
			raw:     `{"session_id":"s1","agent_id":"a7","transcript_path":"/home/u/.claude/projects/x/s1/subagents/agent-a7.jsonl","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"toolu_2","tool_input":{"command":"ls"}}`,
			ok:      true,
			harness: HarnessClaude,
			want:    Action{SessionID: "s1", AgentID: "a7", ToolUseID: "toolu_2", Command: "ls"},
		},
		{
			name:    "codex",
			raw:     `{"session_id":"019a-c","turn_id":"t9","transcript_path":null,"cwd":"/w","hook_event_name":"PreToolUse","model":"gpt","tool_name":"Bash","tool_use_id":"call_1","tool_input":{"command":"cd /w && make"}}`,
			ok:      true,
			harness: HarnessCodex,
			want:    Action{SessionID: "019a-c", ToolUseID: "call_1", Command: "cd /w && make"},
		},
		{
			name: "grok camelCase",
			raw:  `{"sessionId":"g1","toolName":"Bash","toolInput":{"command":"cd /tmp && ls"},"tool_name":"Bash","tool_input":{"command":"cd /tmp && ls"}}`,
		},
		{
			name:    "fallback to transcript for subagent and session",
			raw:     `{"transcript_path":"/home/u/.claude/projects/x/sess-9/subagents/agent-abc.jsonl","tool_name":"Bash","tool_input":{"command":"ls"}}`,
			ok:      true,
			harness: HarnessClaude,
			want:    Action{SessionID: "sess-9", AgentID: "abc", Command: "ls"},
		},
		{
			name:    "fallback to transcript stem for session",
			raw:     `{"transcript_path":"/home/u/.claude/projects/x/sess-1.jsonl","tool_name":"Bash","tool_input":{"command":"ls"}}`,
			ok:      true,
			harness: HarnessClaude,
			want:    Action{SessionID: "sess-1", Command: "ls"},
		},
		{
			name:    "sessionId fallback",
			raw:     `{"sessionId":"camel","tool_name":"Bash","tool_input":{"command":"ls"}}`,
			ok:      true,
			harness: HarnessClaude,
			want:    Action{SessionID: "camel", Command: "ls"},
		},
		{
			name:    "explicit agent id wins over the path",
			raw:     `{"session_id":"s","agent_id":"real","transcript_path":"/p/s/subagents/agent-other.jsonl","tool_name":"Bash","tool_input":{"command":"ls"}}`,
			ok:      true,
			harness: HarnessClaude,
			want:    Action{SessionID: "s", AgentID: "real", Command: "ls"},
		},
		{
			name:    "non-string tool_use_id is dropped",
			raw:     `{"session_id":"s1","tool_name":"Bash","tool_use_id":42,"tool_input":{"command":"ls"}}`,
			ok:      true,
			harness: HarnessClaude,
			want:    Action{SessionID: "s1", Command: "ls"},
		},
		{name: "non-string command", raw: `{"session_id":"s1","tool_name":"Bash","tool_input":{"command":["ls"]}}`},
		{name: "empty command", raw: `{"session_id":"s1","tool_name":"Bash","tool_input":{"command":""}}`},
		{name: "no tool_input", raw: `{"session_id":"s1","tool_name":"Bash"}`},
		{name: "tool_input not an object", raw: `{"session_id":"s1","tool_name":"Bash","tool_input":"ls"}`},
		{name: "non-Bash tool", raw: `{"session_id":"s1","tool_name":"Edit","tool_input":{"command":"ls"}}`},
		{name: "other event", raw: `{"session_id":"s1","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`},
		{name: "garbage", raw: `not json`},
		{name: "empty object", raw: `{}`},
		{name: "array", raw: `[]`},
		{name: "null", raw: `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, harness, ok := ParsePreTool([]byte(tc.raw))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if harness != tc.harness || a != tc.want {
				t.Fatalf("got %+v %s, want %+v %s", a, harness, tc.want, tc.harness)
			}
		})
	}
}
