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

// ElicitOutcome is what the human did with one form elicitation. Only
// ElicitYes is a yes; every other value, and every error, means no.
type ElicitOutcome int

const (
	// ElicitInvalid is a reply that did not parse as an elicitation result.
	ElicitInvalid ElicitOutcome = iota
	// ElicitYes is action "accept" with the boolean field exactly true.
	ElicitYes
	// ElicitNo is action "accept" without the field set to true.
	ElicitNo
	// ElicitDeclined is action "decline": the human said no.
	ElicitDeclined
	// ElicitCancelled is action "cancel", or any other action: dismissed.
	ElicitCancelled
)

// ElicitAvailable reports whether this request's session declared form
// elicitation at initialize.
func ElicitAvailable(ctx context.Context) bool {
	session, ok := ctx.Value(elicitationSessionKey{}).(elicitationSession)
	return ok && session.form
}

// Elicit asks for a boolean confirmation and accepts only an explicit true.
func Elicit(ctx context.Context, message, field, title string, timeout time.Duration) (bool, error) {
	outcome, err := ElicitConfirm(ctx, message, field, title, timeout)
	return err == nil && outcome == ElicitYes, err
}

// ElicitConfirm is Elicit that also says how a no came about, so a caller can
// tell the agent why. It fails closed: anything but an accept whose content
// holds the field as the JSON literal true is not ElicitYes.
func ElicitConfirm(ctx context.Context, message, field, title string, timeout time.Duration) (ElicitOutcome, error) {
	if !ElicitAvailable(ctx) {
		return ElicitInvalid, ErrElicitationUnavailable
	}
	raw, err := CallClient(ctx, "elicitation/create", map[string]any{
		"message":         message,
		"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{field: map[string]any{"type": "boolean", "title": title, "default": false}}, "required": []string{field}},
	}, timeout)
	if err != nil {
		return ElicitInvalid, err
	}
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(raw, &reply); err != nil {
		return ElicitInvalid, nil
	}
	// Decode strings so equivalent JSON escapes retain their JSON meaning.
	var action string
	if err := json.Unmarshal(reply["action"], &action); err != nil {
		return ElicitInvalid, nil
	}
	switch action {
	case "accept":
	case "decline":
		return ElicitDeclined, nil
	default:
		return ElicitCancelled, nil
	}
	var content map[string]json.RawMessage
	if err := json.Unmarshal(reply["content"], &content); err != nil {
		return ElicitNo, nil
	}
	if string(content[field]) == "true" {
		return ElicitYes, nil
	}
	return ElicitNo, nil
}
