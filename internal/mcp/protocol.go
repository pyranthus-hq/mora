// Package mcp owns transport-neutral MCP JSON-RPC framing and result envelopes.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Serve reads newline-delimited JSON-RPC, ignores malformed frames and notifications, and writes one response per request.
func Serve(ctx context.Context, out io.Writer, in io.Reader, maxRequestBytes int, handle Handler) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), maxRequestBytes)
	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue
		}
		resp := handle(ctx, req)
		b, _ := json.Marshal(resp)
		fmt.Fprintln(out, string(b))
	}
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
