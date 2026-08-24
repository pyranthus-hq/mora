package mora

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

const (
	retentionSchemaVersion     = 1
	retentionDefaultOlderDays  = 365
	retentionDefaultRecoverDay = 30
)

type retentionDecision struct {
	Action  string `json:"action"`
	Class   string `json:"class,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type retentionCandidate struct {
	ID         string             `json:"id"`
	Path       string             `json:"path"`
	Type       string             `json:"type"`
	CreatedAt  string             `json:"created_at"`
	ContentSHA string             `json:"content_sha256"`
	SizeBytes  int64              `json:"size_bytes"`
	Decision   *retentionDecision `json:"decision,omitempty"`
}

type retentionReport struct {
	SchemaVersion int                  `json:"schema_version"`
	ReportID      string               `json:"report_id"`
	CreatedAt     string               `json:"created_at"`
	OlderThanDays int                  `json:"older_than_days"`
	RecoveryDays  int                  `json:"recovery_days"`
	Candidates    []retentionCandidate `json:"candidates"`
}

type recoveryEntry struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Mode       uint32 `json:"mode"`
	Data       []byte `json:"data"`
	ContentSHA string `json:"content_sha256"`
}

type recoveryPlaintext struct {
	SchemaVersion int             `json:"schema_version"`
	ManifestID    string          `json:"manifest_id"`
	ReportID      string          `json:"report_id"`
	CreatedAt     string          `json:"created_at"`
	ExpiresAt     string          `json:"expires_at"`
	Entries       []recoveryEntry `json:"entries"`
}

type encryptedRecoveryManifest struct {
	SchemaVersion int    `json:"schema_version"`
	ManifestID    string `json:"manifest_id"`
	ReportID      string `json:"report_id"`
	CreatedAt     string `json:"created_at"`
	ExpiresAt     string `json:"expires_at"`
	Nonce         string `json:"nonce"`
	Ciphertext    string `json:"ciphertext"`
}

type retentionExecutionReceipt struct {
	SchemaVersion int      `json:"schema_version"`
	ReportID      string   `json:"report_id"`
	ManifestID    string   `json:"manifest_id"`
	TargetIDs     []string `json:"target_ids"`
	KeptIDs       []string `json:"kept_ids"`
	ChangedIDs    []string `json:"changed_ids"`
	CompactedIDs  []string `json:"compacted_ids"`
	DeletedIDs    []string `json:"deleted_ids"`
	ExecutedAt    string   `json:"executed_at"`
	IndexCount    int      `json:"index_count"`
}

type retentionIntegrityReceipt struct {
	SchemaVersion      int      `json:"schema_version"`
	VaultCount         int      `json:"vault_count"`
	IndexCount         int      `json:"index_count"`
	MissingIndexIDs    []string `json:"missing_index_ids"`
	OrphanIndexIDs     []string `json:"orphan_index_ids"`
	DanglingGraphEdges []string `json:"dangling_graph_edges"`
	Healthy            bool     `json:"healthy"`
}

type retentionRecoveryReceipt struct {
	SchemaVersion int                       `json:"schema_version"`
	ManifestID    string                    `json:"manifest_id"`
	RestoredIDs   []string                  `json:"restored_ids"`
	RecoveredAt   string                    `json:"recovered_at"`
	Integrity     retentionIntegrityReceipt `json:"integrity"`
}

func retentionRoot(cfg Config) string { return filepath.Join(cfg.StateDir, "retention") }
func retentionReportPath(cfg Config, id string) string {
	return filepath.Join(retentionRoot(cfg), "reports", id+".json")
}
func recoveryManifestPath(cfg Config, id string) string {
	return filepath.Join(retentionRoot(cfg), "recovery", id+".json")
}
func recoveryKeyPath(cfg Config) string { return filepath.Join(retentionRoot(cfg), "recovery.key") }

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func retentionID(prefix string, data []byte) string {
	sum := sha256.Sum256(data)
	return prefix + "_" + hex.EncodeToString(sum[:8])
}

func buildRetentionReport(cfg Config, now time.Time, olderThanDays, recoveryDays int) (retentionReport, error) {
	if olderThanDays <= 0 {
		olderThanDays = retentionDefaultOlderDays
	}
	if recoveryDays <= 0 {
		recoveryDays = retentionDefaultRecoverDay
	}
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return retentionReport{}, err
	}
	cutoff := now.Add(-time.Duration(olderThanDays) * 24 * time.Hour)
	var candidates []retentionCandidate
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil || m.DeletedAt != "" {
			continue
		}
		created, err := time.Parse(time.RFC3339, m.CreatedAt)
		if err != nil || created.After(cutoff) {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return retentionReport{}, err
		}
		rel, err := filepath.Rel(cfg.VaultDir, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			return retentionReport{}, fmt.Errorf("retention candidate escapes vault: %s", path)
		}
		candidates = append(candidates, retentionCandidate{ID: m.ID, Path: filepath.ToSlash(rel), Type: m.Type, CreatedAt: m.CreatedAt, ContentSHA: sha256Hex(raw), SizeBytes: int64(len(raw))})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	stamp := now.UTC().Format(time.RFC3339)
	identity, _ := json.Marshal(struct {
		At         string               `json:"at"`
		Candidates []retentionCandidate `json:"candidates"`
	}{stamp, candidates})
	report := retentionReport{SchemaVersion: retentionSchemaVersion, ReportID: retentionID("ret", identity), CreatedAt: stamp, OlderThanDays: olderThanDays, RecoveryDays: recoveryDays, Candidates: candidates}
	if err := saveRetentionReport(cfg, report); err != nil {
		return retentionReport{}, err
	}
	return report, nil
}

func saveRetentionReport(cfg Config, report retentionReport) error {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(retentionReportPath(cfg, report.ReportID), append(b, '\n'), 0o600)
}

func loadRetentionReport(cfg Config, id string) (retentionReport, error) {
	b, err := os.ReadFile(retentionReportPath(cfg, id))
	if err != nil {
		return retentionReport{}, err
	}
	var report retentionReport
	if err := json.Unmarshal(b, &report); err != nil {
		return retentionReport{}, err
	}
	if report.SchemaVersion != retentionSchemaVersion || report.ReportID != id {
		return retentionReport{}, errors.New("unsupported or mismatched retention report")
	}
	return report, nil
}

func decideRetentionCandidate(cfg Config, reportID, memoryID string, decision retentionDecision) (retentionReport, error) {
	switch decision.Action {
	case "keep", "delete":
		if decision.Class != "" || decision.Summary != "" {
			return retentionReport{}, fmt.Errorf("%s decision accepts no class or summary", decision.Action)
		}
	case "change-class":
		if strings.TrimSpace(decision.Class) == "" {
			return retentionReport{}, errors.New("change-class requires class")
		}
	case "compact":
		if strings.TrimSpace(decision.Summary) == "" {
			return retentionReport{}, errors.New("compact requires summary")
		}
	default:
		return retentionReport{}, fmt.Errorf("unsupported retention decision %q", decision.Action)
	}
	report, err := loadRetentionReport(cfg, reportID)
	if err != nil {
		return retentionReport{}, err
	}
	found := false
	for i := range report.Candidates {
		if report.Candidates[i].ID == memoryID {
			report.Candidates[i].Decision = &decision
			found = true
			break
		}
	}
	if !found {
		return retentionReport{}, fmt.Errorf("candidate %q is not in report %q", memoryID, reportID)
	}
	return report, saveRetentionReport(cfg, report)
}

func recoveryKey(cfg Config) ([]byte, error) {
	path := recoveryKeyPath(cfg)
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, errors.New("retention recovery key has invalid length")
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := atomicio.Write(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func encryptRecoveryManifest(cfg Config, plain recoveryPlaintext) (encryptedRecoveryManifest, error) {
	key, err := recoveryKey(cfg)
	if err != nil {
		return encryptedRecoveryManifest{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return encryptedRecoveryManifest{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return encryptedRecoveryManifest{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return encryptedRecoveryManifest{}, err
	}
	b, err := json.Marshal(plain)
	if err != nil {
		return encryptedRecoveryManifest{}, err
	}
	sealed := gcm.Seal(nil, nonce, b, []byte(plain.ManifestID))
	return encryptedRecoveryManifest{SchemaVersion: retentionSchemaVersion, ManifestID: plain.ManifestID, ReportID: plain.ReportID, CreatedAt: plain.CreatedAt, ExpiresAt: plain.ExpiresAt, Nonce: base64.StdEncoding.EncodeToString(nonce), Ciphertext: base64.StdEncoding.EncodeToString(sealed)}, nil
}

func decryptRecoveryManifest(cfg Config, encrypted encryptedRecoveryManifest) (recoveryPlaintext, error) {
	key, err := recoveryKey(cfg)
	if err != nil {
		return recoveryPlaintext{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return recoveryPlaintext{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return recoveryPlaintext{}, err
	}
	nonce, err := base64.StdEncoding.DecodeString(encrypted.Nonce)
	if err != nil {
		return recoveryPlaintext{}, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(encrypted.Ciphertext)
	if err != nil {
		return recoveryPlaintext{}, err
	}
	b, err := gcm.Open(nil, nonce, ciphertext, []byte(encrypted.ManifestID))
	if err != nil {
		return recoveryPlaintext{}, err
	}
	var plain recoveryPlaintext
	if err := json.Unmarshal(b, &plain); err != nil {
		return recoveryPlaintext{}, err
	}
	if plain.ManifestID != encrypted.ManifestID || plain.ReportID != encrypted.ReportID {
		return recoveryPlaintext{}, errors.New("recovery manifest identity mismatch")
	}
	return plain, nil
}

func retentionCandidatePath(cfg Config, candidate retentionCandidate) (string, error) {
	path := filepath.Clean(filepath.Join(cfg.VaultDir, filepath.FromSlash(candidate.Path)))
	rel, err := filepath.Rel(cfg.VaultDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("retention path escapes vault: %q", candidate.Path)
	}
	return path, nil
}

func validateRetentionReport(cfg Config, report retentionReport) ([]string, error) {
	var targets []string
	for _, candidate := range report.Candidates {
		if candidate.Decision == nil {
			return nil, fmt.Errorf("candidate %q has no decision", candidate.ID)
		}
		path, err := retentionCandidatePath(cfg, candidate)
		if err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("candidate %q changed since report: %w", candidate.ID, err)
		}
		if sha256Hex(raw) != candidate.ContentSHA {
			return nil, fmt.Errorf("candidate %q changed since report; generate a new preview", candidate.ID)
		}
		if candidate.Decision.Action != "keep" {
			targets = append(targets, candidate.ID)
		}
	}
	sort.Strings(targets)
	return targets, nil
}

func writeEncryptedRecovery(cfg Config, plain recoveryPlaintext) error {
	encrypted, err := encryptRecoveryManifest(cfg, plain)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(encrypted, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(recoveryManifestPath(cfg, plain.ManifestID), append(b, '\n'), 0o600)
}

func executeRetentionReport(ctx context.Context, cfg Config, reportID string, now time.Time) (retentionExecutionReceipt, error) {
	report, err := loadRetentionReport(cfg, reportID)
	if err != nil {
		return retentionExecutionReceipt{}, err
	}
	targets, err := validateRetentionReport(cfg, report)
	if err != nil {
		return retentionExecutionReceipt{}, err
	}
	var entries []recoveryEntry
	for _, candidate := range report.Candidates {
		if candidate.Decision.Action == "keep" {
			continue
		}
		path, _ := retentionCandidatePath(cfg, candidate)
		raw, err := os.ReadFile(path)
		if err != nil {
			return retentionExecutionReceipt{}, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return retentionExecutionReceipt{}, err
		}
		entries = append(entries, recoveryEntry{ID: candidate.ID, Path: candidate.Path, Mode: uint32(info.Mode().Perm()), Data: raw, ContentSHA: candidate.ContentSHA})
	}
	stamp := now.UTC().Format(time.RFC3339)
	manifestIdentity, _ := json.Marshal(struct {
		ReportID string   `json:"report_id"`
		Targets  []string `json:"targets"`
		At       string   `json:"at"`
	}{report.ReportID, targets, stamp})
	manifestID := retentionID("recovery", manifestIdentity)
	plain := recoveryPlaintext{SchemaVersion: retentionSchemaVersion, ManifestID: manifestID, ReportID: report.ReportID, CreatedAt: stamp, ExpiresAt: now.Add(time.Duration(report.RecoveryDays) * 24 * time.Hour).UTC().Format(time.RFC3339), Entries: entries}
	if err := writeEncryptedRecovery(cfg, plain); err != nil {
		return retentionExecutionReceipt{}, err
	}
	receipt := retentionExecutionReceipt{SchemaVersion: retentionSchemaVersion, ReportID: report.ReportID, ManifestID: manifestID, TargetIDs: targets, ExecutedAt: stamp}
	var dirtyOps []pendingOp
	for _, candidate := range report.Candidates {
		decision := candidate.Decision
		if decision.Action == "keep" {
			receipt.KeptIDs = append(receipt.KeptIDs, candidate.ID)
			continue
		}
		path, _ := retentionCandidatePath(cfg, candidate)
		m, err := parseMemory(path)
		if err != nil {
			return retentionExecutionReceipt{}, err
		}
		op, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindDelete, Path: path, MemoryID: candidate.ID})
		if err != nil {
			return retentionExecutionReceipt{}, err
		}
		dirtyOps = append(dirtyOps, op)
		switch decision.Action {
		case "change-class":
			m.Type = strings.TrimSpace(decision.Class)
			b, err := renderMemory(m)
			if err != nil {
				return retentionExecutionReceipt{}, err
			}
			if err := atomicio.Write(path, b, 0o644); err != nil {
				return retentionExecutionReceipt{}, err
			}
			receipt.ChangedIDs = append(receipt.ChangedIDs, candidate.ID)
		case "compact":
			compactID := "retained_" + sha256Hex([]byte(report.ReportID + ":" + candidate.ID))[:16]
			compact := Memory{ID: compactID, Scope: m.Scope, Type: "durable", Title: "Retained: " + m.Title, Text: strings.TrimSpace(decision.Summary), Source: "retention:" + report.ReportID, CreatedAt: stamp, ContentHash: ContentHash(decision.Summary), Meta: map[string]any{"retention_source_ids": []string{candidate.ID}, "retention_manifest_id": manifestID}}
			if err := writeMemory(cfg, compact); err != nil {
				return retentionExecutionReceipt{}, err
			}
			if _, err := appendGovernanceEntry(cfg, govEntry{Kind: govKindPrune, Action: govActionSuppress, Atom: govAtom{Kind: atomStableID, Value: candidate.ID, Provider: m.Provider}, Reason: "retention compact " + report.ReportID}); err != nil {
				return retentionExecutionReceipt{}, err
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return retentionExecutionReceipt{}, err
			}
			receipt.CompactedIDs = append(receipt.CompactedIDs, candidate.ID)
		case "delete":
			if _, err := appendGovernanceEntry(cfg, govEntry{Kind: govKindPrune, Action: govActionSuppress, Atom: govAtom{Kind: atomStableID, Value: candidate.ID, Provider: m.Provider}, Reason: "retention delete " + report.ReportID}); err != nil {
				return retentionExecutionReceipt{}, err
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return retentionExecutionReceipt{}, err
			}
			receipt.DeletedIDs = append(receipt.DeletedIDs, candidate.ID)
		}
	}
	count, err := rebuildIndex(ctx, cfg)
	if err != nil {
		return retentionExecutionReceipt{}, err
	}
	for _, op := range dirtyOps {
		if err := unmarkIndexDirty(cfg, op.OpID); err != nil {
			return retentionExecutionReceipt{}, err
		}
	}
	receipt.IndexCount = count
	b, _ := json.MarshalIndent(receipt, "", "  ")
	if err := atomicio.Write(filepath.Join(retentionRoot(cfg), "audit", manifestID+".json"), append(b, '\n'), 0o600); err != nil {
		return retentionExecutionReceipt{}, err
	}
	return receipt, nil
}

func loadEncryptedRecovery(cfg Config, manifestID string) (encryptedRecoveryManifest, error) {
	b, err := os.ReadFile(recoveryManifestPath(cfg, manifestID))
	if err != nil {
		return encryptedRecoveryManifest{}, err
	}
	var encrypted encryptedRecoveryManifest
	if err := json.Unmarshal(b, &encrypted); err != nil {
		return encryptedRecoveryManifest{}, err
	}
	if encrypted.SchemaVersion != retentionSchemaVersion || encrypted.ManifestID != manifestID {
		return encryptedRecoveryManifest{}, errors.New("unsupported or mismatched recovery manifest")
	}
	return encrypted, nil
}

func verifyRetentionIntegrity(ctx context.Context, cfg Config) (retentionIntegrityReceipt, error) {
	receipt := retentionIntegrityReceipt{SchemaVersion: retentionSchemaVersion}
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return receipt, err
	}
	vaultIDs := map[string]bool{}
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil || m.DeletedAt != "" {
			continue
		}
		vaultIDs[m.ID] = true
	}
	receipt.VaultCount = len(vaultIDs)
	db, err := openIndexRO(ctx, cfg)
	if err != nil {
		return receipt, err
	}
	defer db.Close()
	indexIDs := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT id FROM memories ORDER BY id`)
	if err != nil {
		return receipt, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return receipt, err
		}
		indexIDs[id] = true
	}
	if err := rows.Close(); err != nil {
		return receipt, err
	}
	receipt.IndexCount = len(indexIDs)
	for id := range vaultIDs {
		if !indexIDs[id] {
			receipt.MissingIndexIDs = append(receipt.MissingIndexIDs, id)
		}
	}
	for id := range indexIDs {
		if !vaultIDs[id] {
			receipt.OrphanIndexIDs = append(receipt.OrphanIndexIDs, id)
		}
	}
	entityIDs := map[string]bool{}
	entityRows, err := db.QueryContext(ctx, `SELECT id FROM entities`)
	if err != nil {
		return receipt, err
	}
	for entityRows.Next() {
		var id string
		if err := entityRows.Scan(&id); err != nil {
			entityRows.Close()
			return receipt, err
		}
		entityIDs[id] = true
	}
	if err := entityRows.Close(); err != nil {
		return receipt, err
	}
	edges, err := db.QueryContext(ctx, `SELECT src, rel, dst, evidence_id FROM edges ORDER BY src, rel, dst, evidence_id`)
	if err != nil {
		return receipt, err
	}
	for edges.Next() {
		var src, rel, dst, evidence string
		if err := edges.Scan(&src, &rel, &dst, &evidence); err != nil {
			edges.Close()
			return receipt, err
		}
		if (!indexIDs[src] && !entityIDs[src]) || (!indexIDs[dst] && !entityIDs[dst]) || (evidence != "" && !indexIDs[evidence]) {
			receipt.DanglingGraphEdges = append(receipt.DanglingGraphEdges, src+"|"+rel+"|"+dst+"|"+evidence)
		}
	}
	if err := edges.Close(); err != nil {
		return receipt, err
	}
	sort.Strings(receipt.MissingIndexIDs)
	sort.Strings(receipt.OrphanIndexIDs)
	receipt.Healthy = len(receipt.MissingIndexIDs) == 0 && len(receipt.OrphanIndexIDs) == 0 && len(receipt.DanglingGraphEdges) == 0
	return receipt, nil
}

