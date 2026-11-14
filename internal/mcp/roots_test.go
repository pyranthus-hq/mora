package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func rootsTestServer(t *testing.T, ctx context.Context) *twoWay {
	t.Helper()
	return startTwoWay(t, ctx, func(ctx context.Context, req Request) Response {
		if req.Method == "discover" {
			roots, err := ListClientRoots(ctx)
			if err != nil {
				return Response{ID: req.ID, Result: err.Error()}
			}
			return Response{ID: req.ID, Result: roots}
		}
		return Response{ID: req.ID, Result: "initialized"}
	})
}

func initializeRootsTestServer(t *testing.T, s *twoWay, capabilities string) {
	t.Helper()
	s.send(map[string]any{
		"id": "init", "method": "initialize",
		"params": json.RawMessage(`{"capabilities":` + capabilities + `}`),
	})
	if f := s.read(); string(f["id"]) != `"init"` || len(f["method"]) != 0 {
		t.Fatalf("unexpected initialize frame: %s", f)
	}
	s.send(map[string]any{"method": "notifications/initialized"})
}

func requestRootsTestServer(t *testing.T, s *twoWay) map[string]json.RawMessage {
	t.Helper()
	s.send(request("roots", "discover"))
	f := s.read()
	if string(f["method"]) != `"roots/list"` || len(f["params"]) != 0 {
		t.Fatalf("unexpected roots request: %s", f)
	}
	return f
}

func TestListClientRootsUsesTheInitializedConnection(t *testing.T) {
	a := rootsTestServer(t, context.Background())
	initializeRootsTestServer(t, a, `{"roots":{"listChanged":true}}`)
	b := rootsTestServer(t, context.Background())
	initializeRootsTestServer(t, b, `{"roots":{}}`)
	if _, err := ListClientRoots(context.Background()); !errors.Is(err, ErrNoClientChannel) {
		t.Fatalf("a context outside stdio should not use an active connection: %v", err)
	}
	for _, tc := range []struct {
		server *twoWay
		roots  []Root
	}{
		{a, []Root{{URI: "file:///project%20one", Name: "Project One"}}},
		{b, []Root{{URI: "file:///C:/project-two"}}},
	} {
		call := requestRootsTestServer(t, tc.server)
		tc.server.send(result(call["id"], map[string]any{"roots": tc.roots}))
		var got []Root
		f := tc.server.read()
		if err := json.Unmarshal(f["result"], &got); err != nil || !reflect.DeepEqual(got, tc.roots) {
			t.Fatalf("roots result=%s, err=%v, want=%v", f, err, tc.roots)
		}
	}
	for _, caps := range []string{`{}`, `{"roots":null}`, `{"roots":true}`, `{"roots":[]}`} {
		initializeRootsTestServer(t, a, caps)
		a.send(request("roots", "discover"))
		if f := a.read(); string(f["id"]) != `"roots"` || string(f["result"]) != `"`+ErrRootsUnavailable.Error()+`"` {
			t.Fatalf("unexpected response after initialize %s: %s", caps, f)
		}
	}
	initializeRootsTestServer(t, a, `{"roots":{}}`)
	call := requestRootsTestServer(t, a)
	a.send(result(call["id"], map[string]any{"roots": []any{}}))
	if f := a.read(); string(f["result"]) != `[]` {
		t.Fatalf("empty roots list: %s", f)
	}
	a.finish(nil)
	b.finish(nil)
}

func TestListClientRootsWaitsForInitializedNotification(t *testing.T) {
	s := rootsTestServer(t, context.Background())
	s.send(map[string]any{
		"id": "init", "method": "initialize",
		"params": json.RawMessage(`{"capabilities":{"roots":{}}}`),
	})
	if f := s.read(); string(f["id"]) != `"init"` {
		t.Fatalf("unexpected initialize frame: %s", f)
	}
	// A strict client has received initialize's response but has not yet sent
	// notifications/initialized. The server must fail locally rather than send
	// the forbidden roots/list request.
	s.send(request("before", "discover"))
	if f := s.read(); string(f["id"]) != `"before"` || string(f["result"]) != `"`+ErrClientNotInitialized.Error()+`"` {
		t.Fatalf("roots request escaped before initialized notification: %s", f)
	}
	s.send(map[string]any{"method": "notifications/initialized"})
	call := requestRootsTestServer(t, s)
	s.send(result(call["id"], map[string]any{"roots": []any{}}))
	if f := s.read(); string(f["id"]) != `"roots"` || string(f["result"]) != `[]` {
		t.Fatalf("roots did not resume after initialized notification: %s", f)
	}
	s.finish(nil)
}

func TestListClientRootsRejectsInvalidAndOversizedReplies(t *testing.T) {
	s := rootsTestServer(t, context.Background())
	initializeRootsTestServer(t, s, `{"roots":{}}`)
	many := make([]Root, 65)
	for i := range many {
		many[i].URI = "file:///fixture"
	}
	for _, reply := range []any{
		nil, []any{}, map[string]any{}, map[string]any{"roots": nil},
		map[string]any{"roots": []any{nil}},
		map[string]any{"roots": []Root{{URI: "https://fixture.invalid"}}},
		map[string]any{"roots": []Root{{URI: "file:///%zz"}}},
		map[string]any{"roots": []Root{{URI: "file:///fixture", Name: strings.Repeat("a", 33<<10)}}},
		map[string]any{"roots": many},
	} {
		call := requestRootsTestServer(t, s)
		s.send(result(call["id"], reply))
		if f := s.read(); string(f["result"]) != `"`+ErrInvalidRootsReply.Error()+`"` {
			t.Fatalf("unexpected error frame: %s", f)
		}
	}
	call := requestRootsTestServer(t, s)
	s.send(map[string]any{"id": call["id"], "error": map[string]any{"code": -32603, "message": "file:///private/fixture"}})
	if f := s.read(); string(f["result"]) != `"`+ErrRootsRequestFailed.Error()+`"` {
		t.Fatalf("unexpected client error frame: %s", f)
	}
	s.finish(nil)
}

func TestListClientRootsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := rootsTestServer(t, ctx)
	initializeRootsTestServer(t, s, `{"roots":{}}`)
	call := requestRootsTestServer(t, s)
	cancel()
	if f := s.read(); string(f["result"]) != `"context canceled"` {
		t.Fatalf("unexpected cancellation frame: %s", f)
	}
	s.send(result(call["id"], map[string]any{"roots": []any{}}))
	s.send(request("cancelled", "discover"))
	if f := s.read(); string(f["id"]) != `"cancelled"` || string(f["result"]) != `"context canceled"` {
		t.Fatalf("a canceled context should not send a roots request: %s", f)
	}
	s.finish(nil)
}

func TestListClientRootsTimeout(t *testing.T) {
	s := rootsTestServer(t, context.Background())
	initializeRootsTestServer(t, s, `{"roots":{}}`)
	call := requestRootsTestServer(t, s)
	cancelled := s.read()
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(cancelled["params"], &params); err != nil || string(cancelled["method"]) != `"notifications/cancelled"` || string(params.RequestID) != string(call["id"]) {
		t.Fatalf("unexpected timeout notification: %s, err=%v", cancelled, err)
	}
	if f := s.read(); string(f["id"]) != `"roots"` || string(f["result"]) != `"context deadline exceeded"` {
		t.Fatalf("unexpected deadline response: %s", f)
	}
	s.send(result(call["id"], map[string]any{"roots": []any{}}))
	s.finish(nil)
}
