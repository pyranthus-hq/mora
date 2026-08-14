package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestServeIgnoresMalformedAndNotifications(t *testing.T) {
	input := "not-json\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" + `{"jsonrpc":"2.0","id":7,"method":"ping"}` + "\n"
	var out bytes.Buffer
	calls := 0
	err := Serve(context.Background(), &out, strings.NewReader(input), 4<<20, func(_ context.Context, req Request) Response {
		calls++
		return Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"ok": true}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	var got Response
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &got); err != nil {
		t.Fatal(err)
	}
	if got.JSONRPC != "2.0" || got.ID.(float64) != 7 {
		t.Fatalf("response=%+v", got)
	}
}
func TestServePreservesOneLineFramingAndOrder(t *testing.T) {
	input := `{"id":"a","method":"one"}` + "\n" + `{"id":"b","method":"two"}` + "\n"
	var out bytes.Buffer
	err := Serve(context.Background(), &out, strings.NewReader(input), 4<<20, func(_ context.Context, req Request) Response {
		return Response{JSONRPC: "2.0", ID: req.ID, Result: req.Method}
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"id":"a"`) || !strings.Contains(lines[1], `"id":"b"`) {
		t.Fatalf("lines=%q", lines)
	}
}
func TestServeRejectsOversizedLineLoudly(t *testing.T) {
	var out bytes.Buffer
	err := Serve(context.Background(), &out, strings.NewReader(strings.Repeat("x", 70<<10)+"\n"), 65<<10, func(context.Context, Request) Response { t.Fatal("handler called"); return Response{} })
	if err == nil || !strings.Contains(err.Error(), "MCP request line exceeded the 66560-byte cap") {
		t.Fatalf("error=%v", err)
	}
}
func TestCallToolResultShapes(t *testing.T) {
	obj := map[string]any{"x": 1}
	got := CallToolResult(obj, nil)
	if got["isError"] != false || !strings.Contains(got["content"].([]map[string]any)[0]["text"].(string), `"x": 1`) || !reflectMap(got["structuredContent"], obj) {
		t.Fatalf("object=%#v", got)
	}
	arr := CallToolResult([]string{"x"}, nil)
	if _, ok := arr["structuredContent"]; ok {
		t.Fatalf("array structured=%#v", arr)
	}
	sentinel := errors.New("boom")
	bad := CallToolResult(nil, sentinel)
	if bad["isError"] != true || bad["content"].([]map[string]any)[0]["text"] != "boom" {
		t.Fatalf("error=%#v", bad)
	}
	fallback := CallToolResult(func() {}, nil)
	if text := fallback["content"].([]map[string]any)[0]["text"].(string); !strings.HasPrefix(text, "0x") {
		t.Fatalf("fallback=%#v", fallback)
	}
}
func reflectMap(got any, want map[string]any) bool {
	m, ok := got.(map[string]any)
	return ok && m["x"] == want["x"]
}
