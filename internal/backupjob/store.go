package backupjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

// LoadTarget reads the selected target without discovering or contacting a provider.
func LoadTarget(configDir string) (Target, bool, error) {
	path, err := checkedPath(configDir, targetPath(configDir))
	if err != nil {
		return Target{}, false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Target{}, false, nil
	}
	if err != nil {
		return Target{}, false, fmt.Errorf("read backup target: %w", err)
	}
	var target Target
	if err := json.Unmarshal(b, &target); err != nil || validateTarget(target, time.Now()) != nil {
		return Target{}, false, errors.New("backup target is unreadable")
	}
	return target, true, nil
}

// SaveSelectedTarget persists the initially selected target. A changed target
// requires the higher-level signed cutover path and is refused here.
func SaveSelectedTarget(configDir string, target Target) error {
	if err := validateTarget(target, time.Now()); err != nil {
		return err
	}
	path, err := checkedPath(configDir, targetPath(configDir))
	if err != nil {
		return err
	}
	return leasefile.WithGuard(path, func() error {
		current, found, err := LoadTarget(configDir)
		if err != nil {
			return err
		}
		if found {
			if sameTarget(current, target) {
				return nil
			}
			return errors.New("backup target change requires cutover")
		}
		return writeDurableJSON(path, target)
	})
}

// LoadReceipt returns no receipt when the job has never been started.
func LoadReceipt(stateDir, jobID string) (Receipt, bool, error) {
	if !safeComponent(jobID) {
		return Receipt{}, false, errors.New("invalid backup job id")
	}
	path, err := checkedPath(stateDir, receiptPath(stateDir, jobID))
	if err != nil {
		return Receipt{}, false, err
	}
	return loadReceipt(path)
}

// Begin durably records a running receipt before any caller may perform work.
// It refuses a live record even if a short file lock has expired; recovery must
// carry the exact observed run and start time as explicit stale evidence.
func Begin(stateDir string, spec JobSpec, now time.Time) (Receipt, error) {
	if spec.RunID == "" {
		var err error
		spec.RunID, err = newRunID()
		if err != nil {
			return Receipt{}, err
		}
	}
	if err := validateSpec(spec); err != nil {
		return Receipt{}, err
	}
	path, err := checkedPath(stateDir, receiptPath(stateDir, spec.JobID))
	if err != nil {
		return Receipt{}, err
	}
	var next Receipt
	err = leasefile.WithGuard(path, func() error {
		previous, found, err := loadReceipt(path)
		if err != nil {
			return err
		}
		if found && previous.State == "running" && (spec.RecoverRunID != previous.RunID || spec.RecoverStartedAt != previous.StartedAt) {
			return ErrRunning
		}
		if found && previous.State == "running" && !validTimestamp(spec.RecoverStartedAt, now) {
			return errors.New("invalid interrupted-run recovery evidence")
		}
		next = Receipt{Schema: schema, JobID: spec.JobID, TargetID: spec.TargetID, PolicyDigest: spec.PolicyDigest, RunID: spec.RunID, Operation: spec.Operation, State: "running", StartedAt: now.UTC().Format(time.RFC3339Nano)}
		if found && previous.TargetID == spec.TargetID && previous.PolicyDigest == spec.PolicyDigest {
			next.LastSuccessAt = previous.LastSuccessAt
		}
		return writeDurableJSON(path, next)
	})
	return next, err
}

// Finish owner-compares the active receipt. A late completer never overwrites a successor.
func Finish(stateDir string, running Receipt, result Result, now time.Time) (Receipt, error) {
	if err := validateReceiptIdentity(running); err != nil || running.State != "running" {
		return Receipt{}, ErrOwnershipLost
	}
	switch result.State {
	case "succeeded", "failed", "diverged":
	default:
		return Receipt{}, errors.New("invalid backup result state")
	}
	path, err := checkedPath(stateDir, receiptPath(stateDir, running.JobID))
	if err != nil {
		return Receipt{}, err
	}
	var next Receipt
	err = leasefile.WithGuard(path, func() error {
		current, found, err := loadReceipt(path)
		if err != nil || !found || current.State != "running" || current.RunID != running.RunID || current.JobID != running.JobID || current.TargetID != running.TargetID || current.PolicyDigest != running.PolicyDigest || current.Operation != running.Operation {
			return ErrOwnershipLost
		}
		next = current
		next.State = result.State
		next.FinishedAt = now.UTC().Format(time.RFC3339Nano)
		next.LocalOID, next.RemoteOID = result.LocalOID, result.RemoteOID
		if result.State == "succeeded" {
			next.LastSuccessAt = next.FinishedAt
			next.FailureCode, next.SafeDetail = "", ""
		} else {
			next.FailureCode = safeFailureCode(result.FailureCode)
			if result.State == "diverged" {
				next.FailureCode = "diverged"
			}
			next.SafeDetail = safeMessage(next.FailureCode)
		}
		return writeDurableJSON(path, next)
	})
	return next, err
}

func loadReceipt(path string) (Receipt, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("read backup receipt: %w", err)
	}
	var receipt Receipt
	if err := json.Unmarshal(b, &receipt); err != nil || validateReceiptIdentity(receipt) != nil {
		return Receipt{}, false, errors.New("backup receipt is unreadable")
	}
	return receipt, true, nil
}

func validateReceiptIdentity(r Receipt) error {
	if r.Schema != schema || !safeComponent(r.JobID) || !safeComponent(r.TargetID) || !safeComponent(r.PolicyDigest) || !safeComponent(r.RunID) || !safeComponent(r.Operation) {
		return errors.New("invalid backup receipt identity")
	}
	return nil
}

func sameTarget(a, b Target) bool {
	a.SelectedAt, b.SelectedAt = "", ""
	if a.Schema != b.Schema || a.ID != b.ID || a.Provider != b.Provider || a.Repository != b.Repository || a.RemoteDigest != b.RemoteDigest || a.Visibility != b.Visibility || a.PrivateVerifiedAt != b.PrivateVerifiedAt || len(a.AuthorizedReaderIDs) != len(b.AuthorizedReaderIDs) {
		return false
	}
	for i := range a.AuthorizedReaderIDs {
		if a.AuthorizedReaderIDs[i] != b.AuthorizedReaderIDs[i] {
			return false
		}
	}
	return true
}

func writeDurableJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(path, append(b, '\n'), 0o600)
}

// checkedPath rejects a linked output root or an existing linked child before a
// durable write; job and target IDs are validated before paths are constructed.
func checkedPath(root, path string) (string, error) {
	if root == "" {
		return "", errors.New("missing backup state directory")
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || len(rel) > 2 && rel[:3] == ".."+string(filepath.Separator) {
		return "", errors.New("backup output path escapes state directory")
	}
	for current := root; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("backup output path is linked")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	for current := filepath.Dir(path); current != root && current != filepath.Dir(current); current = filepath.Dir(current) {
		if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("backup output path is linked")
		}
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("backup output path is linked")
	}
	return path, nil
}