func recoverRetentionManifest(ctx context.Context, cfg Config, manifestID string, now time.Time) (retentionRecoveryReceipt, error) {
	encrypted, err := loadEncryptedRecovery(cfg, manifestID)
	if err != nil {
		return retentionRecoveryReceipt{}, err
	}
	plain, err := decryptRecoveryManifest(cfg, encrypted)
	if err != nil {
		return retentionRecoveryReceipt{}, err
	}
	expires, err := time.Parse(time.RFC3339, plain.ExpiresAt)
	if err != nil || now.After(expires) {
		return retentionRecoveryReceipt{}, fmt.Errorf("recovery window expired at %s", plain.ExpiresAt)
	}
	for _, entry := range plain.Entries {
		if sha256Hex(entry.Data) != entry.ContentSHA {
			return retentionRecoveryReceipt{}, fmt.Errorf("recovery entry %q failed integrity", entry.ID)
		}
		path := filepath.Clean(filepath.Join(cfg.VaultDir, filepath.FromSlash(entry.Path)))
		rel, err := filepath.Rel(cfg.VaultDir, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return retentionRecoveryReceipt{}, fmt.Errorf("recovery path escapes vault: %q", entry.Path)
		}
		if existing, err := os.ReadFile(path); err == nil && sha256Hex(existing) != entry.ContentSHA {
			return retentionRecoveryReceipt{}, fmt.Errorf("refusing to overwrite changed recovery target %q", entry.ID)
		}
	}
	for _, entry := range plain.Entries {
		path := filepath.Join(cfg.VaultDir, filepath.FromSlash(entry.Path))
		mode := os.FileMode(entry.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := atomicio.Write(path, entry.Data, mode); err != nil {
			return retentionRecoveryReceipt{}, err
		}
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		return retentionRecoveryReceipt{}, err
	}
	restoredSet := map[string]bool{}
	for _, entry := range plain.Entries {
		restoredSet[entry.ID] = true
	}
	for _, entry := range g.activeSuppress() {
		if entry.Kind == govKindPrune && entry.Atom.Kind == atomStableID && restoredSet[entry.Atom.Value] {
			if _, err := revokeGovernanceEntry(cfg, entry.ID); err != nil {
				return retentionRecoveryReceipt{}, err
			}
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return retentionRecoveryReceipt{}, err
	}
	integrity, err := verifyRetentionIntegrity(ctx, cfg)
	if err != nil {
		return retentionRecoveryReceipt{}, err
	}
	if !integrity.Healthy {
		return retentionRecoveryReceipt{}, fmt.Errorf("recovery integrity verification failed: %+v", integrity)
	}
	restored := make([]string, 0, len(plain.Entries))
	for _, entry := range plain.Entries {
		restored = append(restored, entry.ID)
	}
	sort.Strings(restored)
	return retentionRecoveryReceipt{SchemaVersion: retentionSchemaVersion, ManifestID: manifestID, RestoredIDs: restored, RecoveredAt: now.UTC().Format(time.RFC3339), Integrity: integrity}, nil
}
