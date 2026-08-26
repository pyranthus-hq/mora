package mora

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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

func TestRetentionCLIAllowsDocumentedPositionalFirstFlags(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--title", "old", "--text", "old body")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	report, err := buildRetentionReport(cfg, time.Now().Add(400*24*time.Hour), 365, 30)
	if err != nil || len(report.Candidates) == 0 {
		t.Fatalf("report: %v candidates=%d", err, len(report.Candidates))
	}
	out := run(t, "retention", "decide", report.ReportID, report.Candidates[0].ID, "--action", "keep", "--json")
	var receipt map[string]any
	if err := json.Unmarshal([]byte(out), &receipt); err != nil || receipt["schema"] != "mora.retention.decision" {
		t.Fatalf("decision receipt: %s err=%v", out, err)
	}
}
