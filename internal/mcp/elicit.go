package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrElicitationUnavailable means this session has not declared form elicitation.
var ErrElicitationUnavailable = errors.New("MCP form elicitation unavailable")

type elicitationSessionKey struct{}
type elicitationSession struct {
	client *clientTransport
	form   bool
}

func declaresForm(params json.RawMessage) bool {
	var p struct {
		Capabilities struct {
			Elicitation map[string]json.RawMessage `json:"elicitation"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Capabilities.Elicitation == nil {
		return false
	}
	_, form := p.Capabilities.Elicitation["form"]
	_, url := p.Capabilities.Elicitation["url"]
	return form || !url
}

// Elicit asks for a boolean confirmation and accepts only an explicit true.
func Elicit(ctx context.Context, message, field, title string, timeout time.Duration) (bool, error) {
	session, ok := ctx.Value(elicitationSessionKey{}).(elicitationSession)
	if !ok || !session.form {
		return false, ErrElicitationUnavailable
	}
	raw, err := CallClient(ctx, "elicitation/create", map[string]any{
		"message":         message,
		"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{field: map[string]any{"type": "boolean", "title": title, "default": false}}, "required": []string{field}},
	}, timeout)
	if err != nil {
		return false, err
	}
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(raw, &reply); err != nil {
		return false, nil
	}
	if string(reply["action"]) != `"accept"` {
		// Decode strings so equivalent JSON escapes retain their JSON meaning.
		var action string
		if err := json.Unmarshal(reply["action"], &action); err != nil || action != "accept" {
			return false, nil
		}
	}
	var content map[string]json.RawMessage
	if err := json.Unmarshal(reply["content"], &content); err != nil {
		return false, nil
	}
	return string(content[field]) == "true", nil
}
