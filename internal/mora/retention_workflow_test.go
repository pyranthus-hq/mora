package mora

import (
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
