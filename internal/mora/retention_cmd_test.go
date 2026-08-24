package mora

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRetentionCLIRequiresReviewAndExplicitConfirmation(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	if err := writeMemory(cfg, Memory{ID: "cli-old", Scope: "personal", Type: "note", Title: "Old", Text: "old", CreatedAt: "2024-01-01T00:00:00Z", ContentHash: "old"}); err != nil {
		t.Fatal(err)
	}
	out := run(t, "retention", "report", "--older-than-days", "365", "--json")
	var report retentionReport
	if err := json.Unmarshal([]byte(out), &report); err != nil || report.ReportID == "" || len(report.Candidates) != 1 {
		t.Fatalf("report=%s err=%v", out, err)
	}
	run(t, "retention", "decide", "--action", "delete", "--json", report.ReportID, "cli-old")
	if _, err := runErr(t, "retention", "execute", report.ReportID); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("execution without confirmation err=%v", err)
	}
}
