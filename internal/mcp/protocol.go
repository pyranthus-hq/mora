// Package mcp owns transport-neutral MCP JSON-RPC framing and result envelopes.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// EOFGrace is how long a running handler may continue after stdin closes before its context is cancelled.
var EOFGrace = 3 * time.Second

// Serve reads newline-delimited JSON-RPC, ignores malformed frames and notifications, and writes one response per request.
// Requests run one at a time. Stdin is read on its own goroutine so that EOF or a read error cancels the context
// passed to the running handler: a client that exits mid-request must not keep the handler (and its SQLite write lock) alive.
// Cancelling ctx itself makes Serve return nil once the running handler returns.
func Serve(ctx context.Context, out io.Writer, in io.Reader, maxRequestBytes int, handle Handler) error {
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lines := make(chan []byte)
	var scanErr error // written before close(lines); read only after lines is closed
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 64*1024), maxRequestBytes)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		scanErr = scanner.Err()
		// Let a request piped in before EOF (`echo req | mora mcp serve`) finish if it is quick.
		time.AfterFunc(EOFGrace, cancel)
	}()
	for {
		var line []byte
		select {
		case <-ctx.Done():
			return nil
		case l, ok := <-lines:
			if !ok {
				if errors.Is(scanErr, bufio.ErrTooLong) {
					return fmt.Errorf("MCP request line exceeded the %d-byte cap: %w", maxRequestBytes, scanErr)
				}
				return scanErr
			}
			line = l
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue
		}
		resp := handle(hctx, req)
		b, _ := json.Marshal(resp)
		fmt.Fprintln(out, string(b)) // the client may be gone; a failed write is not actionable
	}
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
