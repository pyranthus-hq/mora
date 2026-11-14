package backupjob

import (
	"time"

	"github.com/pyranthus-hq/mora/internal/health"
)

// Classify projects local durable evidence only. It never probes a provider,
// performs a retry, or advances producer or alert state.
func Classify(target *Target, receipt *Receipt, producer *health.Producer, expected bool, leaseLive bool, now time.Time) Health {
	if target == nil && (receipt == nil || targetRequired(*receipt)) {
		return classified("", "unconfigured", "", "", "", "", expected, false, "No private backup target is selected.", "Select a private backup target.")
	}
	if target != nil && validateTarget(*target, now) != nil {
		return classified("", "failed", "", "", "destination_mismatch", "", false, false, safeMessage("destination_mismatch"), "Review the selected backup target.")
	}
	if receipt == nil {
		if !expected {
			return classified("", "healthy", target.ID, "", "", "", false, false, "", "")
		}
		return classified("", "stale", target.ID, "", "", "", true, true, "No confirmed backup success is recorded.", "Retry the registered backup job.")
	}
	targetID := receipt.TargetID
	if target != nil {
		targetID = target.ID
	}
	if validateReceiptIdentity(*receipt) != nil || !validReceiptTimes(*receipt, now) {
		return receiptProjection(classified(receipt.JobID, "failed", targetID, "", "receipt_unreadable", receipt.PolicyDigest, false, false, safeMessage("receipt_unreadable"), "Review the backup receipt."), *receipt)
	}
	if target != nil && receipt.TargetID != target.ID {
		return receiptProjection(classified(receipt.JobID, "failed", targetID, receipt.StartedAt, "destination_mismatch", receipt.PolicyDigest, false, false, safeMessage("destination_mismatch"), "Review the selected backup target."), *receipt)
	}
	switch receipt.State {
	case "running":
		if leaseLive {
			return receiptProjection(classified(receipt.JobID, "running", targetID, receipt.StartedAt, "", receipt.PolicyDigest, false, false, "Backup is currently running.", "Wait for the active backup run."), *receipt)
		}
		return receiptProjection(classified(receipt.JobID, "failed", targetID, receipt.StartedAt, "interrupted", receipt.PolicyDigest, true, true, safeMessage("interrupted"), "Recover the interrupted backup run with matching evidence."), *receipt)
	case "failed":
		code := safeFailureCode(receipt.FailureCode)
		if code == "offline" || code == "timeout" {
			return receiptProjection(classified(receipt.JobID, "offline", targetID, receipt.StartedAt, code, receipt.PolicyDigest, true, retryable(code), safeMessage(code), "Retry the registered backup job when connectivity returns."), *receipt)
		}
		return receiptProjection(classified(receipt.JobID, "failed", targetID, receipt.StartedAt, code, receipt.PolicyDigest, true, retryable(code), safeMessage(code), "Review the backup failure."), *receipt)
	case "diverged":
		return receiptProjection(classified(receipt.JobID, "diverged", targetID, receipt.StartedAt, "diverged", receipt.PolicyDigest, false, false, safeMessage("diverged"), "Reconcile preserved backup histories."), *receipt)
	case "succeeded":
		if receipt.LastSuccessAt == "" {
			return receiptProjection(classified(receipt.JobID, "failed", targetID, receipt.StartedAt, "receipt_unreadable", receipt.PolicyDigest, false, false, safeMessage("receipt_unreadable"), "Review the backup receipt."), *receipt)
		}
		if staleSuccess(receipt.LastSuccessAt, producer, expected, now) {
			return receiptProjection(classified(receipt.JobID, "stale", targetID, receipt.StartedAt, "", receipt.PolicyDigest, true, true, "Backup success is older than its configured cadence.", "Retry the registered backup job."), *receipt)
		}
		return receiptProjection(classified(receipt.JobID, "healthy", targetID, receipt.StartedAt, "", receipt.PolicyDigest, false, false, "", ""), *receipt)
	default:
		return receiptProjection(classified(receipt.JobID, "failed", targetID, receipt.StartedAt, "receipt_unreadable", receipt.PolicyDigest, false, false, safeMessage("receipt_unreadable"), "Review the backup receipt."), *receipt)
	}
}

func targetRequired(r Receipt) bool { return r.Operation == "push" || r.Operation == "isolated_pull" }

func classified(jobID, state, targetID, attempt, code, policy string, hasAttempt, retry bool, reason, action string) Health {
	h := Health{JobID: jobID, TargetID: targetID, State: state, FailureCode: code, PolicyDigest: policy, RetryEligible: retry, SafeReason: reason, NextAction: action}
	if hasAttempt {
		h.LastAttemptAt = attempt
	}
	return h
}

func receiptProjection(h Health, r Receipt) Health {
	h.LastSuccessAt = r.LastSuccessAt
	return h
}

func validReceiptTimes(r Receipt, now time.Time) bool {
	if !validTimestamp(r.StartedAt, now) {
		return false
	}
	if r.FinishedAt != "" && !validTimestamp(r.FinishedAt, now) {
		return false
	}
	return r.LastSuccessAt == "" || validTimestamp(r.LastSuccessAt, now)
}

func staleSuccess(success string, producer *health.Producer, expected bool, now time.Time) bool {
	if !expected || producer == nil || producer.IntervalSeconds <= 0 {
		return false
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
