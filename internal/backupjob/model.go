// Package backupjob owns durable, target-bound backup job receipts.
// It deliberately contains no provider, process, network, or UI authority.
package backupjob

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	schema         = 1
	maxID          = 128
	maxReaderID    = 64
	maxDigest      = 128
	maxReaders     = 1024
	maxRecordBytes = 128 * 1024
)

var (
	ErrRunning           = errors.New("backup job is already running")
	ErrOwnershipLost     = errors.New("backup job receipt ownership lost")
	ErrReceiptUnreadable = errors.New("backup receipt is unreadable")
	ErrAlertUnreadable   = errors.New("backup alert is unreadable")
)

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

// JobSpec is both the expected job identity for Classify and the input to Begin.
type JobSpec struct {
	JobID, TargetID, PolicyDigest, RunID, Operation string
	RecoverRunID, RecoverStartedAt                  string
	RecoverReceiptDigest                            string
}

type Result struct{ State, FailureCode, LocalOID, RemoteOID string }

// Run is the authority returned by Begin. Its lock remains held for the whole
// external operation; Close only releases that lock and never fabricates success.
type Run struct {
	Receipt  Receipt
	stateDir string
	lock     *os.File
	mu       sync.Mutex
}

func (r *Run) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeLocked()
}

func (r *Run) closeLocked() error {
	if r.lock == nil {
		return nil
	}
	lock := r.lock
	r.lock = nil
	if err := unlockRun(lock); err != nil {
		_ = lock.Close()
		return err
	}
	return lock.Close()
}

type Health struct {
	JobID, TargetID, State              string
	LastAttemptAt, LastSuccessAt        string
	FailureCode, SafeReason, NextAction string
	EpisodeID, PolicyDigest             string
	RetryEligible                       bool
}

type Alert struct {
	JobID, TargetID, EpisodeID, Fingerprint string
	FirstSeenAt, LastSeenAt, RecoveredAt    string
	NotificationID, DeliveryState           string
	PolicyDigest, FailureAttemptAt          string
	State, FailureCode                      string
	Sequence                                int
}

// UnreadableError exposes observed-byte evidence for explicit local recovery.
// An empty Digest means the file exceeded the bounded reader and needs local inspection.
type UnreadableError struct{ Kind, Digest string }

func (e *UnreadableError) Error() string { return e.Kind + " is unreadable" }
func (e *UnreadableError) Is(target error) bool {
	return (e.Kind == "backup receipt" && target == ErrReceiptUnreadable) || (e.Kind == "backup alert" && target == ErrAlertUnreadable)
}
func unreadableDigest(err error) string {
	var bad *UnreadableError
	if errors.As(err, &bad) {
		return bad.Digest
	}
	return ""
}

func safeToken(v string, max int) bool {
	if v == "" || len(v) > max || v == "." || v == ".." || strings.TrimSpace(v) != v || strings.TrimRight(v, ". ") != v {
		return false
	}
	for _, r := range v {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	switch strings.ToUpper(strings.Split(v, ".")[0]) {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$", "CONIN$", "CONOUT$", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return false
	}
	return true
}

func validJobID(v string) bool {
	if v == "vault-git" || v == "vault-git-pull" || v == "local-archive" {
		return true
	}
	for _, prefix := range []string{"share-pull:", "share-push:"} {
		if name, found := strings.CutPrefix(v, prefix); found {
			return safeToken(name, maxReaderID)
		}
	}
	return false
}
func validJobOperation(id, operation string) bool {
	switch {
	case id == "vault-git":
		return operation == "push"
	case id == "vault-git-pull":
		return operation == "isolated_pull"
	case id == "local-archive":
		return operation == "local_archive"
	case strings.HasPrefix(id, "share-pull:"):
		return operation == "share_pull"
	case strings.HasPrefix(id, "share-push:"):
		return operation == "share_push"
	}
	return false
}
func validTimestamp(v string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, v)
	return v != "" && err == nil && !t.After(now.UTC())
}
func parseTimestamp(v string) (time.Time, error) { return time.Parse(time.RFC3339, v) }

