package backupjob

import (
	"time"

	"github.com/pyranthus-hq/mora/internal/health"
)

// Inspect preserves load failures in the public health projection and returns
// their typed recovery evidence separately from the fixed safe display text.
func Inspect(stateDir string, expected JobSpec, target *Target, producer *health.Producer, now time.Time) (Health, error) {
	receipt, found, err := LoadReceipt(stateDir, expected.JobID)
	if err != nil {
		return healthFor(expected, "failed", "receipt_unreadable", false, safeMessage("receipt_unreadable"), "Review the backup receipt."), err
	}
	if !found {
		return Classify(stateDir, expected, target, nil, producer, now), nil
	}
	return Classify(stateDir, expected, target, &receipt, producer, now), nil
}

// Classify is side-effect-free. Running liveness comes from Begin's OS lease.
func Classify(stateDir string, expected JobSpec, target *Target, receipt *Receipt, producer *health.Producer, now time.Time) Health {
	if err := validateExpected(expected); err != nil {
		return healthFor(expected, "failed", "receipt_unreadable", false, "Backup job configuration cannot be trusted.", "Review the registered backup job.")
	}
	if targetRequired(expected) && target == nil {
		return healthFor(expected, "unconfigured", "", false, "No private backup target is selected.", "Select a private backup target.")
	}
	if targetRequired(expected) && target != nil && (validateTarget(*target, now) != nil || target.ID != expected.TargetID) {
		return healthFor(expected, "failed", "destination_mismatch", false, safeMessage("destination_mismatch"), "Review the selected backup target.")
	}
	if expected.TargetID == "" {
		return healthFor(expected, "failed", "destination_mismatch", false, safeMessage("destination_mismatch"), "Review the backup job target.")
	}
	if receipt == nil {
		return healthFor(expected, "stale", "", true, "No confirmed backup success is recorded.", "Retry the registered backup job.")
	}
	if validateReceipt(*receipt, now) != nil || receipt.JobID != expected.JobID || receipt.TargetID != expected.TargetID || receipt.PolicyDigest != expected.PolicyDigest || receipt.Operation != expected.Operation {
		h := healthFor(expected, "failed", "destination_mismatch", false, safeMessage("destination_mismatch"), "Review the backup receipt and target.")
		h.LastAttemptAt, h.LastSuccessAt = receipt.StartedAt, receipt.LastSuccessAt
		return h
	}
	h := healthFor(expected, "", "", false, "", "")
	h.LastAttemptAt, h.LastSuccessAt = receipt.StartedAt, receipt.LastSuccessAt
	switch receipt.State {
	case "running":
		live, err := LeaseLive(stateDir, expected.JobID)
		if err != nil {
			h.State, h.FailureCode, h.SafeReason, h.NextAction = "failed", "receipt_unreadable", safeMessage("receipt_unreadable"), "Review the backup receipt."
			return h
		}
		if live {
			h.State, h.SafeReason, h.NextAction = "running", "Backup is currently running.", "Wait for the active backup run."
			return h
		}
		h.State, h.FailureCode, h.SafeReason, h.NextAction, h.RetryEligible = "failed", "interrupted", safeMessage("interrupted"), "Recover the interrupted backup run with matching evidence.", true
		return h
	case "failed":
		code := safeFailureCode(receipt.FailureCode)
		h.FailureCode, h.RetryEligible, h.SafeReason = code, retryable(code), safeMessage(code)
		if code == "offline" || code == "timeout" {
			h.State, h.NextAction = "offline", "Retry the registered backup job when connectivity returns."
		} else {
			h.State, h.NextAction = "failed", "Review the backup failure."
		}
		return h
	case "diverged":
		h.State, h.FailureCode, h.SafeReason, h.NextAction = "diverged", "diverged", safeMessage("diverged"), "Reconcile preserved backup histories."
		return h
	case "succeeded":
		if staleSuccess(receipt.LastSuccessAt, producer, now) {
			h.State, h.RetryEligible, h.SafeReason, h.NextAction = "stale", true, "Backup success is older than its configured cadence.", "Retry the registered backup job."
			return h
		}
		h.State = "healthy"
		return h
	default:
		h.State, h.FailureCode, h.SafeReason, h.NextAction = "failed", "receipt_unreadable", safeMessage("receipt_unreadable"), "Review the backup receipt."
		return h
	}
}

func targetRequired(spec JobSpec) bool {
	return spec.Operation == "push" || spec.Operation == "isolated_pull"
}
func healthFor(spec JobSpec, state, code string, retry bool, reason, action string) Health {
	return Health{JobID: spec.JobID, TargetID: spec.TargetID, State: state, FailureCode: code, PolicyDigest: spec.PolicyDigest, RetryEligible: retry, SafeReason: reason, NextAction: action}
}
func staleSuccess(success string, producer *health.Producer, now time.Time) bool {
	if producer == nil || producer.IntervalSeconds <= 0 {
		return true
	}
	when, err := time.Parse(time.RFC3339, success)
	if err != nil || when.After(now.UTC()) {
		return true
	}
	if producer.State == health.ProducerFailed || producer.State == health.ProducerStale || producer.State == health.ProducerNever {
		return true
	}
	return now.UTC().Sub(when.UTC()) >= 2*time.Duration(producer.IntervalSeconds)*time.Second
}
