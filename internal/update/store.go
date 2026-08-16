// Package update owns strict durable evidence for update checks and applications.
package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/pyranthus-hq/mora/internal/atomicio"
)

const (
	SchemaVersion   = 1
	MaxReceiptBytes = 64 << 10
	ClockSkew       = 5 * time.Minute
)

type Store struct {
	StateDir string
	Now      func() time.Time
}

func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

type Receipt struct {
	SchemaVersion         int    `json:"schema_version"`
	LastAttemptAt         string `json:"last_attempt_at,omitempty"`
	LastSuccessAt         string `json:"last_success_at,omitempty"`
	LastErrorCode         string `json:"last_error_code,omitempty"`
	LatestVersion         string `json:"latest_version,omitempty"`
	UpdateAvailable       bool   `json:"update_available"`
	LastNotifiedAt        string `json:"last_notified_at,omitempty"`
	LastNotifiedVersion   string `json:"last_notified_version,omitempty"`
	NotificationErrorCode string `json:"notification_error_code,omitempty"`
	ApplyVersion          string `json:"apply_version,omitempty"`
	ApplyAttemptAt        string `json:"apply_attempt_at,omitempty"`
	AppliedAt             string `json:"applied_at,omitempty"`
	ApplyOutcome          string `json:"apply_outcome,omitempty"`
	ApplyErrorCode        string `json:"apply_error_code,omitempty"`
	RollbackOutcome       string `json:"rollback_outcome,omitempty"`
	RebuildOutcome        string `json:"rebuild_outcome,omitempty"`
}

