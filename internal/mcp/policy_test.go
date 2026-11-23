package mcp

import (
	"strings"
	"testing"
)

func TestNormalizeWritePolicy(t *testing.T) {
	// An unset policy resolves to tentative: low-risk agent writes save at once
	// with a label, and the rest wait for the owner.
	if DefaultWritePolicy != WritePolicyTentative {
		t.Fatalf("DefaultWritePolicy=%q, want tentative", DefaultWritePolicy)
	}
	if got := NormalizeWritePolicy(""); got != WritePolicyTentative {
		t.Fatalf("empty=%q, want tentative", got)
	}
	for _, p := range []string{WritePolicyOpen, WritePolicyPropose, WritePolicyReadonly, WritePolicyTentative, "future"} {
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
	tentative := InstructionsFor(WritePolicyTentative)
	if strings.Contains(tentative, OpenWriteInstruction) || !strings.Contains(tentative, "saves at once as tentative") || !strings.Contains(tentative, "delete_memory is unavailable") {
		t.Fatalf("tentative=%q", tentative)
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
	cases := []struct{ policy, tool, action, errContains string }{{WritePolicyOpen, "write_memory", ActionExecute, ""}, {WritePolicyOpen, "delete_memory", ActionExecute, ""}, {WritePolicyOpen, "submit_resource", ActionExecute, ""}, {WritePolicyPropose, "write_memory", ActionPropose, ""}, {WritePolicyPropose, "submit_resource", ActionPropose, ""}, {WritePolicyPropose, "delete_memory", ActionRefuse, "never stages destructive deletes"}, {WritePolicyTentative, "write_memory", ActionTentative, ""}, {WritePolicyTentative, "submit_resource", ActionTentative, ""}, {WritePolicyTentative, "delete_memory", ActionRefuse, "never runs destructive deletes"}, {WritePolicyReadonly, "write_memory", ActionRefuse, "mcp_write_policy=readonly"}, {WritePolicyReadonly, "submit_resource", ActionRefuse, "mcp_write_policy=readonly"}, {WritePolicyReadonly, "delete_memory", ActionRefuse, "mcp_write_policy=readonly"}, {WritePolicyReadonly, "search_memory", ActionExecute, ""}, {"unknown", "write_memory", ActionExecute, ""}}
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

// Every policy tells the agent what an owner-confirmed row means.
func TestInstructionsExplainOwnerConfirmedRows(t *testing.T) {
	const want = `A row with "trust":"owner-confirmed" is an agent-written note whose exact text the owner confirmed in their own terminal`
	for _, p := range []string{WritePolicyOpen, WritePolicyPropose, WritePolicyTentative, WritePolicyReadonly} {
		if got := InstructionsFor(p); !strings.Contains(got, want) || !strings.Contains(got, "the confirmed one wins") {
			t.Errorf("%s instructions do not explain owner-confirmed rows", p)
		}
	}
}
