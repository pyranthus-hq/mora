package mcp

import (
	"context"
	"encoding/json"
)

type InitializeFunc func() any
type ListToolsFunc func() any
type CallToolFunc func(context.Context, string, map[string]any) any

// Dispatch routes a validated JSON-RPC request without owning application handlers.
func Dispatch(ctx context.Context, req Request, initialize InitializeFunc, listTools ListToolsFunc, callTool CallToolFunc) Response {
	resp := Response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = initialize()
	case "tools/list":
		resp.Result = listTools()
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &params)
		resp.Result = callTool(ctx, params.Name, params.Arguments)
	default:
		resp.Error = map[string]any{"code": -32601, "message": "method not found"}
	}
	return resp
}
