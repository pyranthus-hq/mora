package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type twoWay struct {
	t      *testing.T
	in     *io.PipeWriter
	frames chan map[string]json.RawMessage
	done   chan error
}

func startTwoWay(t *testing.T, ctx context.Context, h Handler) *twoWay {
	t.Helper()
	r, w := io.Pipe()
	out, ow := io.Pipe()
	s := &twoWay{t: t, in: w, frames: make(chan map[string]json.RawMessage, 256), done: make(chan error, 1)}
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			var f map[string]json.RawMessage
			if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
				t.Errorf("interleaved output: %q", scanner.Text())
				continue
			}
			s.frames <- f
		}
	}()
	go func() { s.done <- Serve(ctx, ow, r, 70<<10, h); ow.Close() }()
	t.Cleanup(func() { w.Close(); out.Close(); r.Close() })
	return s
}
func (s *twoWay) send(v any) {
	s.t.Helper()
	done := make(chan error, 1)
	go func() {
		b, e := json.Marshal(v)
		if e == nil {
			_, e = fmt.Fprintln(s.in, string(b))
		}
		done <- e
	}()
	select {
	case e := <-done:
		if e != nil {
			s.t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		s.t.Fatal("write deadlock")
	}
}
func (s *twoWay) read() map[string]json.RawMessage {
	s.t.Helper()
	select {
	case f := <-s.frames:
		return f
	case <-time.After(5 * time.Second):
		s.t.Fatal("read deadlock")
		return nil
	}
}
func (s *twoWay) finish(want error) {
	s.t.Helper()
	s.in.Close()
	select {
	case e := <-s.done:
		if !errors.Is(e, want) {
			s.t.Fatalf("Serve: %v, want %v", e, want)
		}
	case <-time.After(5 * time.Second):
		s.t.Fatal("Serve deadlock")
	}
}
func request(id any, method string) map[string]any    { return map[string]any{"id": id, "method": method} }
func result(id json.RawMessage, v any) map[string]any { return map[string]any{"id": id, "result": v} }

func TestTwoWayReplyAndQueue(t *testing.T) {
	// AC-001.1 AC-001.2 AC-001.3 AC-001.4 AC-001.5
	// AC-002.1 AC-002.2 AC-002.3 AC-002.5
	var ids []string
	s := startTwoWay(t, context.Background(), func(ctx context.Context, r Request) Response {
		if r.Method == "call" {
			for i := 0; i < 2; i++ {
				got, err := CallClient(ctx, "client/tool", map[string]int{"x": 1}, time.Second*4)
				if i == 0 && (err != nil || string(got) != `{"ok":true}`) {
					t.Errorf("reply=%s err=%v", got, err)
				}
				if i == 1 && err == nil {
					t.Error("client error ignored")
				}
			}
		}
		return Response{JSONRPC: "2.0", ID: r.ID, Result: r.Method}
	})
	s.send(request("first", "call"))
	first := s.read()
	var id string
	if err := json.Unmarshal(first["id"], &id); err != nil {
		t.Errorf("unmarshal: %v", err)
	}
	if !regexp.MustCompile(`^mora-[0-9]+$`).MatchString(id) {
		t.Fatalf("id=%q", id)
	}
	ids = append(ids, id)
	s.send(request("ping", "ping"))
	if f := s.read(); string(f["id"]) != `"ping"` || string(f["result"]) != "{}" {
		t.Fatalf("ping=%s", f)
	}
	for i := 0; i < 64; i++ {
		s.send(request(i, "queued"))
	}
	s.send(request("overflow", "queued"))
	f := s.read()
	if string(f["id"]) != `"overflow"` || string(f["error"]) != `{"code":-32000,"message":"server busy"}` {
		t.Fatalf("busy=%s", f)
	}
	s.send(result(json.RawMessage(`"unknown"`), nil))
	s.send(result(first["id"], map[string]bool{"ok": true}))
	second := s.read()
	if err := json.Unmarshal(second["id"], &id); err != nil {
		t.Errorf("unmarshal: %v", err)
	}
	if !regexp.MustCompile(`^mora-[0-9]+$`).MatchString(id) || id == ids[0] {
		t.Fatalf("second id=%q", id)
	}
	s.send(result(first["id"], "duplicate"))
	s.send(map[string]any{"id": second["id"], "error": map[string]any{"code": -1, "message": "failed"}})
	if f = s.read(); string(f["id"]) != `"first"` {
		t.Fatalf("first=%s", f)
	}
	for i := 0; i < 64; i++ {
		if f = s.read(); string(f["id"]) != fmt.Sprint(i) {
			t.Fatalf("order %d: %s", i, f)
		}
	}
	s.finish(nil)
	select {
	case f := <-s.frames:
		t.Fatalf("unexpected output: %s", f)
	default:
	}
}

func TestTwoWayTimeout(t *testing.T) {
	// AC-002.4
	s := startTwoWay(t, context.Background(), func(ctx context.Context, r Request) Response {
		_, err := CallClient(ctx, "silent", nil, 30*time.Millisecond)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("timeout=%v", err)
		}
		return Response{ID: r.ID, Result: "done"}
	})
	s.send(request(1, "call"))
	call := s.read()
	cancel := s.read()
	var p map[string]json.RawMessage
	if err := json.Unmarshal(cancel["params"], &p); err != nil {
		t.Errorf("unmarshal: %v", err)
	}
	if string(cancel["method"]) != `"notifications/cancelled"` || string(p["requestId"]) != string(call["id"]) {
		t.Fatalf("cancel=%s", cancel)
	}
	s.read()
	s.send(result(call["id"], "late"))
	s.finish(nil)
	select {
	case f := <-s.frames:
		t.Fatalf("late reply output: %s", f)
	default:
	}
}

