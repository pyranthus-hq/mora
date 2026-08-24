package mora

import (
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
