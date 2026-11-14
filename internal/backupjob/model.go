// Package backupjob owns durable, target-bound backup job receipts.
// It deliberately contains no provider, process, network, or UI authority.
package backupjob

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const schema = 1

var (
	ErrRunning       = errors.New("backup job is already running")
	ErrOwnershipLost = errors.New("backup job receipt ownership lost")
)

// Target is the selected, already-authorized private backup destination. It
// carries no credential and is inert until a higher-level adapter authorizes use.
type Target struct {
	Schema              int      `json:"schema"`
	ID                  string   `json:"id"`
	Provider            string   `json:"provider"`
	Repository          string   `json:"repository"`
	RemoteDigest        string   `json:"remote_digest"`
	Visibility          string   `json:"visibility"`
	SelectedAt          string   `json:"selected_at"`
	PrivateVerifiedAt   string   `json:"private_verified_at"`
	AuthorizedReaderIDs []string `json:"authorized_reader_ids"`
}

// Receipt is the durable record for exactly one job/target/policy run.
type Receipt struct {
	Schema        int    `json:"schema"`
	JobID         string `json:"job_id"`
	TargetID      string `json:"target_id"`
	PolicyDigest  string `json:"policy_digest"`
	RunID         string `json:"run_id"`
	Operation     string `json:"operation"`
	State         string `json:"state"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	FailureCode   string `json:"failure_code,omitempty"`
	SafeDetail    string `json:"safe_detail,omitempty"`
	LocalOID      string `json:"local_oid,omitempty"`
	RemoteOID     string `json:"remote_oid,omitempty"`
}

// JobSpec is the private input to Begin. RecoverRunID and RecoverStartedAt are
// explicit stale-run evidence; a running receipt is never replaced without both.
type JobSpec struct {
	JobID, TargetID, PolicyDigest, RunID, Operation string
	RecoverRunID, RecoverStartedAt                  string
}

// Result has only terminal states. Details are always selected from fixed codes.
type Result struct {
	State, FailureCode, LocalOID, RemoteOID string
}

// Health is a read-only projection. PolicyDigest is retained solely so alert
// recovery can prove that a success applies to the same policy.
type Health struct {
	JobID, TargetID, State              string
	LastAttemptAt, LastSuccessAt        string
	FailureCode, SafeReason, NextAction string
	EpisodeID, PolicyDigest             string
	RetryEligible                       bool
}

// Alert is the current durable failure episode for one job.
type Alert struct {
	JobID, TargetID, EpisodeID, Fingerprint string
	FirstSeenAt, LastSeenAt, RecoveredAt    string
	NotificationID, DeliveryState           string
	PolicyDigest, FailureAttemptAt          string
	Sequence                                int
}

func targetPath(configDir string) string { return filepath.Join(configDir, "backup-target.json") }
func receiptPath(stateDir, jobID string) string {
	return filepath.Join(stateDir, "backup", "jobs", jobID+".json")
}
func alertPath(stateDir, jobID string) string {
	return filepath.Join(stateDir, "backup", "alerts", jobID+".json")
}

func safeComponent(v string) bool {
	if v == "" || v == "." || v == ".." || strings.ContainsAny(v, "/\\\x00") || strings.TrimSpace(v) != v || strings.TrimRight(v, ". ") != v {
		return false
	}
	upper := strings.ToUpper(strings.Split(v, ".")[0])
	switch upper {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return false
	}
	return true
}

func validTimestamp(v string, now time.Time) bool {
	if v == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, v)
	return err == nil && !t.After(now.UTC())
}

func validateTarget(t Target, now time.Time) error {
	if t.Schema != schema || !safeComponent(t.ID) || t.Provider != "github" || t.Visibility != "private" {
		return errors.New("invalid backup target")
	}
	parts := strings.Split(t.Repository, "/")
	if len(parts) != 2 || !safeComponent(parts[0]) || !safeComponent(parts[1]) || strings.Contains(t.Repository, ":") || strings.Contains(t.Repository, "@") || !safeComponent(t.RemoteDigest) {
		return errors.New("invalid backup target identity")
	}
	if !validTimestamp(t.SelectedAt, now) || !validTimestamp(t.PrivateVerifiedAt, now) {
		return errors.New("invalid backup target verification time")
	}
	seen := map[string]struct{}{}
	for _, reader := range t.AuthorizedReaderIDs {
		if !safeComponent(reader) {
			return errors.New("invalid backup target reader")
		}
		if _, exists := seen[reader]; exists {
			return errors.New("duplicate backup target reader")
		}
		seen[reader] = struct{}{}
	}
	return nil
}

func validateSpec(spec JobSpec) error {
	if !safeComponent(spec.JobID) || !safeComponent(spec.TargetID) || !safeComponent(spec.PolicyDigest) || !safeComponent(spec.RunID) {
		return errors.New("invalid backup job identity")
	}
	switch spec.Operation {
	case "push", "isolated_pull", "local_archive", "share_pull", "share_push":
	default:
		return errors.New("invalid backup operation")
	}
	if (spec.RecoverRunID == "") != (spec.RecoverStartedAt == "") || (spec.RecoverRunID != "" && !safeComponent(spec.RecoverRunID)) {
		return errors.New("invalid interrupted-run recovery evidence")
	}
	return nil
}

func newRunID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate backup run id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func safeMessage(code string) string {
	switch code {
	case "offline":
		return "Backup destination is unreachable."
	case "timeout":
		return "Backup attempt timed out."
	case "auth_required":
		return "Backup authorization is required."
	case "destination_mismatch":
		return "Selected backup destination does not match this job."
	case "diverged":
		return "Backup histories diverged and require reconciliation."
	case "sensitive_tracked":
		return "Backup was blocked because tracked sensitive data was detected."
	case "repo_corrupt":
		return "Backup repository is unavailable."
	case "interrupted":
		return "Previous backup attempt was interrupted."
	case "receipt_unreadable":
		return "Backup receipt cannot be trusted."
	default:
		return "Backup could not complete."
	}
}

func safeFailureCode(code string) string {
	switch code {
	case "offline", "timeout", "auth_required", "destination_mismatch", "diverged", "sensitive_tracked", "repo_corrupt", "interrupted", "receipt_unreadable":
		return code
	default:
		return "unknown"
	}
}

func retryable(code string) bool {
	return code == "offline" || code == "timeout" || code == "interrupted"
}