func TestTwoWayNoServe(t *testing.T) {
	// AC-002.6
	if _, err := CallClient(context.Background(), "absent", nil, time.Hour); !errors.Is(err, ErrNoClientChannel) {
		t.Fatalf("error=%v", err)
	}
}

// twoWayCtxKey is a private context key for the shutdown test.
type twoWayCtxKey struct{}

func TestTwoWayShutdown(t *testing.T) {
	// AC-003.1 AC-003.2 AC-003.3 AC-003.4
	for _, ending := range []string{"eof", "read error", "oversized"} {
		t.Run(ending, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), twoWayCtxKey{}, "original")
			returned := make(chan struct{})
			release := make(chan struct{})
			s := startTwoWay(t, ctx, func(got context.Context, r Request) Response {
				if got != ctx {
					t.Error("handler context changed")
				}
				if r.Method == "call" {
					_, err := CallClient(got, "waiting", nil, time.Hour)
					if err == nil {
						t.Error("EOF did not fail call")
					}
					_, err = CallClient(got, "after", nil, time.Hour)
					if err == nil {
						t.Error("call after EOF succeeded")
					}
					close(returned)
					<-release
				}
				return Response{ID: r.ID, Result: r.Method}
			})
			s.send(request(1, "call"))
			s.read()
			s.send(request(2, "queued"))
			sentinel := errors.New("input failed")
			switch ending {
			case "eof":
				s.in.Close()
			case "read error":
				s.in.CloseWithError(sentinel)
			case "oversized":
				done := make(chan struct{})
				go func() { fmt.Fprintln(s.in, strings.Repeat("x", 80<<10)); close(done) }()
				// The scanner stops reading once the cap is exceeded.
				t.Cleanup(func() { s.in.Close() })
			}
			select {
			case <-returned:
			case <-time.After(5 * time.Second):
				t.Fatal("pending call did not fail")
			}
			select {
			case e := <-s.done:
				t.Fatalf("Serve returned before handler: %v", e)
			default:
			}
			close(release)
			for i := 1; i <= 2; i++ {
				if f := s.read(); string(f["id"]) != fmt.Sprint(i) {
					t.Fatalf("response=%s", f)
				}
			}
			select {
			case err := <-s.done:
				if ending == "eof" && err != nil {
					t.Fatal(err)
				}
				if ending == "read error" && !errors.Is(err, sentinel) {
					t.Fatalf("error=%v", err)
				}
				if ending == "oversized" && (err == nil || !strings.Contains(err.Error(), "MCP request line exceeded")) {
					t.Fatalf("error=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Serve deadlock")
			}
		})
	}
}

// AC-001.5
func TestTwoWayConcurrentOutput(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	c := &clientTransport{out: bytewiseWriter{w}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var writers sync.WaitGroup
		for i := 0; i < 64; i++ {
			writers.Add(1)
			go func(id int) {
				defer writers.Done()
				if err := c.write(Response{ID: id, Result: strings.Repeat("x", 100)}); err != nil {
					t.Errorf("write: %v", err)
				}
			}(i)
		}
		writers.Wait()
		w.Close()
	}()
	lines := make(chan []string, 1)
	go func() {
		var all []string
		s := bufio.NewScanner(r)
		for s.Scan() {
			all = append(all, s.Text())
		}
		lines <- all
	}()
	select {
	case all := <-lines:
		if len(all) != 64 {
			t.Fatalf("lines=%d", len(all))
		}
		seen := map[float64]bool{}
		for _, line := range all {
			var f Response
			if err := json.Unmarshal([]byte(line), &f); err != nil {
				t.Fatal(err)
			}
			seen[f.ID.(float64)] = true
		}
		if len(seen) != 64 {
			t.Fatalf("distinct frames=%d", len(seen))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("output deadlock")
	}
	<-done
}

type bytewiseWriter struct{ io.Writer }

func (w bytewiseWriter) Write(p []byte) (int, error) {
	for i := range p {
		if _, err := w.Writer.Write(p[i : i+1]); err != nil {
			return i, err
		}
		runtime.Gosched()
	}
	return len(p), nil
}
