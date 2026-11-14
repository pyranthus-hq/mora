package backupjob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/pyranthus-hq/mora/internal/leasefile"
)

// AdvanceAlert claims an episode, but returns it for delivery again until an
// explicit delivery receipt is recorded with RecordAlertDelivery.
func AdvanceAlert(stateDir string, health Health, now time.Time) (Alert, bool, error) {
	if !validJobID(health.JobID) || !safeToken(health.PolicyDigest, maxDigest) || (health.TargetID != "" && !safeToken(health.TargetID, maxID)) {
		return Alert{}, false, errors.New("invalid backup alert identity")
	}
	path, err := preparePath(stateDir, alertPath(stateDir, health.JobID))
	if err != nil {
		return Alert{}, false, err
	}
	var out Alert
	var notify bool
	err = leasefile.WithGuard(path, func() error {
		prior, found, err := loadAlert(path, health.JobID)
		if err != nil {
			return err
		}
		if health.State == "healthy" {
			if found && prior.RecoveredAt == "" && sameAlertScope(prior, health) && successAfter(health.LastSuccessAt, prior.FailureAttemptAt, now) {
				prior.RecoveredAt, prior.LastSeenAt = now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano)
				if !validAlert(prior, health.JobID, now) {
					return errors.New("invalid backup alert chronology")
				}
				if err := writeDurableJSON(path, prior); err != nil {
					return err
				}
			}
			out = prior
			return nil
		}
		if !alertable(health) {
			out = prior
			return nil
		}
		fingerprint := fingerprintFor(health)
		if found && prior.Fingerprint == fingerprint && prior.RecoveredAt == "" {
			prior.LastSeenAt = now.UTC().Format(time.RFC3339Nano)
			if successAfter(health.LastAttemptAt, prior.FailureAttemptAt, now) {
				prior.FailureAttemptAt = health.LastAttemptAt
			}
			if !validAlert(prior, health.JobID, now) {
				return errors.New("invalid backup alert chronology")
			}
			if err := writeDurableJSON(path, prior); err != nil {
				return err
			}
			out, notify = prior, prior.DeliveryState != "delivered"
			return nil
		}
		sequence := 1
		if found {
			sequence = prior.Sequence + 1
		}
		stamp := now.UTC().Format(time.RFC3339Nano)
		attempt := health.LastAttemptAt
		if attempt == "" {
			attempt = stamp
		}
		out = Alert{JobID: health.JobID, TargetID: health.TargetID, Fingerprint: fingerprint, PolicyDigest: health.PolicyDigest, FailureAttemptAt: attempt, FirstSeenAt: stamp, LastSeenAt: stamp, State: health.State, FailureCode: safeFailureCode(health.FailureCode), Sequence: sequence, DeliveryState: "unknown"}
		id, err := newRunID()
		if err != nil {
			return err
		}
		out.EpisodeID = "backup-" + id
		out.NotificationID = out.EpisodeID
		if !validAlert(out, health.JobID, now) {
			return errors.New("invalid backup alert")
		}
		if err := writeDurableJSON(path, out); err != nil {
			return err
		}
		notify = true
		return nil
	})
	return out, notify, err
}

// RecordAlertDelivery is a compare-and-set acknowledgement; a stale platform
// ack cannot mark a newer episode delivered.
func RecordAlertDelivery(stateDir, jobID, episodeID string, now time.Time) error {
	if !validJobID(jobID) || episodeID == "" {
		return errors.New("invalid backup alert delivery receipt")
	}
	path, err := checkedPath(stateDir, alertPath(stateDir, jobID))
	if err != nil {
		return err
	}
	return leasefile.WithGuard(path, func() error {
		alert, found, err := loadAlert(path, jobID)
		if err != nil {
			return err
		}
		if !found || alert.EpisodeID != episodeID || alert.RecoveredAt != "" {
			return ErrOwnershipLost
		}
		alert.DeliveryState, alert.LastSeenAt = "delivered", now.UTC().Format(time.RFC3339Nano)
		if !validAlert(alert, jobID, now) {
			return errors.New("invalid backup alert chronology")
		}
		return writeDurableJSON(path, alert)
	})
}

