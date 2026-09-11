package mora

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/segments"
	"sort"
	"time"
)

// Explicit source event dates only: recently re-imported old mail must not
// displace old threads with new replies. This lists evidence, not obligations.
func recentSourceEvents(items []Memory, now time.Time, hours, limit int) []Memory {
	cutoff := now.Add(-time.Duration(hours) * time.Hour)
	out := []Memory{}
	times := map[string]time.Time{}
	for _, m := range items {
		raw, _ := m.Meta["occurred_at"].(string)
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil || at.Before(cutoff) || at.After(now) {
			continue
		}
		times[m.ID] = at
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := times[out[i].ID], times[out[j].ID]
		if a.Equal(b) {
			return out[i].ID < out[j].ID
		}
		return a.After(b)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		if out[i].Provider != "gmail" {
			continue
		}
		rows, diag := segments.Derive(out[i])
		if diag != nil {
			continue
		}
		var latest *segments.Row
		for j := range rows {
			at, err := time.Parse(time.RFC3339, rows[j].At)
			if err != nil {
				continue
			}
			if latest == nil {
				latest = &rows[j]
				continue
			}
			previous, _ := time.Parse(time.RFC3339, latest.At)
			if at.After(previous) {
				latest = &rows[j]
			}
		}
		if latest != nil {
			body := []rune(latest.Text)
			if len(body) > 1600 {
				body = body[:1600]
			}
			ev := memory.GmailSegmentEvidence{EvidenceRef: latest.EvidenceRef, Sender: latest.Sender, At: latest.At, Snippet: string(body)}
			segments.AttachReadable(&ev, latest.Text, segments.ReadableListRunes)
			out[i].Evidence = &ev
		}
	}
	return out
}
