package mora

import (
	"net/url"
	"slices"
	"strings"
)

type evidenceManifestEntry struct {
	EvidenceID        string `json:"evidence_id"`
	CanonicalSourceID string `json:"canonical_source_id"`
	Timestamp         string `json:"timestamp,omitempty"`
	DeepLink          string `json:"deep_link,omitempty"`
}

type rankingReceipt struct {
	EvidenceID          string   `json:"evidence_id"`
	Position            int      `json:"position"`
	Score               float64  `json:"score,omitempty"`
	SupportingLanes     []string `json:"supporting_lanes"`
	Why                 []string `json:"why"`
	CollapsedEvidenceID []string `json:"collapsed_evidence_ids,omitempty"`
}

func canonicalSourceID(m Memory) string {
	if m.Owner != "" {
		return "share:" + m.Owner
	}
	if m.Provider != "" {
		if m.Account != "" {
			return m.Provider + ":" + m.Account
		}
		return m.Provider
	}
	if m.Source != "" {
		return m.Source
	}
	return "vault"
}

func evidenceTimestamp(m Memory) string {
	if s, ok := m.Meta["occurred_at"].(string); ok && s != "" {
		return s
	}
	return m.CreatedAt
}

func evidenceDeepLink(m Memory) string {
	for _, key := range []string{"canonical_url", "html_link", "url"} {
		if s, ok := m.Meta[key].(string); ok && (strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) {
			return s
		}
	}
	if m.Provider == "gmail" && m.ProviderID != "" {
		id := m.ProviderID
		if slash := strings.LastIndexByte(id, '/'); slash >= 0 {
			id = id[slash+1:]
		}
		if id != "" {
			return "https://mail.google.com/mail/#all/" + url.PathEscape(id)
		}
	}
	return ""
}

func evidenceManifest(returned, originals []Memory) []evidenceManifestEntry {
	byID := make(map[string]Memory, len(originals))
	for _, m := range originals {
		byID[m.ID] = m
	}
	seen := map[string]bool{}
	manifest := make([]evidenceManifestEntry, 0, len(returned))
	add := func(entry evidenceManifestEntry) {
		if entry.EvidenceID == "" || seen[entry.EvidenceID] {
			return
		}
		seen[entry.EvidenceID] = true
		manifest = append(manifest, entry)
	}
	for _, shaped := range returned {
		m := shaped
		if original, ok := byID[shaped.ID]; ok {
			m = original
		}
		add(evidenceManifestEntry{EvidenceID: m.ID, CanonicalSourceID: canonicalSourceID(m), Timestamp: evidenceTimestamp(m), DeepLink: evidenceDeepLink(m)})
		for _, ref := range shaped.Corroborating {
			add(evidenceManifestEntry{EvidenceID: ref.ID, CanonicalSourceID: ref.Source, Timestamp: ref.CreatedAt})
		}
		if shaped.Evidence != nil {
			add(evidenceManifestEntry{EvidenceID: shaped.Evidence.EvidenceRef, CanonicalSourceID: canonicalSourceID(m), Timestamp: shaped.Evidence.At, DeepLink: evidenceDeepLink(m)})
		}
	}
	return manifest
}

func traceLanes(id string, trace retrievalTrace) []string {
	var lanes []string
	for _, lane := range []struct {
		name string
		ids  []string
	}{{"fts", trace.FTS}, {"vector", trace.Vec}, {"graph", trace.Graph}, {"segment", trace.Segment}} {
		if slices.Contains(lane.ids, id) {
			lanes = append(lanes, lane.name)
		}
	}
	return lanes
}

func rankingReceipts(returned []Memory, trace retrievalTrace, scoreFused bool) []rankingReceipt {
	out := make([]rankingReceipt, 0, len(returned))
	for i, m := range returned {
		lanes := traceLanes(m.ID, trace)
		why := []string{"rank_order"}
		if scoreFused {
			why = []string{"reciprocal_rank_fusion"}
		}
		if len(lanes) > 0 {
			why = append(why, "matched:"+strings.Join(lanes, ","))
		}
		collapsed := make([]string, 0, len(m.Corroborating))
		for _, ref := range m.Corroborating {
			collapsed = append(collapsed, ref.ID)
		}
		out = append(out, rankingReceipt{EvidenceID: m.ID, Position: i + 1, Score: m.Score, SupportingLanes: lanes, Why: why, CollapsedEvidenceID: collapsed})
	}
	return out
}