func validateTarget(t Target, now time.Time) error {
	if t.Schema != schema || !safeToken(t.ID, maxID) || t.Provider != "github" || t.Visibility != "private" || !safeToken(t.RemoteDigest, maxDigest) || !validTimestamp(t.SelectedAt, now) || !validTimestamp(t.PrivateVerifiedAt, now) {
		return errors.New("invalid backup target")
	}
	parts := strings.Split(t.Repository, "/")
	if len(parts) != 2 || !safeToken(parts[0], maxID) || !safeToken(parts[1], maxID) {
		return errors.New("invalid backup target repository")
	}
	if len(t.AuthorizedReaderIDs) > maxReaders {
		return errors.New("too many backup target readers")
	}
	seen := map[string]bool{}
	for _, reader := range t.AuthorizedReaderIDs {
		if !safeToken(reader, maxReaderID) || seen[reader] {
			return errors.New("invalid backup target reader")
		}
		seen[reader] = true
	}
	return nil
}

func validateExpected(spec JobSpec) error {
	if !validJobID(spec.JobID) || (spec.TargetID != "" && !safeToken(spec.TargetID, maxID)) || !safeToken(spec.PolicyDigest, maxDigest) || !validJobOperation(spec.JobID, spec.Operation) || (spec.RunID != "" && !safeToken(spec.RunID, maxID)) {
		return errors.New("invalid backup job identity")
	}
	if (spec.RecoverRunID == "") != (spec.RecoverStartedAt == "") || (spec.RecoverRunID != "" && !safeToken(spec.RecoverRunID, maxID)) || (spec.RecoverReceiptDigest != "" && !safeToken(spec.RecoverReceiptDigest, maxDigest)) {
		return errors.New("invalid backup recovery evidence")
	}
	return nil
}

func validateReceipt(r Receipt, now time.Time) error {
	if r.Schema != schema || !validJobID(r.JobID) || !safeToken(r.TargetID, maxID) || !safeToken(r.PolicyDigest, maxDigest) || !safeToken(r.RunID, maxID) || !validJobOperation(r.JobID, r.Operation) || !validTimestamp(r.StartedAt, now) || (r.LocalOID != "" && !safeToken(r.LocalOID, maxDigest)) || (r.RemoteOID != "" && !safeToken(r.RemoteOID, maxDigest)) {
		return errors.New("invalid backup receipt identity")
	}
	start, _ := parseTimestamp(r.StartedAt)
	if r.LastSuccessAt != "" && !validTimestamp(r.LastSuccessAt, now) {
		return errors.New("invalid backup receipt success time")
	}
	lastSuccess := time.Time{}
	if r.LastSuccessAt != "" {
		lastSuccess, _ = parseTimestamp(r.LastSuccessAt)
	}
	switch r.State {
	case "running":
		if r.FinishedAt != "" || r.FailureCode != "" || r.SafeDetail != "" || (!lastSuccess.IsZero() && lastSuccess.After(start)) {
			return errors.New("invalid running receipt")
		}
	case "succeeded":
		if !validTimestamp(r.FinishedAt, now) || !validTimestamp(r.LastSuccessAt, now) || r.FailureCode != "" || r.SafeDetail != "" {
			return errors.New("invalid success receipt")
		}
		finished, _ := parseTimestamp(r.FinishedAt)
		success, _ := parseTimestamp(r.LastSuccessAt)
		if finished.Before(start) || !finished.Equal(success) {
			return errors.New("invalid success chronology")
		}
	case "failed", "diverged":
		if !validTimestamp(r.FinishedAt, now) || safeFailureCode(r.FailureCode) != r.FailureCode || r.SafeDetail != safeMessage(r.FailureCode) {
			return errors.New("invalid failure receipt")
		}
		finished, _ := parseTimestamp(r.FinishedAt)
		if finished.Before(start) || (!lastSuccess.IsZero() && lastSuccess.After(start)) {
			return errors.New("invalid failure chronology")
		}
	default:
		return errors.New("invalid receipt state")
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
	case "offline", "timeout", "auth_required", "destination_mismatch", "diverged", "sensitive_tracked", "repo_corrupt", "interrupted", "receipt_unreadable", "unknown":
		return code
	}
	return "unknown"
}
func retryable(code string) bool {
	return code == "offline" || code == "timeout" || code == "interrupted"
}
