package mcp

import (
	"reflect"
	"testing"
)

func TestInitializeResultExactHandshakeByPolicy(t *testing.T) {
	for _, policy := range []string{WritePolicyOpen, WritePolicyPropose, WritePolicyReadonly} {
		got := InitializeResult("v1.2.3", policy, true)
		want := map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]string{"name": "mora", "version": "v1.2.3"}, "capabilities": map[string]any{"tools": map[string]any{}}, "instructions": InstructionsFor(policy)}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s got=%#v", policy, got)
		}
	}
}
func TestInitializeResultFailsClosedWhenConfigUnavailable(t *testing.T) {
	for _, policy := range []string{WritePolicyOpen, WritePolicyPropose, WritePolicyReadonly} {
		got := InitializeResult("dev", policy, false)
		if got["instructions"] != ConfigUnavailableInstructions {
			t.Errorf("%s instructions=%q", policy, got["instructions"])
		}
		if got["capabilities"] == nil || got["serverInfo"] == nil {
			t.Errorf("%s incomplete=%#v", policy, got)
		}
	}
}
func TestInitializeProtocolConstants(t *testing.T) {
	if ProtocolVersion != "2024-11-05" || ServerName != "mora" {
		t.Fatalf("protocol=%q server=%q", ProtocolVersion, ServerName)
	}
}
