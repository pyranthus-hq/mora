package mcp

import (
	"strings"
	"testing"
)

func TestNormalizeWritePolicy(t *testing.T) {
	if got := NormalizeWritePolicy(""); got != WritePolicyOpen {
		t.Fatalf("empty=%q", got)
	}
	for _, p := range []string{WritePolicyOpen, WritePolicyPropose, WritePolicyReadonly, "future"} {
		if got := NormalizeWritePolicy(p); got != p {
			t.Errorf("%q=%q", p, got)
		}
	}
}
func TestInstructionsAgreeWithPolicy(t *testing.T) {
	open := InstructionsFor(WritePolicyOpen)
	if open != OpenInstructions || strings.Count(open, OpenWriteInstruction) != 1 {
		t.Fatal("open instructions changed")
	}
	propose := InstructionsFor(WritePolicyPropose)
	if strings.Contains(propose, OpenWriteInstruction) || !strings.Contains(propose, "pending proposal queue") || !strings.Contains(propose, "delete_memory is unavailable") {
		t.Fatalf("propose=%q", propose)
	}
	readonly := InstructionsFor(WritePolicyReadonly)
	if strings.Contains(readonly, OpenWriteInstruction) || !strings.Contains(readonly, "read-only") || !strings.Contains(readonly, "both will refuse") {
		t.Fatalf("readonly=%q", readonly)
	}
	if got := InstructionsFor("unknown"); got != OpenInstructions {
		t.Fatal("unknown policy fallback changed")
	}
}
func TestMutationAction(t *testing.T) {
	cases := []struct{ policy, tool, action, errContains string }{{WritePolicyOpen, "write_memory", ActionExecute, ""}, {WritePolicyOpen, "delete_memory", ActionExecute, ""}, {WritePolicyPropose, "write_memory", ActionPropose, ""}, {WritePolicyPropose, "delete_memory", ActionRefuse, "never stages destructive deletes"}, {WritePolicyReadonly, "write_memory", ActionRefuse, "mcp_write_policy=readonly"}, {WritePolicyReadonly, "delete_memory", ActionRefuse, "mcp_write_policy=readonly"}, {WritePolicyReadonly, "search_memory", ActionExecute, ""}, {"unknown", "write_memory", ActionExecute, ""}}
	for _, tc := range cases {
		action, err := MutationAction(tc.policy, tc.tool)
		if action != tc.action {
			t.Errorf("(%s,%s) action=%q", tc.policy, tc.tool, action)
		}
		if tc.errContains == "" && err != nil {
			t.Errorf("(%s,%s) err=%v", tc.policy, tc.tool, err)
		}
		if tc.errContains != "" && (err == nil || !strings.Contains(err.Error(), tc.errContains)) {
			t.Errorf("(%s,%s) err=%v", tc.policy, tc.tool, err)
		}
	}
}
