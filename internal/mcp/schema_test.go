package mcp

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestToolBuildsStrictSchema(t *testing.T) {
	got := Tool("search", "Find", Param{Name: "query", Type: "string", Desc: "Terms", Required: true}, Param{Name: "limit", Type: "integer", Desc: "Maximum"}, Param{Name: "scope", Type: "string", Desc: "Scope", Required: true})
	schema := got["inputSchema"].(map[string]any)
	if got["name"] != "search" || got["description"] != "Find" || schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("tool=%#v", got)
	}
	wantReq := []string{"query", "scope"}
	if !reflect.DeepEqual(schema["required"], wantReq) {
		t.Fatalf("required=%v", schema["required"])
	}
	props := schema["properties"].(map[string]any)
	if len(props) != 3 || props["limit"].(map[string]any)["type"] != "integer" {
		t.Fatalf("properties=%#v", props)
	}
}
func TestToolOmitsEmptyRequiredAndStableJSON(t *testing.T) {
	got := Tool("ping", "Ping")
	schema := got["inputSchema"].(map[string]any)
	if _, ok := schema["required"]; ok {
		t.Fatalf("required present=%#v", schema)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"description":"Ping","inputSchema":{"additionalProperties":false,"properties":{},"type":"object"},"name":"ping"}`
	if string(body) != want {
		t.Fatalf("json=%s", body)
	}
}
func TestArgumentCoercionPreservesDefaults(t *testing.T) {
	args := map[string]any{"s": "value", "empty": "", "n": float64(7.9), "int": 7, "yes": true, "false": "false", "bad": "nope"}
	if StringArg(args, "s", "d") != "value" || StringArg(args, "empty", "d") != "" || StringArg(args, "missing", "d") != "d" {
		t.Fatal("string coercion")
	}
	if IntArg(args, "n", 2) != 7 || IntArg(args, "int", 2) != 2 || IntArg(args, "missing", 2) != 2 {
		t.Fatal("integer coercion")
	}
	if !BoolArg(args, "yes", false) || BoolArg(args, "false", true) || !BoolArg(args, "bad", true) || BoolArg(args, "missing", false) {
		t.Fatal("boolean coercion")
	}
}
