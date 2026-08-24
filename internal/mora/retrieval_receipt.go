package mora

import (
	"net/url"
	"slices"
	"strings"
)

func diversityKeys(m Memory) []string {
	keys := []string{"source:" + canonicalSourceID(m), "type:" + m.Type}
	if ts := evidenceTimestamp(m); len(ts) >= 7 {
		keys = append(keys, "month:"+ts[:7])
	}
	for id := range clusterIdentitySetForReceipt(m) {
		keys = append(keys, "entity:"+id)
	}
	slices.Sort(keys)
	return keys
}

func clusterIdentitySetForReceipt(m Memory) map[string]bool {
	set := map[string]bool{}
	var add func(any)
	add = func(v any) {
		switch value := v.(type) {
		case string:
			if normalized := strings.ToLower(strings.TrimSpace(value)); normalized != "" {
				set[normalized] = true
			}
		case []string:
			for _, item := range value {
				add(item)
			}
		case []any:
			for _, item := range value {
				add(item)
			}
		case []map[string]string:
			for _, pair := range value {
				add(pair["handle"])
			}
		}
	}
	for _, field := range []string{"from", "to", "cc", "attendees", "organizer", "participants"} {
		add(m.Meta[field])
	}
	return set
}

// diversifyEvidence preserves the strongest result, then greedily promotes
// unseen source, entity, month, and evidence-type facets. It is a permutation:
// repeated evidence remains available and can still corroborate; only the
// presentation and byte-budget survival order changes.
func diversifyEvidence(in []Memory) []Memory {
	if len(in) < 2 {
		return in
	}
	remaining := append([]Memory(nil), in...)
	out := make([]Memory, 0, len(in))
	seen := map[string]bool{}
	for len(remaining) > 0 {
		best, bestNovelty := 0, -1
		if len(out) != 0 { // the first iteration keeps best=0 and pins strongest
			for i, candidate := range remaining {
				novelty := 0
				for _, key := range diversityKeys(candidate) {
					if !seen[key] {
						novelty++
					}
				}
				if novelty > bestNovelty {
					best, bestNovelty = i, novelty
				}
			}
		}
		chosen := remaining[best]
		out = append(out, chosen)
		for _, key := range diversityKeys(chosen) {
			seen[key] = true
		}
		remaining = append(remaining[:best], remaining[best+1:]...)
	}
	return out
}

type evidenceManifestEntry struct {
	EvidenceID          string `json:"evidence_id"`
	CanonicalSourceID   string `json:"canonical_source_id"`
	Timestamp           string `json:"timestamp,omitempty"`
	DeepLink            string `json:"deep_link,omitempty"`
	IngestCorrelationID string `json:"ingest_correlation_id,omitempty"`
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

func ingestCorrelationID(m Memory) string {
	value, _ := m.Meta["ingest_correlation_id"].(string)
	return value
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
		add(evidenceManifestEntry{EvidenceID: m.ID, CanonicalSourceID: canonicalSourceID(m), Timestamp: evidenceTimestamp(m), DeepLink: evidenceDeepLink(m), IngestCorrelationID: ingestCorrelationID(m)})
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
