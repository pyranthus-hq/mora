// Package mcp owns transport-neutral MCP JSON-RPC framing and result envelopes.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type Response struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

type Handler func(context.Context, Request) Response

// ErrNoClientChannel means there is no live stdio client.
var ErrNoClientChannel = errors.New("no MCP client channel")

var clientChannel struct {
	sync.Mutex
	active *clientTransport
}
var clientSequence atomic.Uint64

type clientReply struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}
type clientTransport struct {
	out     io.Writer
	writeMu sync.Mutex
	mu      sync.Mutex
	closed  bool
	waiters map[string]chan clientReply
	done    chan struct{}
}

func (c *clientTransport) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = fmt.Fprintln(c.out, string(b))
	return err
}

// CallClient sends a request to the active stdio client.
func CallClient(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	if session, ok := ctx.Value(elicitationSessionKey{}).(elicitationSession); ok {
		return session.client.call(ctx, method, params, timeout)
	}
	clientChannel.Lock()
	c := clientChannel.active
	clientChannel.Unlock()
	if c == nil {
		return nil, ErrNoClientChannel
	}
	return c.call(ctx, method, params, timeout)
}

func (c *clientTransport) call(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id := fmt.Sprintf("mora-%d", clientSequence.Add(1))
	reply := make(chan clientReply, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrNoClientChannel
	}
	c.waiters[id] = reply
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.waiters, id); c.mu.Unlock() }()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-reply:
		if len(r.Error) != 0 && string(r.Error) != "null" {
			return nil, fmt.Errorf("MCP client error: %s", r.Error)
		}
		return r.Result, nil
	case <-c.done:
		return nil, ErrNoClientChannel
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
		_ = c.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id}})
		return nil, context.DeadlineExceeded
	}
}

// Serve reads frames independently of the single ordered request handler.
func Serve(ctx context.Context, out io.Writer, in io.Reader, maxRequestBytes int, handle Handler) error {
	c := &clientTransport{out: out, waiters: make(map[string]chan clientReply), done: make(chan struct{})}
	clientChannel.Lock()
	clientChannel.active = c
	clientChannel.Unlock()
	defer func() {
		clientChannel.Lock()
		if clientChannel.active == c {
			clientChannel.active = nil
		}
		clientChannel.Unlock()
	}()
	requests := make(chan Request, 64)
	handled := make(chan struct{})
	go func() {
		defer close(handled)
		handlerCtx := ctx
		for req := range requests {
			if req.Method == "initialize" {
				handlerCtx = context.WithValue(ctx, elicitationSessionKey{}, elicitationSession{client: c, form: declaresForm(req.Params)})
			}
			_ = c.write(handle(handlerCtx, req))
		}
	}()
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), maxRequestBytes)
	for scanner.Scan() {
		var req Request
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		if req.Method == "" {
			if id, ok := req.ID.(string); ok {
				var reply clientReply
				if json.Unmarshal(scanner.Bytes(), &reply) == nil {
					c.mu.Lock()
					if waiter := c.waiters[id]; waiter != nil {
						delete(c.waiters, id)
						waiter <- reply
					}
					c.mu.Unlock()
				}
			}
			continue
		}
		c.mu.Lock()
		waiting := len(c.waiters) > 0
		c.mu.Unlock()
		if req.Method == "ping" && waiting {
			_ = c.write(Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
			continue
		}
		select {
		case requests <- req:
		default:
			_ = c.write(Response{JSONRPC: "2.0", ID: req.ID, Error: map[string]any{"code": -32000, "message": "server busy"}})
		}
	}
	c.mu.Lock()
	c.closed = true
	close(c.done)
	c.mu.Unlock()
	close(requests)
	<-handled
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("MCP request line exceeded the %d-byte cap: %w", maxRequestBytes, err)
		}
		return err
	}
	return nil
}

// CallToolResult creates the spec-compliant text envelope and mirrors object payloads into structuredContent.
func CallToolResult(v any, err error) map[string]any {
	if err != nil {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}
	}
	text, mErr := json.MarshalIndent(v, "", "  ")
	if mErr != nil {
		text = []byte(fmt.Sprintf("%v", v))
	}
	res := map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}, "isError": false}
	if len(text) > 0 && text[0] == '{' {
		res["structuredContent"] = v
	}
	return res
}
