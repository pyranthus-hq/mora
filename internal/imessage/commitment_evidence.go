package imessage

import (
	"fmt"
	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/segments"
	"strconv"
	"strings"
	"time"
)

type CommitmentMessage struct {
	MessageRef, BlockRef, Body, At string
	Self                           bool
}

// CommitmentMessages returns present=false only when the immutable message-evidence schema is absent. Malformed or partial evidence fails closed as present with no messages.
func CommitmentMessages(m memory.Memory) ([]CommitmentMessage, bool) {
	if _, present := m.Meta["message_evidence"]; !present {
		if _, schemaPresent := m.Meta["message_evidence_schema"]; schemaPresent {
			return nil, true
		}
		return nil, false
	}
	if fmt.Sprint(m.Meta["message_evidence_schema"]) != "1" {
		return nil, true
	}
	if _, hasDiagnostics := m.Meta["message_evidence_diagnostics"]; hasDiagnostics {
		return nil, true
	}
	messageCount, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(m.Meta["message_count"])))
	if err != nil || messageCount < 1 {
		return nil, true
	}
	rows, diagnostic := segments.Derive(m)
	if diagnostic != nil {
		return nil, true
	}
	if (!m.Truncated && len(rows) != messageCount) || (m.Truncated && len(rows) > messageCount) || !evidenceCoversRenderedBody(m.Text, rows) {
		return nil, true
	}
	messages := make([]CommitmentMessage, 0, len(rows))
	var lastAt time.Time
	for _, row := range rows {
		at, err := time.Parse(time.RFC3339, row.At)
		if err != nil || (!lastAt.IsZero() && at.Before(lastAt)) {
			return nil, true
		}
		lastAt = at
		trimmed := strings.TrimSpace(row.Text)
		if strings.HasPrefix(trimmed, "*") && strings.HasSuffix(trimmed, "*") {
			continue
		}
		direction := segments.Direction(row.BlockRefs)
		body, ok := trustedAuthoredBody(row, direction)
		if !ok {
			return nil, true
		}
		messages = append(messages, CommitmentMessage{MessageRef: row.EvidenceRef, BlockRef: "body", Body: body, At: row.At, Self: direction == "outgoing"})
	}
	return messages, true
}
func trustedAuthoredBody(row segments.Row, direction string) (string, bool) {
	if direction != "incoming" && direction != "outgoing" {
		return "", false
	}
	firstLine, rest, _ := strings.Cut(strings.TrimSpace(row.Text), "\n")
	label, firstBody, ok := strings.Cut(firstLine, ":")
	if !ok {
		return "", false
	}
	label = strings.TrimSpace(label)
	if direction == "outgoing" {
		if !strings.EqualFold(label, "me") || !strings.EqualFold(strings.TrimSpace(row.Sender), "me") {
			return "", false
		}
	} else if strings.EqualFold(label, "me") || !strings.EqualFold(label, strings.TrimSpace(row.Sender)) {
		return "", false
	}
	body := strings.TrimSpace(firstBody + "\n" + rest)
	if body == "" {
		return "", false
	}
	return body, true
}
func evidenceCoversRenderedBody(body string, rows []segments.Row) bool {
	cursor := 0
	for _, row := range rows {
		start, end, ok := evidenceByteRange(row.BlockRefs)
		if !ok || start < cursor || end > len(body) || !structuralGap(body[cursor:start]) {
			return false
		}
		cursor = end
	}
	return structuralGap(body[cursor:])
}
func evidenceByteRange(refs []string) (int, int, bool) {
	for _, ref := range refs {
		raw, ok := strings.CutPrefix(ref, "bytes:")
		if !ok {
			continue
		}
		startRaw, endRaw, ok := strings.Cut(raw, "-")
		if !ok {
			return 0, 0, false
		}
		start, startErr := strconv.Atoi(startRaw)
		end, endErr := strconv.Atoi(endRaw)
		return start, end, startErr == nil && endErr == nil && start >= 0 && end > start
	}
	return 0, 0, false
}
func structuralGap(gap string) bool {
	for _, line := range strings.Split(gap, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "> ") {
			continue
		}
		return false
	}
	return true
}
