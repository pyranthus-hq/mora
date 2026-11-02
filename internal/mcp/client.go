package mcp

import (
	"context"
	"encoding/json"
	"sync"
)

type clientNameKey struct{}

// WithClientName returns ctx carrying the MCP client's self-reported
// clientInfo.name for the session that issued the request.
func WithClientName(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, clientNameKey{}, name)
}

// ClientName returns the clientInfo.name carried by ctx, or "".
func ClientName(ctx context.Context) string {
	name, _ := ctx.Value(clientNameKey{}).(string)
	return name
}

// Session remembers the clientInfo.name a client sent in initialize. A stdio
// server process serves exactly one client, so one Session per Serve call is
// one client's identity.
type Session struct {
	mu   sync.Mutex
	name string
}

// Observe records clientInfo.name from an initialize request. Other methods
// and malformed params leave the recorded name unchanged.
func (s *Session) Observe(req Request) {
	if req.Method != "initialize" || len(req.Params) == 0 {
		return
	}
	var params struct {
		ClientInfo struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
	}
	if json.Unmarshal(req.Params, &params) != nil || params.ClientInfo.Name == "" {
		return
	}
	s.mu.Lock()
	s.name = params.ClientInfo.Name
	s.mu.Unlock()
}

// Context returns ctx carrying the session's client name, when known.
func (s *Session) Context(ctx context.Context) context.Context {
	s.mu.Lock()
	name := s.name
	s.mu.Unlock()
	return WithClientName(ctx, name)
}
