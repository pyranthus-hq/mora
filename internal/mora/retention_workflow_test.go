package mora

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestRetentionReportIsReadOnlyAndDecisionsAreExplicit(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	m := Memory{ID: "old-memory", Scope: "personal", Type: "note", Title: "Old", Text: "old evidence", CreatedAt: "2024-01-01T00:00:00Z", ContentHash: "old"}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	files, err := allMemoryFiles(cfg)
	if err != nil || len(files) != 1 {
		t.Fatalf("memory files=%v err=%v", files, err)
	}
	path := files[0]
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := buildRetentionReport(cfg, time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC), 365, 0)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || len(report.Candidates) != 1 || report.RecoveryDays != 30 {
		t.Fatalf("report mutated vault or missed candidate: %+v", report)
	}
	updated, err := decideRetentionCandidate(cfg, report.ReportID, m.ID, retentionDecision{Action: "compact", Summary: "durable summary"})
	if err != nil || updated.Candidates[0].Decision == nil || updated.Candidates[0].Decision.Action != "compact" {
		t.Fatalf("decision=%+v err=%v", updated, err)
	}
}

func TestRetentionExecutionUsesPreviewTargetsAndCompactsWithProvenance(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	for _, m := range []Memory{
		{ID: "keep-old", Scope: "personal", Type: "note", Title: "Keep", Text: "keep", CreatedAt: "2024-01-01T00:00:00Z", ContentHash: "keep"},
		{ID: "compact-old", Scope: "personal", Type: "email", Title: "Compact", Text: "long source", Provider: "gmail", ProviderID: "thread/old", CreatedAt: "2024-01-02T00:00:00Z", ContentHash: "compact"},
	} {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	report, err := buildRetentionReport(cfg, now, 365, 30)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decideRetentionCandidate(cfg, report.ReportID, "keep-old", retentionDecision{Action: "keep"}); err != nil {
		t.Fatal(err)
	}
	report, err = decideRetentionCandidate(cfg, report.ReportID, "compact-old", retentionDecision{Action: "compact", Summary: "Durable compact fact"})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := validateRetentionReport(cfg, report)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := executeRetentionReport(context.Background(), cfg, report.ReportID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview) != 1 || len(receipt.TargetIDs) != 1 || preview[0] != receipt.TargetIDs[0] || receipt.TargetIDs[0] != "compact-old" {
		t.Fatalf("preview=%v execution=%+v", preview, receipt)
	}
	if _, err := findMemoryRaw(cfg, "compact-old"); err == nil {
		t.Fatal("compacted source still exists")
	}
	memories, err := listMemories(cfg, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var retained *Memory
	for i := range memories {
		if memories[i].Type == "durable" {
			retained = &memories[i]
		}
	}
	if retained == nil || retained.Text != "Durable compact fact" {
		t.Fatalf("retained memory=%+v", retained)
	}
	sources, _ := retained.Meta["retention_source_ids"].([]any)
	if len(sources) != 1 || sources[0] != "compact-old" {
		t.Fatalf("retention provenance=%+v", retained.Meta)
	}
}

func TestRetentionExecutionRefusesCandidateChangedAfterPreview(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	if err := writeMemory(cfg, Memory{ID: "changed", Scope: "personal", Type: "note", Title: "Old", Text: "one", CreatedAt: "2024-01-01T00:00:00Z", ContentHash: "one"}); err != nil {
		t.Fatal(err)
	}
	report, err := buildRetentionReport(cfg, time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC), 365, 30)
	if err != nil {
		t.Fatal(err)
	}
	report, err = decideRetentionCandidate(cfg, report.ReportID, "changed", retentionDecision{Action: "delete"})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := retentionCandidatePath(cfg, report.Candidates[0])
	if err := os.WriteFile(path, []byte("changed after preview"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := executeRetentionReport(context.Background(), cfg, report.ReportID, time.Now()); err == nil {
		t.Fatal("changed candidate executed")
	}
}

func TestRecoveryManifestEncryptionRoundTripAndTamperFails(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	plain := recoveryPlaintext{SchemaVersion: 1, ManifestID: "recovery-one", ReportID: "report-one", CreatedAt: "2026-08-24T12:00:00Z", ExpiresAt: "2026-09-23T12:00:00Z", Entries: []recoveryEntry{{ID: "m1", Path: "memories/m1.md", Data: []byte("secret memory"), ContentSHA: sha256Hex([]byte("secret memory"))}}}
	encrypted, err := encryptRecoveryManifest(cfg, plain)
	if err != nil {
		t.Fatal(err)
	}
	if encrypted.Ciphertext == "" || encrypted.Ciphertext == string(plain.Entries[0].Data) {
		t.Fatalf("manifest is not encrypted: %+v", encrypted)
	}
	got, err := decryptRecoveryManifest(cfg, encrypted)
	if err != nil || string(got.Entries[0].Data) != "secret memory" {
		t.Fatalf("round trip=%+v err=%v", got, err)
	}
	encrypted.Ciphertext = encrypted.Ciphertext[:len(encrypted.Ciphertext)-2] + "AA"
	if _, err := decryptRecoveryManifest(cfg, encrypted); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
}
