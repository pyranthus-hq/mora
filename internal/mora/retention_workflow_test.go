package mora

import (
	"context"
	"database/sql"
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

func TestRetentionIntegrityDetectsMissingIndexAndDanglingGraphEdge(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	if err := writeMemory(cfg, Memory{ID: "integrity-memory", Scope: "personal", Type: "note", Title: "Integrity", Text: "Alex evidence", CreatedAt: "2026-01-01T00:00:00Z", ContentHash: "integrity", Meta: map[string]any{"from": "alex@example.com"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM memories WHERE id = 'integrity-memory'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO edges(src, rel, dst, evidence_id) VALUES('missing-src','RELATED','missing-dst','missing-evidence')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := verifyRetentionIntegrity(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Healthy || len(got.MissingIndexIDs) != 1 || got.MissingIndexIDs[0] != "integrity-memory" || len(got.DanglingGraphEdges) == 0 {
		t.Fatalf("integrity verifier missed sabotage: %+v", got)
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

func TestRetentionRecoveryRestoresRecordAndIndexIntegrity(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	original := Memory{ID: "recover-me", Scope: "personal", Type: "note", Title: "Recover", Text: "recoverable evidence", CreatedAt: "2024-01-01T00:00:00Z", ContentHash: "recover"}
	if err := writeMemory(cfg, original); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	report, err := buildRetentionReport(cfg, now, 365, 30)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decideRetentionCandidate(cfg, report.ReportID, original.ID, retentionDecision{Action: "delete"}); err != nil {
		t.Fatal(err)
	}
	executed, err := executeRetentionReport(context.Background(), cfg, report.ReportID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := findMemoryRaw(cfg, original.ID); err == nil {
		t.Fatal("deleted record remained before recovery")
	}
	recovered, err := recoverRetentionManifest(context.Background(), cfg, executed.ManifestID, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.RestoredIDs) != 1 || recovered.RestoredIDs[0] != original.ID || !recovered.Integrity.Healthy {
		t.Fatalf("recovery=%+v", recovered)
	}
	got, err := findMemory(cfg, original.ID)
	if err != nil || got.Text != original.Text {
		t.Fatalf("restored memory=%+v err=%v", got, err)
	}
}

func TestRetentionRecoveryRefusesExpiredWindow(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	plain := recoveryPlaintext{SchemaVersion: 1, ManifestID: "expired", ReportID: "report", CreatedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2026-01-31T00:00:00Z"}
	if err := writeEncryptedRecovery(cfg, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverRetentionManifest(context.Background(), cfg, plain.ManifestID, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("expired recovery succeeded")
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
