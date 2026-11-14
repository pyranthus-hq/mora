package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

type elicitOutcome struct {
	yes bool
	err error
}

func elicitServer(t *testing.T, timeout time.Duration) (*twoWay, <-chan elicitOutcome) {
	t.Helper()
	outcomes := make(chan elicitOutcome, 1)
	s := startTwoWay(t, context.Background(), func(ctx context.Context, r Request) Response {
		if r.Method == "ask" || r.Method == "initialize" {
			yes, err := Elicit(ctx, "Proceed?", "confirm", "Approval", timeout)
			outcomes <- elicitOutcome{yes, err}
		}
		return Response{ID: r.ID, Result: "done"}
	})
	return s, outcomes
}

func elicitOutcomeRead(t *testing.T, ch <-chan elicitOutcome) elicitOutcome {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("elicitation deadlock")
		return elicitOutcome{}
	}
}

func checkElicitFrame(t *testing.T, f map[string]json.RawMessage) {
	t.Helper()
	if string(f["method"]) != `"elicitation/create"` {
		t.Fatalf("frame=%s", f)
	}
	var got any
	if err := json.Unmarshal(f["params"], &got); err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal([]byte(`{"message":"Proceed?","requestedSchema":{"type":"object","properties":{"confirm":{"type":"boolean","title":"Approval","default":false}},"required":["confirm"]}}`), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("params=%s", f["params"])
	}
}

func TestElicitCapabilities(t *testing.T) {
	// AC-001.1 AC-001.2 AC-001.3 AC-001.4 AC-001.5 AC-001.6
	// AC-002.2 AC-002.3
	for _, tc := range []struct {
		name      string
		caps      []string
		available []bool
	}{
		{"empty", []string{`{"elicitation":{}}`}, []bool{true}},
		{"form", []string{`{"elicitation":{"form":{}}}`}, []bool{true}},
		{"url", []string{`{"elicitation":{"url":{}}}`}, []bool{false}},
		{"both", []string{`{"elicitation":{"form":{},"url":{}}}`}, []bool{true}},
		{"absent", []string{`{}`}, []bool{false}},
		{"replace", []string{`{"elicitation":{}}`, `{}`, `{"elicitation":{"form":{}}}`}, []bool{true, false, true}},
		{"null", []string{`{"elicitation":null}`}, []bool{false}},
		{"array", []string{`{"elicitation":[]}`}, []bool{false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, outcomes := elicitServer(t, time.Second)
			for i, caps := range tc.caps {
				s.send(map[string]any{"id": i + 1, "method": "initialize", "params": json.RawMessage(`{"capabilities":` + caps + `}`)})
				if tc.available[i] {
					f := s.read()
					checkElicitFrame(t, f)
					s.send(result(f["id"], map[string]any{"action": "accept", "content": map[string]bool{"confirm": true}}))
				}
				got := elicitOutcomeRead(t, outcomes)
				if tc.available[i] {
					if !got.yes || got.err != nil {
						t.Fatalf("outcome=%+v", got)
					}
				} else if got.yes || !errors.Is(got.err, ErrElicitationUnavailable) {
					t.Fatalf("outcome=%+v", got)
				}
				if f := s.read(); string(f["id"]) != string(mustElicitJSON(t, i+1)) || len(f["method"]) != 0 {
					t.Fatalf("extra request: %s", f)
				}
			}
			s.finish(nil)
			select {
			case f := <-s.frames:
				t.Fatalf("extra frame=%s", f)
			default:
			}
		})
	}
}

func mustElicitJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestElicitPlainContext(t *testing.T) {
	// AC-002.1
	// Even with an active capable session, a plain context cannot write a frame.
	s := startTwoWay(t, context.Background(), func(ctx context.Context, r Request) Response { return Response{ID: r.ID, Result: "done"} })
	s.send(map[string]any{"id": 1, "method": "initialize", "params": json.RawMessage(`{"capabilities":{"elicitation":{}}}`)})
	s.read()
	yes, err := Elicit(context.Background(), "x", "x", "x", time.Second)
	if yes || !errors.Is(err, ErrElicitationUnavailable) {
		t.Fatalf("yes=%v err=%v", yes, err)
	}
	s.finish(nil)
	select {
	case f := <-s.frames:
		t.Fatalf("unexpected frame=%s", f)
	default:
	}
}

