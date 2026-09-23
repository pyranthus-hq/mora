package mora

import "testing"

func TestEvidenceRefReadCarriesDerivedProvenance(t *testing.T) {
	cfg := seedGmailSegmentsFixture(t)
	res, err := mcpReadMemory(testCtx(t), cfg, map[string]any{
		"id": gsWellFormedID, "evidence_ref": gsWellMsg2Ref,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := res.(map[string]any)
	got := payload["memory"].(Memory)
	if got.Provenance != "evidence" {
		t.Fatalf("evidence_ref read provenance = %q", got.Provenance)
	}
	if receipt := payload["receipt"]; receipt == nil {
		t.Fatal("evidence_ref read lost its scoped receipt")
	}
}