func (s Store) Path() string { return filepath.Join(s.StateDir, "update", "status.json") }
func (s Store) Load() (Receipt, error) {
	var receipt Receipt
	body, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return Receipt{SchemaVersion: SchemaVersion}, nil
	}
	if err != nil {
		return receipt, err
	}
	if len(body) > MaxReceiptBytes {
		return receipt, fmt.Errorf("update status exceeds %d bytes", MaxReceiptBytes)
	}
	if err := rejectDuplicateReceiptKeys(body); err != nil {
		return receipt, fmt.Errorf("reading update status: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, fmt.Errorf("reading update status: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return receipt, fmt.Errorf("reading update status: multiple JSON values")
		}
		return receipt, fmt.Errorf("reading update status: %w", err)
	}
	if err := validateReceipt(receipt, s.now()); err != nil {
		return receipt, fmt.Errorf("reading update status: %w", err)
	}
	return receipt, nil
}
func rejectDuplicateReceiptKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("update status must be one JSON object")
	}
	seen := map[string]struct{}{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("update status contains a non-string key")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("update status contains duplicate field %q", key)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := closing.(json.Delim); !ok || delim != '}' {
		return fmt.Errorf("update status has an invalid object terminator")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("update status contains multiple JSON values")
		}
		return err
	}
	return nil
}
func (s Store) Save(receipt Receipt) error {
	receipt.SchemaVersion = SchemaVersion
	if err := validateReceipt(receipt, s.now()); err != nil {
		return fmt.Errorf("refusing invalid update status: %w", err)
	}
	body, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if len(body) > MaxReceiptBytes {
		return fmt.Errorf("update status exceeds %d bytes", MaxReceiptBytes)
	}
	return atomicio.WriteDurable(s.Path(), body, 0o600)
}
func validateReceipt(receipt Receipt, now time.Time) error {
	if receipt.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema %d", receipt.SchemaVersion)
	}
	codes := []struct {
		field, value, allowed string
	}{
		{"last_error_code", receipt.LastErrorCode, "check_failed"},
		{"notification_error_code", receipt.NotificationErrorCode, "notification_failed"},
	}
	for _, code := range codes {
		if code.value != "" && code.value != code.allowed {
			return fmt.Errorf("%s contains unknown code %q", code.field, code.value)
		}
	}
	if !oneOf(receipt.ApplyErrorCode, "", "unsafe_health", "app_unwritable", "not_verified_app", "installed_verify_failed", "download_failed", "staged_verify_failed", "state_changed", "swap_failed", "post_swap_verify_failed", "rebuild_failed", "post_health_failed", "rollback_rebuild_failed", "failpoint") {
		return fmt.Errorf("apply_error_code contains unknown code %q", receipt.ApplyErrorCode)
	}
	if !oneOf(receipt.ApplyOutcome, "", "in_progress", "deferred", "failed_before_swap", "updated", "rolled_back", "rollback_failed") {
		return fmt.Errorf("apply_outcome contains unknown code %q", receipt.ApplyOutcome)
	}
	if !oneOf(receipt.RollbackOutcome, "", "not_needed", "succeeded", "failed") {
		return fmt.Errorf("rollback_outcome contains unknown code %q", receipt.RollbackOutcome)
	}
	if !oneOf(receipt.RebuildOutcome, "", "not_run", "not_needed", "succeeded", "failed") {
		return fmt.Errorf("rebuild_outcome contains unknown code %q", receipt.RebuildOutcome)
	}
	versions := []struct{ field, value string }{
		{"latest_version", receipt.LatestVersion},
		{"last_notified_version", receipt.LastNotifiedVersion},
		{"apply_version", receipt.ApplyVersion},
	}
	for _, version := range versions {
		if version.value != "" && !CanonicalStableVersion(version.value) {
			return fmt.Errorf("%s %q is not canonical stable semver", version.field, version.value)
		}
	}
	if receipt.UpdateAvailable && receipt.LatestVersion == "" {
		return fmt.Errorf("update_available requires latest_version")
	}
	if receipt.LatestVersion != "" && receipt.LastSuccessAt == "" {
		return fmt.Errorf("latest_version requires last_success_at")
	}
	if receipt.LastSuccessAt != "" && receipt.LastAttemptAt == "" {
		return fmt.Errorf("last_success_at requires last_attempt_at")
	}
	if receipt.LastErrorCode != "" && receipt.LastAttemptAt == "" {
		return fmt.Errorf("last_error_code requires last_attempt_at")
	}
	if (receipt.LastNotifiedAt == "") != (receipt.LastNotifiedVersion == "") {
		return fmt.Errorf("last_notified_at and last_notified_version must appear together")
	}
	if receipt.NotificationErrorCode != "" && !receipt.UpdateAvailable && receipt.ApplyOutcome != "updated" {
		return fmt.Errorf("notification_error_code requires an available update or completed apply")
	}
	applyAny := receipt.ApplyVersion != "" || receipt.ApplyAttemptAt != "" || receipt.AppliedAt != "" || receipt.ApplyOutcome != "" || receipt.ApplyErrorCode != "" || receipt.RollbackOutcome != "" || receipt.RebuildOutcome != ""
	if applyAny && (receipt.ApplyVersion == "" || receipt.ApplyAttemptAt == "" || receipt.ApplyOutcome == "") {
		return fmt.Errorf("apply evidence requires version, attempt timestamp, and outcome")
	}
	switch receipt.ApplyOutcome {
	case "":
	case "in_progress":
		if receipt.ApplyErrorCode != "" || receipt.RollbackOutcome != "" || receipt.RebuildOutcome != "" || receipt.AppliedAt != "" {
			return fmt.Errorf("in_progress outcome cannot claim later-stage evidence")
		}
	case "deferred":
		if !oneOf(receipt.ApplyErrorCode, "unsafe_health", "app_unwritable", "not_verified_app") || receipt.RollbackOutcome != "not_needed" || receipt.RebuildOutcome != "not_run" || receipt.AppliedAt != "" {
			return fmt.Errorf("deferred outcome has incompatible evidence")
		}
	case "failed_before_swap":
		if !oneOf(receipt.ApplyErrorCode, "not_verified_app", "installed_verify_failed", "download_failed", "staged_verify_failed", "state_changed", "swap_failed", "failpoint") || receipt.RollbackOutcome != "not_needed" || receipt.RebuildOutcome != "not_run" || receipt.AppliedAt != "" {
			return fmt.Errorf("failed_before_swap outcome has incompatible evidence")
		}
	case "updated":
		if receipt.AppliedAt == "" || receipt.ApplyErrorCode != "" || receipt.RollbackOutcome != "not_needed" || !oneOf(receipt.RebuildOutcome, "not_needed", "succeeded") {
			return fmt.Errorf("updated outcome has incompatible evidence")
		}
	case "rolled_back":
		if !oneOf(receipt.ApplyErrorCode, "post_swap_verify_failed", "rebuild_failed", "post_health_failed", "failpoint") || receipt.RollbackOutcome != "succeeded" || !oneOf(receipt.RebuildOutcome, "not_run", "not_needed", "failed", "succeeded") || receipt.AppliedAt != "" {
			return fmt.Errorf("rolled_back outcome has incompatible evidence")
		}
	case "rollback_failed":
		if !oneOf(receipt.ApplyErrorCode, "post_swap_verify_failed", "rebuild_failed", "post_health_failed", "rollback_rebuild_failed", "failpoint") || receipt.RollbackOutcome != "failed" || !oneOf(receipt.RebuildOutcome, "not_run", "not_needed", "failed", "succeeded") || receipt.AppliedAt != "" {
			return fmt.Errorf("rollback_failed outcome has incompatible evidence")
		}
	}

	timestamps := []struct{ field, value string }{
		{"last_attempt_at", receipt.LastAttemptAt},
		{"last_success_at", receipt.LastSuccessAt},
		{"last_notified_at", receipt.LastNotifiedAt},
		{"apply_attempt_at", receipt.ApplyAttemptAt},
		{"applied_at", receipt.AppliedAt},
	}
	parsed := make(map[string]time.Time, len(timestamps))
	for _, timestamp := range timestamps {
		if timestamp.value == "" {
			continue
		}
		value, err := time.Parse(time.RFC3339, timestamp.value)
		if err != nil {
			return fmt.Errorf("%s is not RFC3339", timestamp.field)
		}
		if value.After(now.Add(ClockSkew)) {
			return fmt.Errorf("%s is too far in the future", timestamp.field)
		}
		parsed[timestamp.field] = value
	}
	if success, ok := parsed["last_success_at"]; ok {
		if attempt := parsed["last_attempt_at"]; success.After(attempt) {
			return fmt.Errorf("last_success_at is after last_attempt_at")
		}
	}
	if notified, ok := parsed["last_notified_at"]; ok {
		if receipt.ApplyOutcome == "updated" && receipt.LastNotifiedVersion == receipt.ApplyVersion {
			if applied, present := parsed["applied_at"]; !present || notified.Before(applied) {
				return fmt.Errorf("post-apply last_notified_at must be at or after applied_at")
			}
		} else if attempt, present := parsed["last_attempt_at"]; !present || notified.After(attempt) {
			return fmt.Errorf("last_notified_at requires an equal or later last_attempt_at")
		}
	}
	if applied, ok := parsed["applied_at"]; ok {
		if attempt := parsed["apply_attempt_at"]; applied.Before(attempt) {
			return fmt.Errorf("applied_at is before apply_attempt_at")
		}
	}
	return nil
}
func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
func CanonicalStableVersion(version string) bool {
	parsed, err := semver.StrictNewVersion(version)
	return err == nil && parsed.Prerelease() == "" && parsed.Metadata() == "" && parsed.String() == version
}
