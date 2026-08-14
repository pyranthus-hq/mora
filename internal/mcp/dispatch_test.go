package mcp

import (
	"context"
	"reflect"
	"testing"
)

type testContextKey struct{}

func TestDispatchRoutesOnlyRequestedMethod(t *testing.T) {
	ctx := context.WithValue(context.Background(), testContextKey{}, "ctx")
	tests := []struct {
		method string
		params string
		want   any
		calls  string
	}{{"initialize", "", map[string]any{"init": true}, "i"}, {"tools/list", "", map[string]any{"tools": true}, "l"}, {"tools/call", `{"name":"search_memory","arguments":{"query":"x"}}`, map[string]any{"called": "search_memory", "query": "x"}, "c"}}
	for _, tc := range tests {
		calls := ""
		resp := Dispatch(ctx, Request{ID: 7, Method: tc.method, Params: []byte(tc.params)}, func() any { calls += "i"; return map[string]any{"init": true} }, func() any { calls += "l"; return map[string]any{"tools": true} }, func(gotCtx context.Context, name string, args map[string]any) any {
			calls += "c"
			if gotCtx != ctx {
				t.Error("context changed")
			}
			return map[string]any{"called": name, "query": args["query"]}
		})
		if resp.JSONRPC != "2.0" || resp.ID != 7 || resp.Error != nil || !reflect.DeepEqual(resp.Result, tc.want) || calls != tc.calls {
			t.Errorf("%s resp=%+v calls=%q", tc.method, resp, calls)
		}
	}
}
func TestDispatchCallMalformedParamsPreservesZeroValueSemantics(t *testing.T) {
	called := false
	resp := Dispatch(context.Background(), Request{Method: "tools/call", Params: []byte("{")}, func() any { return nil }, func() any { return nil }, func(_ context.Context, name string, args map[string]any) any {
		called = true
		if name != "" || args != nil {
			t.Fatalf("call=(%q,%v)", name, args)
		}
		return "result"
	})
	if !called || resp.Result != "result" || resp.Error != nil {
		t.Fatalf("resp=%+v called=%v", resp, called)
	}
}
func TestDispatchUnknownMethod(t *testing.T) {
	called := false
	never := func() any { called = true; return nil }
	resp := Dispatch(context.Background(), Request{JSONRPC: "1.0", ID: "id", Method: "missing"}, never, never, func(context.Context, string, map[string]any) any { called = true; return nil })
	want := map[string]any{"code": -32601, "message": "method not found"}
	if resp.JSONRPC != "2.0" || resp.ID != "id" || resp.Result != nil || !reflect.DeepEqual(resp.Error, want) || called {
		t.Fatalf("resp=%+v called=%v", resp, called)
	}
}
