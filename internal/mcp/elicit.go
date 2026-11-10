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
	yes, _, err := ElicitAnswer(ctx, message, field, title, "", timeout)
	return yes, err
}

// ElicitAnswer is Elicit plus how the client ended the form: "accept",
// "decline", "cancel", or "" for any other reply. yes is true only for an
// accepted explicit true, so the action is for wording, never for authority.
// A non-empty hint is sent as the field description.
func ElicitAnswer(ctx context.Context, message, field, title, hint string, timeout time.Duration) (yes bool, action string, err error) {
	session, ok := ctx.Value(elicitationSessionKey{}).(elicitationSession)
	if !ok || !session.form {
		return false, "", ErrElicitationUnavailable
	}
	prop := map[string]any{"type": "boolean", "title": title, "default": false}
	if hint != "" {
		prop["description"] = hint
	}
	raw, err := CallClient(ctx, "elicitation/create", map[string]any{
		"message":         message,
		"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{field: prop}, "required": []string{field}},
	}, timeout)
	if err != nil {
		return false, "", err
	}
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(raw, &reply); err != nil {
		return false, "", nil
	}
	// Decode the string so equivalent JSON escapes keep their JSON meaning.
	if err := json.Unmarshal(reply["action"], &action); err != nil {
		return false, "", nil
	}
	switch action {
	case "accept":
	case "decline", "cancel":
		return false, action, nil
	default:
		return false, "", nil
	}
	var content map[string]json.RawMessage
	if err := json.Unmarshal(reply["content"], &content); err != nil {
		return false, action, nil
	}
	return string(content[field]) == "true", action, nil
}
