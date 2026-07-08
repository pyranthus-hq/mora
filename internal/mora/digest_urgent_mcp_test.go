package mora

import "testing"

// Issue #62 defect 2 — the urgent shelf is lifted out of the sections, so the MCP
// digest/brief payload must carry it as its own protected array or an agent loses the
// highest-value items entirely.
func TestDigestMCPPayloadCarriesUrgentShelf(t *testing.T) {
	d := Digest{
		Generated:  "2026-07-02T00:00:00Z",
		Urgent:     []DigestItem{{ID: "g1", Title: "MSA sign-off", Snippet: "sign by eod", Source: "gmail"}},
		UrgentMore: 2,
	}
	p := digestMCPPayload(Config{}, d, 10000)

	u, ok := p["urgent"].([]DigestItem)
	if !ok || len(u) != 1 || u[0].ID != "g1" {
		t.Fatalf("MCP payload must carry the urgent shelf; got %#v", p["urgent"])
	}
	if p["urgent_more"] != 2 {
		t.Fatalf("MCP payload must carry urgent_more; got %#v", p["urgent_more"])
	}
}