func TestElicitReplies(t *testing.T) {
	// AC-003.1 AC-003.2 AC-003.3 AC-003.4 AC-003.5
	for _, raw := range []string{
		`{"action":"accept","content":{"confirm":true}}`,
		`{"action":"decline","content":{"confirm":true}}`, `{"action":"cancel","content":{"confirm":true}}`,
		`{"action":"accept","content":{"confirm":false}}`, `{"action":"accept","content":{}}`, `{"action":"accept"}`,
		`{"action":"accept","content":{"confirm":"true"}}`, `{"action":"accept","content":{"confirm":1}}`, `{"action":"accept","content":{"confirm":null}}`,
		`{"action":"Accept","content":{"confirm":true}}`, `{"action":"ACCEPT","content":{"confirm":true}}`,
		`null`, `[]`, `"accept"`, `true`, `1`, `{"action":true}`, `{"action":"accept","content":[]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			s, outcomes := elicitServer(t, time.Second)
			s.send(map[string]any{"id": 1, "method": "initialize", "params": json.RawMessage(`{"capabilities":{"elicitation":{}}}`)})
			f := s.read()
			checkElicitFrame(t, f)
			s.send(result(f["id"], json.RawMessage(raw)))
			got := elicitOutcomeRead(t, outcomes)
			if got.err != nil || got.yes != (raw == `{"action":"accept","content":{"confirm":true}}`) {
				t.Fatalf("outcome=%+v", got)
			}
			if f := s.read(); len(f["method"]) != 0 {
				t.Fatalf("extra request=%s", f)
			}
			s.finish(nil)
		})
	}
}

func TestElicitTransportErrors(t *testing.T) {
	// AC-003.6 AC-003.7 AC-003.8
	for _, ending := range []string{"error", "timeout", "eof"} {
		t.Run(ending, func(t *testing.T) {
			timeout := time.Second
			if ending == "timeout" {
				timeout = 30 * time.Millisecond
			}
			s, outcomes := elicitServer(t, timeout)
			s.send(map[string]any{"id": 1, "method": "initialize", "params": json.RawMessage(`{"capabilities":{"elicitation":{}}}`)})
			f := s.read()
			checkElicitFrame(t, f)
			switch ending {
			case "error":
				s.send(map[string]any{"id": f["id"], "error": map[string]any{"code": -1, "message": "failed"}})
			case "eof":
				if err := s.in.Close(); err != nil {
					t.Fatal(err)
				}
			case "timeout":
				if f := s.read(); string(f["method"]) != `"notifications/cancelled"` {
					t.Fatalf("cancel=%s", f)
				}
			}
			got := elicitOutcomeRead(t, outcomes)
			if got.yes || got.err == nil || errors.Is(got.err, ErrElicitationUnavailable) {
				t.Fatalf("outcome=%+v", got)
			}
			s.read()
			s.finish(nil)
		})
	}
}

// ElicitConfirm names how a no came about; only accept+true is ElicitYes.
func TestElicitConfirmOutcomes(t *testing.T) {
	for raw, want := range map[string]ElicitOutcome{
		`{"action":"accept","content":{"confirm":true}}`:   ElicitYes,
		`{"action":"accept","content":{"confirm":false}}`:  ElicitNo,
		`{"action":"accept","content":{"confirm":"true"}}`: ElicitNo,
		`{"action":"accept"}`:                              ElicitNo,
		`{"action":"decline","content":{"confirm":true}}`:  ElicitDeclined,
		`{"action":"cancel","content":{"confirm":true}}`:   ElicitCancelled,
		`{"action":"Accept","content":{"confirm":true}}`:   ElicitCancelled,
		`{"action":true}`: ElicitInvalid,
		`null`:            ElicitInvalid,
	} {
		t.Run(raw, func(t *testing.T) {
			outcomes := make(chan ElicitOutcome, 1)
			s := startTwoWay(t, context.Background(), func(ctx context.Context, r Request) Response {
				if r.Method == "initialize" {
					if !ElicitAvailable(ctx) {
						t.Error("capability not seen")
					}
					got, err := ElicitConfirm(ctx, "Proceed?", "confirm", "Approval", time.Second)
					if err != nil {
						t.Error(err)
					}
					outcomes <- got
				}
				return Response{ID: r.ID, Result: "done"}
			})
			s.send(map[string]any{"id": 1, "method": "initialize", "params": json.RawMessage(`{"capabilities":{"elicitation":{}}}`)})
			f := s.read()
			checkElicitFrame(t, f)
			s.send(result(f["id"], json.RawMessage(raw)))
			if got := <-outcomes; got != want {
				t.Fatalf("outcome=%v want %v", got, want)
			}
			s.read()
			s.finish(nil)
		})
	}
}
