package backupjob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pyranthus-hq/mora/internal/leasefile"
)

// AdvanceAlert records at most one notification-worthy episode per distinct
// current failure. Callers own notification delivery; health-only reads do not call this.
func AdvanceAlert(stateDir string, health Health, now time.Time) (Alert, bool, error) {
	if !safeComponent(health.JobID) {
		return Alert{}, false, errors.New("invalid backup alert job id")
	}
	path, err := checkedPath(stateDir, alertPath(stateDir, health.JobID))
	if err != nil {
		return Alert{}, false, err
	}
	var out Alert
	var notify bool
	err = leasefile.WithGuard(path, func() error {
		prior, found, err := loadAlert(path)
		if err != nil {
			return err
		}
		if health.State == "healthy" {
			if found && prior.RecoveredAt == "" && sameAlertScope(prior, health) && successAfter(health.LastSuccessAt, prior.FailureAttemptAt, now) {
				prior.RecoveredAt = now.UTC().Format(time.RFC3339Nano)
				prior.LastSeenAt = prior.RecoveredAt
				if err := writeDurableJSON(path, prior); err != nil {
					return err
				}
				out = prior
				return nil
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
			if err := writeDurableJSON(path, prior); err != nil {
				return err
			}
			out = prior
			return nil
		}
		sequence := 1
		if found {
			sequence = prior.Sequence + 1
		}
		stamp := now.UTC().Format(time.RFC3339Nano)
		out = Alert{JobID: health.JobID, TargetID: health.TargetID, Fingerprint: fingerprint, PolicyDigest: health.PolicyDigest, FailureAttemptAt: health.LastAttemptAt, FirstSeenAt: stamp, LastSeenAt: stamp, Sequence: sequence}
		out.EpisodeID = fmt.Sprintf("backup-%s-%d", fingerprint[:16], sequence)
		out.NotificationID = out.EpisodeID
		out.DeliveryState = "unknown"
		if err := writeDurableJSON(path, out); err != nil {
			return err
		}
		notify = true
		return nil
	})
	return out, notify, err
}

func loadAlert(path string) (Alert, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Alert{}, false, nil
	}
	if err != nil {
		return Alert{}, false, fmt.Errorf("read backup alert: %w", err)
	}
	var alert Alert
	if err := json.Unmarshal(b, &alert); err != nil || !safeComponent(alert.JobID) || !safeComponent(alert.TargetID) || alert.EpisodeID == "" || alert.Fingerprint == "" || !safeComponent(alert.PolicyDigest) || alert.Sequence < 1 {
		return Alert{}, false, errors.New("backup alert is unreadable")
	}
	return alert, true, nil
}

func alertable(h Health) bool {
	switch h.State {
	case "offline", "failed", "diverged", "stale":
		return h.TargetID != ""
	default:
		return false
	}
}

func fingerprintFor(h Health) string {
	s := h.JobID + "\x00" + h.TargetID + "\x00" + h.State + "\x00" + safeFailureCode(h.FailureCode) + "\x00" + h.PolicyDigest
	digest := sha256.Sum256([]byte(s))
	return hex.EncodeToString(digest[:])
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
