package eywa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"
)

// MaxUpdateChars is the service's limit on an update's text and excerpt,
// counted the way its JSON schema counts (UTF-16 code units).
const MaxUpdateChars = 8000

// Update is exactly what one push sends for one authored memory. The preview
// prints these fields; nothing else leaves the machine.
type Update struct {
	MemoryID       string
	Title          string
	Text           string
	SourceURI      string
	ContentHash    string
	Excerpt        string
	IdempotencyKey string
}

// CanonicalContent is the service's canonical form for hashing: NFC, trimmed.
func CanonicalContent(s string) string { return strings.TrimSpace(norm.NFC.String(s)) }

// ContentHash is the SHA-256 hex of the canonical content, as the lineage
// contract requires.
func ContentHash(s string) string {
	sum := sha256.Sum256([]byte(CanonicalContent(s)))
	return hex.EncodeToString(sum[:])
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// SourceURIFor names the Mora memory as an agent run step of this agent:
// eywa-run://<agent-id>/mora/<memory-id>.
func SourceURIFor(agentID, memoryID string) string {
	return "eywa-run://" + url.PathEscape(agentID) + "/mora/" + url.PathEscape(memoryID)
}

// Local refusal reasons. BuildUpdate wraps one of these, so a caller can report
// a stable reason code without repeating any memory text.
var (
	ErrNoAgentID     = errors.New("eywa: push needs the agent id (mora connect eywa --agent-id <id>)")
	ErrEmptyUpdate   = errors.New("no text to send")
	ErrUpdateTooLong = errors.New("over the service's size limit")
)

// RefusalCode names why BuildUpdate refused an update: missing_agent_id,
// empty_text, too_long, or invalid_update for anything else.
func RefusalCode(err error) string {
	switch {
	case errors.Is(err, ErrNoAgentID):
		return "missing_agent_id"
	case errors.Is(err, ErrEmptyUpdate):
		return "empty_text"
	case errors.Is(err, ErrUpdateTooLong):
		return "too_long"
	default:
		return "invalid_update"
	}
}

// BuildUpdate turns one authored memory into the update a push would send.
// The excerpt is the full text, so the content hash covers exactly what the
// service will verify. An empty or over-long memory is refused, never trimmed.
func BuildUpdate(agentID, memoryID, title, body string) (Update, error) {
	if strings.TrimSpace(agentID) == "" {
		return Update{}, ErrNoAgentID
	}
	text := strings.TrimSpace(body)
	t := strings.TrimSpace(title)
	if t != "" && !strings.HasPrefix(text, t) {
		text = strings.TrimSpace(t + "\n\n" + text)
	}
	text = CanonicalContent(text)
	if text == "" {
		return Update{}, fmt.Errorf("memory %s has %w", memoryID, ErrEmptyUpdate)
	}
	if n := utf16Len(text); n > MaxUpdateChars {
		return Update{}, fmt.Errorf("memory %s is %d characters, %w of %d — split or trim it", memoryID, n, ErrUpdateTooLong, MaxUpdateChars)
	}
	hash := ContentHash(text)
	key := "mora:" + hash[:24] + ":" + memoryID
	if len(key) > 128 {
		key = key[:128]
	}
	return Update{
		MemoryID: memoryID, Title: t, Text: text, Excerpt: text, ContentHash: hash,
		SourceURI:      SourceURIFor(agentID, memoryID),
		IdempotencyKey: key,
	}, nil
}

// WriteAck is the service's acknowledgement of an update.
type WriteAck struct {
	WriteID string `json:"write_id"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
}

// Remember sends one update to the remember tool as an agent update with lineage.
func (c *Client) Remember(ctx context.Context, topic string, u Update) (WriteAck, error) {
	if strings.TrimSpace(topic) == "" {
		return WriteAck{}, errors.New("eywa: push needs the agent's scope topic (mora connect eywa --topic <topic>)")
	}
	args := map[string]any{
		"text":  u.Text,
		"topic": topic,
		"lineage": map[string]any{
			"source_uri":   u.SourceURI,
			"content_hash": u.ContentHash,
			"excerpt":      u.Excerpt,
		},
		"idempotency_key": u.IdempotencyKey,
	}
	var ack WriteAck
	if err := c.CallTool(ctx, "remember", args, &ack); err != nil {
		var fe *ForbiddenError
		if errors.As(err, &fe) {
			return WriteAck{}, &AuthError{Status: 403, Stage: "mcp", Detail: "the agent may not write to topic " + topic}
		}
		return WriteAck{}, err
	}
	if ack.WriteID == "" {
		return WriteAck{}, errors.New("eywa: remember returned no write id")
	}
	return ack, nil
}

// WriteStatus is a write's current outcome as get_memory(write_id) reports it.
type WriteStatus struct {
	WriteID string `json:"write_id"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
}

// GetWriteStatus reads one write's status.
func (c *Client) GetWriteStatus(ctx context.Context, writeID string) (WriteStatus, error) {
	var got struct {
		Write *struct {
			WriteID string  `json:"write_id"`
			Status  string  `json:"status"`
			Reason  *string `json:"reason"`
		} `json:"write"`
	}
	if err := c.CallTool(ctx, "get_memory", map[string]any{"write_id": writeID}, &got); err != nil {
		return WriteStatus{}, err
	}
	if got.Write == nil {
		return WriteStatus{}, fmt.Errorf("eywa: get_memory returned no status for write %s", writeID)
	}
	ws := WriteStatus{WriteID: firstNonEmpty(got.Write.WriteID, writeID), Status: got.Write.Status}
	if got.Write.Reason != nil {
		ws.Reason = *got.Write.Reason
	}
	return ws, nil
}

// FinalStatus reports whether the service has decided an update. "received"
// is the only pending state; in_review and held are outcomes the service owns
// (a curator may still act on them later).
func FinalStatus(status string) bool {
	switch status {
	case "admitted", "admitted-unchanged", "in_review", "held", "rejected", "no-fact":
		return true
	default:
		return false
	}
}