// RecoverAlert quarantines a corrupt owned alert only when the caller presents
// the digest of the observed bytes. It never silently resets notification state.
func RecoverAlert(stateDir, jobID, observedDigest string) error {
	if !validJobID(jobID) || !safeToken(observedDigest, maxDigest) {
		return errors.New("invalid backup alert recovery evidence")
	}
	path, err := checkedPath(stateDir, alertPath(stateDir, jobID))
	if err != nil {
		return err
	}
	return leasefile.WithGuard(path, func() error {
		_, _, loadErr := loadAlert(path, jobID)
		if !errors.Is(loadErr, ErrAlertUnreadable) {
			if loadErr == nil {
				return errors.New("backup alert is not corrupt")
			}
			return loadErr
		}
		if digest := unreadableDigest(loadErr); digest != observedDigest {
			return errors.New("backup alert recovery evidence does not match")
		}
		return quarantine(path, observedDigest)
	})
}

func loadAlert(path, expectedJobID string) (Alert, bool, error) {
	b, err := readRecord(path, "backup alert")
	if errors.Is(err, os.ErrNotExist) {
		return Alert{}, false, nil
	}
	if err != nil {
		return Alert{}, false, errors.Join(errors.New("read backup alert"), err)
	}
	var alert Alert
	if json.Unmarshal(b, &alert) != nil || !validAlert(alert, expectedJobID, time.Now()) {
		return Alert{}, false, &UnreadableError{Kind: "backup alert", Digest: digestBytes(b)}
	}
	return alert, true, nil
}
func validAlert(a Alert, jobID string, now time.Time) bool {
	if a.JobID != jobID || !validJobID(a.JobID) || (a.TargetID != "" && !safeToken(a.TargetID, maxID)) || !safeToken(a.PolicyDigest, maxDigest) || !alertable(Health{JobID: a.JobID, TargetID: a.TargetID, State: a.State, FailureCode: a.FailureCode, PolicyDigest: a.PolicyDigest}) || safeFailureCode(a.FailureCode) != a.FailureCode || a.Fingerprint != fingerprintFor(Health{JobID: a.JobID, TargetID: a.TargetID, State: a.State, FailureCode: a.FailureCode, PolicyDigest: a.PolicyDigest}) || !safeToken(a.EpisodeID, maxID) || a.NotificationID != a.EpisodeID || len(a.Fingerprint) != 64 || a.Sequence < 1 || (a.DeliveryState != "unknown" && a.DeliveryState != "delivered") || !validTimestamp(a.FirstSeenAt, now) || !validTimestamp(a.LastSeenAt, now) || !validTimestamp(a.FailureAttemptAt, now) {
		return false
	}
	if a.RecoveredAt != "" && !validTimestamp(a.RecoveredAt, now) {
		return false
	}
	first, _ := parseTimestamp(a.FirstSeenAt)
	last, _ := parseTimestamp(a.LastSeenAt)
	attempt, _ := parseTimestamp(a.FailureAttemptAt)
	if last.Before(first) || attempt.After(last) {
		return false
	}
	if a.RecoveredAt != "" {
		recovered, _ := parseTimestamp(a.RecoveredAt)
		if recovered.Before(first) || recovered.After(last) {
			return false
		}
	}
	return true
}
func alertable(h Health) bool {
	if h.JobID == "" {
		return false
	}
	switch h.State {
	case "offline", "failed", "diverged", "stale", "unconfigured":
		return true
	}
	return false
}
func fingerprintFor(h Health) string {
	sum := sha256.Sum256([]byte(h.JobID + "\x00" + h.TargetID + "\x00" + h.State + "\x00" + safeFailureCode(h.FailureCode) + "\x00" + h.PolicyDigest))
	return hex.EncodeToString(sum[:])
}
func sameAlertScope(a Alert, h Health) bool {
	return a.JobID == h.JobID && a.TargetID == h.TargetID && a.PolicyDigest == h.PolicyDigest
}
func successAfter(success, attempted string, now time.Time) bool {
	if !validTimestamp(success, now) || !validTimestamp(attempted, now) {
		return false
	}
	s, _ := time.Parse(time.RFC3339, success)
	a, _ := time.Parse(time.RFC3339, attempted)
	return !s.Before(a)
}
