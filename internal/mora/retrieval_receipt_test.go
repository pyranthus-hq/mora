package mora

import (
	"context"
	"testing"
)

func TestEvidenceManifestContainsEveryReturnedCitation(t *testing.T) {
	returned := []Memory{{
		ID: "gmail_thread/abc", Provider: "gmail", Account: "work", ProviderID: "thread/abc", CreatedAt: "2026-08-24T10:00:00Z",
		Corroborating: []CorroboratingRef{{ID: "calendar_event/1", Source: "calendar:work", CreatedAt: "2026-08-24T09:00:00Z"}},
		Evidence:      &GmailSegmentEvidence{EvidenceRef: "gmail_message/msg-1", At: "2026-08-24T10:01:00Z"},
	}}
	manifest := evidenceManifest(returned, returned)
	if len(manifest) != 3 {
		t.Fatalf("manifest=%+v", manifest)
	}
	want := map[string]bool{"gmail_thread/abc": true, "calendar_event/1": true, "gmail_message/msg-1": true}
	for _, entry := range manifest {
		delete(want, entry.EvidenceID)
	}
	if len(want) != 0 {
		t.Fatalf("citations absent from manifest: %v", want)
	}
	if manifest[0].CanonicalSourceID != "gmail:work" || manifest[0].DeepLink != "https://mail.google.com/mail/#all/abc" {
		t.Fatalf("canonical provenance=%+v", manifest[0])
	}
}

func TestRankingReceiptExplainsLanesAndCollapse(t *testing.T) {
	rows := []Memory{{ID: "a", Score: 0.8, Corroborating: []CorroboratingRef{{ID: "b"}}}}
	receipts := rankingReceipts(rows, retrievalTrace{FTS: []string{"a"}, Graph: []string{"a"}}, true)
	if len(receipts) != 1 || receipts[0].Position != 1 || len(receipts[0].SupportingLanes) != 2 || len(receipts[0].CollapsedEvidenceID) != 1 || receipts[0].CollapsedEvidenceID[0] != "b" {
		t.Fatalf("receipt=%+v", receipts)
	}
}

func TestDiversifyEvidencePreservesStrongestAndPromotesNovelFacets(t *testing.T) {
	rows := []Memory{
		{ID: "strong", Provider: "gmail", Type: "email", CreatedAt: "2026-08-01T00:00:00Z", Meta: map[string]any{"from": "alex"}},
		{ID: "repeat", Provider: "gmail", Type: "email", CreatedAt: "2026-08-02T00:00:00Z", Meta: map[string]any{"from": "alex"}},
		{ID: "novel", Provider: "calendar", Type: "event", CreatedAt: "2026-07-02T00:00:00Z", Meta: map[string]any{"organizer": "sam"}},
	}
	got := diversifyEvidence(rows)
	if got[0].ID != "strong" || got[1].ID != "novel" || got[2].ID != "repeat" {
		t.Fatalf("diversified order=%v", []string{got[0].ID, got[1].ID, got[2].ID})
	}
}

func TestCrossSourceSearchCitationsAreClosedByManifest(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	for _, m := range []Memory{
		{ID: "gmail_thread/atlas", Scope: "personal", Type: "email", Title: "Atlas launch", Text: "atlas launch evidence", Provider: "gmail", ProviderID: "thread/atlas", Source: "gmail", CreatedAt: "2026-08-24T09:00:00Z", ContentHash: "mail"},
		{ID: "github_issue/atlas", Scope: "personal", Type: "issue", Title: "Atlas launch issue", Text: "atlas launch evidence", Provider: "github", ProviderID: "o/r/1", Source: "github", CreatedAt: "2026-08-24T10:00:00Z", ContentHash: "issue", Meta: map[string]any{"canonical_url": "https://github.com/o/r/issues/1"}},
	} {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	result, err := mcpSearchMemory(testCtx(t), cfg, map[string]any{"query": "atlas launch", "limit": float64(10)})
	if err != nil {
		t.Fatal(err)
	}
	out := result.(map[string]any)
	manifest := out["evidence_manifest"].([]evidenceManifestEntry)
	allowed := map[string]bool{}
	for _, entry := range manifest {
		allowed[entry.EvidenceID] = true
	}
	for _, row := range out["results"].([]Memory) {
		if !allowed[row.ID] {
			t.Fatalf("returned citation %q is absent from manifest %+v", row.ID, manifest)
		}
		for _, ref := range row.Corroborating {
			if !allowed[ref.ID] {
				t.Fatalf("collapsed citation %q is absent from manifest %+v", ref.ID, manifest)
			}
		}
		if row.Evidence != nil && !allowed[row.Evidence.EvidenceRef] {
			t.Fatalf("segment citation %q is absent from manifest %+v", row.Evidence.EvidenceRef, manifest)
		}
	}
}
