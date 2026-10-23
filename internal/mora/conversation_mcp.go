package mora

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"time"
	"unicode/utf8"

	conv "github.com/pyranthus-hq/mora/internal/conversation"
)

func conversationSearchHits(cfg Config, query, scope string, budget int) ([]conv.Hit, error) {
	settings, err := conv.LoadSettings(conversationSettingsPath(cfg))
	if err != nil {
		return nil, err
	}
	if len(settings.Sources) == 0 {
		return nil, nil
	}
	hits, err := conversationStore(cfg).Search(query, scope, 3)
	if err != nil {
		return nil, err
	}
	out := []conv.Hit{}
	for _, h := range hits {
		candidate := append(out, h)
		b, err := json.Marshal(candidate)
		if err != nil {
			return nil, err
		}
		if len(b) > budget {
			break
		}
		out = candidate
	}
	return out, nil
}
func withConversationEnvelope(envelope any, continuity, status string) (any, error) {
	b, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err = json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	out["conversation_continuity"] = continuity
	out["conversation_capture"] = status
	return out, nil
}

func conversationInt(args map[string]any, key string, def int) (int, error) {
	raw, ok := args[key]
	if !ok {
		return def, nil
	}
	var n float64
	switch v := raw.(type) {
	case int:
		return v, nil
	case float64:
		n = v
	case json.Number:
		var err error
		n, err = v.Float64()
		if err != nil {
			return 0, err
		}
	default:
		return 0, errors.New(key + " must be an integer")
	}
	if n != math.Trunc(n) || math.IsNaN(n) || math.IsInf(n, 0) || n > math.MaxInt32 || n < math.MinInt32 {
		return 0, errors.New(key + " must be a bounded integer")
	}
	return int(n), nil
}
func mcpConversationList(_ context.Context, cfg Config, args map[string]any) (any, error) {
	if err := conversationGuardPaths(cfg); err != nil {
		return nil, err
	}
	limit, err := conversationInt(args, "limit", 10)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		return nil, errors.New("limit must be 1..100")
	}
	sessions, err := conversationStore(cfg).List(strArg(args, "scope", ""))
	if err != nil {
		return nil, err
	}
	more := 0
	if len(sessions) > limit {
		more = len(sessions) - limit
		sessions = sessions[:limit]
	}
	return map[string]any{"sessions": sessions, "more": more, "health": compactHealthOf(cfg, time.Now())}, nil
}
func mcpConversationSearch(_ context.Context, cfg Config, args map[string]any) (any, error) {
	if err := conversationGuardPaths(cfg); err != nil {
		return nil, err
	}
	query := strArg(args, "query", "")
	if query == "" {
		return nil, errors.New("query required")
	}
	limit, err := conversationInt(args, "limit", 8)
	if err != nil {
		return nil, err
	}
	hits, err := conversationStore(cfg).Search(query, strArg(args, "scope", ""), limit)
	return map[string]any{"health": compactHealthOf(cfg, time.Now()), "results": hits, "provenance_note": "Original conversation records are evidence of what was said or returned, not independent proof of truth. User-kind records may contain pasted or forwarded material."}, err
}
func mcpConversationView(_ context.Context, cfg Config, args map[string]any) (any, error) {
	if err := conversationGuardPaths(cfg); err != nil {
		return nil, err
	}
	budget, err := conversationInt(args, "max_bytes", 32000)
	if err != nil {
		return nil, err
	}
	v, err := conversationStore(cfg).View(strArg(args, "session", ""), budget)
	if err != nil {
		return nil, err
	}
	// Text already contains every node; do not double the context by returning
	// the same nodes as a second representation.
	return map[string]any{"session": v.Session, "view": v.Text, "summary_kind": v.SummaryKind, "health": compactHealthOf(cfg, time.Now())}, nil
}
func mcpConversationZoom(_ context.Context, cfg Config, args map[string]any) (any, error) {
	if err := conversationGuardPaths(cfg); err != nil {
		return nil, err
	}
	start, err := conversationInt(args, "start", -1)
	if err != nil {
		return nil, err
	}
	count, err := conversationInt(args, "count", 1)
	if err != nil {
		return nil, err
	}
	z, err := conversationStore(cfg).Zoom(strArg(args, "session", ""), start, count)
	if err != nil {
		return nil, err
	}
	if z.Message == nil {
		return map[string]any{"session": z.Session, "start": z.Start, "count": z.Count, "nodes": z.Nodes, "health": compactHealthOf(cfg, time.Now())}, nil
	}
	offset, err := conversationInt(args, "offset", 0)
	if err != nil {
		return nil, err
	}
	budget, err := conversationInt(args, "max_bytes", 8000)
	if err != nil {
		return nil, err
	}
	if offset < 0 || offset > len(z.Message.Text) || budget < 1 || budget > 32000 {
		return nil, errors.New("offset outside message or max_bytes outside 1..32000")
	}
	text := z.Message.Text
	if offset < len(text) && !utf8.RuneStart(text[offset]) {
		return nil, errors.New("offset must be a UTF-8 boundary")
	}
	end := offset + budget
	if end > len(text) {
		end = len(text)
	}
	for end > offset && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	if end == offset && end < len(text) {
		return nil, errors.New("max_bytes too small for next UTF-8 character")
	}
	out := map[string]any{"health": compactHealthOf(cfg, time.Now()), "session": z.Session, "start": start, "count": 1, "kind": z.Message.Kind, "date": z.Message.Date, "text": text[offset:end], "offset": offset, "total_bytes": len(text), "truncated": end < len(text)}
	if end < len(text) {
		out["next_offset"] = end
	}
	return out, nil
}

func conversationBriefPayload(cfg Config, args map[string]any, budget int) (string, int) {
	if _, ok := args["entity"]; ok {
		return "", budget
	}
	if _, ok := args["since_days"]; ok {
		return "", budget
	}
	text, err := conversationContinuity(cfg, strArg(args, "scope", ""), "", budget/4)
	if err != nil {
		text = "Conversation continuity unavailable; run mora conversation status."
	}
	if text == "" {
		return "", budget
	}
	// Escaping in JSON can expand the text. Reserve its encoded size, keeping
	// the opt-in addition inside the existing brief budget.
	b, _ := json.Marshal(text)
	status, _ := json.Marshal(conversationCaptureWarning(cfg))
	if len(b)+len(status)+64 >= budget {
		return "", budget
	}
	return text, budget - len(b) - len(status) - 64
}
func conversationCaptureWarning(cfg Config) string {
	b, err := os.ReadFile(conversationStatusPath(cfg))
	if err != nil {
		return "Conversation capture has not completed; run mora conversation sync."
	}
	var r struct {
		Checked string   `json:"checked"`
		Errors  []string `json:"errors"`
	}
	if err = json.Unmarshal(b, &r); err != nil {
		return "Conversation capture status is unreadable."
	}
	if len(r.Errors) > 0 {
		return "Conversation capture has errors; originals may be incomplete. Run mora conversation status."
	}
	return "Conversation capture last checked: " + r.Checked + ". This is a snapshot of allowlisted sources."
}
