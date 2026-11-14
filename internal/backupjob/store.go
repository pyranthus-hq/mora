package backupjob

import (
	"crypto/sha256"
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
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

func targetPath(configDir string) string { return filepath.Join(configDir, "backup-target.json") }
func jobFilename(jobID string) string {
	digest := sha256.Sum256([]byte(jobID))
	return "job-" + hex.EncodeToString(digest[:16]) + ".json"
}
func receiptPath(stateDir, jobID string) string {
	return filepath.Join(stateDir, "backup", "jobs", jobFilename(jobID))
}
func alertPath(stateDir, jobID string) string {
	return filepath.Join(stateDir, "backup", "alerts", jobFilename(jobID))
}
func runLockPath(stateDir, jobID string) string {
	return filepath.Join(stateDir, "backup", "jobs", jobFilename(jobID)+".runlock")
}

func LoadTarget(configDir string) (Target, bool, error) {
	path, err := checkedPath(configDir, targetPath(configDir))
	if err != nil {
		return Target{}, false, err
	}
	b, err := readRecord(path, "backup target")
	if errors.Is(err, os.ErrNotExist) {
		return Target{}, false, nil
	}
	if err != nil {
		return Target{}, false, fmt.Errorf("read backup target: %w", err)
	}
	var target Target
	if json.Unmarshal(b, &target) != nil || validateTarget(target, time.Now()) != nil {
		return Target{}, false, errors.New("backup target is unreadable")
	}
	return canonicalTarget(target), true, nil
}

// SaveSelectedTarget only refreshes verification for the same target identity.
func SaveSelectedTarget(configDir string, target Target) error {
	target = canonicalTarget(target)
	if err := validateTarget(target, time.Now()); err != nil {
		return err
	}
	path, err := preparePath(configDir, targetPath(configDir))
	if err != nil {
		return err
	}
	return leasefile.WithGuard(path, func() error {
		current, found, err := LoadTarget(configDir)
		if err != nil {
			return err
		}
		if found {
			if !sameTarget(current, target) {
				return errors.New("backup target change requires cutover")
			}
			target.SelectedAt = current.SelectedAt
		}
		return writeDurableJSON(path, target)
	})
}

func LoadReceipt(stateDir, jobID string) (Receipt, bool, error) {
	if !validJobID(jobID) {
		return Receipt{}, false, errors.New("invalid backup job id")
	}
	path, err := checkedPath(stateDir, receiptPath(stateDir, jobID))
	if err != nil {
		return Receipt{}, false, err
	}
	return loadReceipt(path, jobID)
}

// Begin holds the actual OS lease through Finish or Close, rather than treating
// a short receipt guard as evidence that external work is still live.
func Begin(stateDir string, spec JobSpec, now time.Time) (_ *Run, retErr error) {
	if spec.RunID == "" {
		var err error
		spec.RunID, err = newRunID()
		if err != nil {
			return nil, err
		}
	}
	if spec.TargetID == "" {
		return nil, errors.New("backup target identity is required")
	}
	if err := validateExpected(spec); err != nil {
		return nil, err
	}
	path, err := preparePath(stateDir, receiptPath(stateDir, spec.JobID))
	if err != nil {
		return nil, err
	}
	lockPath, err := preparePath(stateDir, runLockPath(stateDir, spec.JobID))
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockRun(lock); err != nil {
		_ = lock.Close()
		return nil, ErrRunning
	}
	run := &Run{stateDir: stateDir, lock: lock}
	defer func() {
		if retErr != nil {
			_ = run.Close()
		}
	}()
	if err := leasefile.WithGuard(path, func() error {
		previous, found, err := loadReceipt(path, spec.JobID)
		if err != nil {
			if digest := unreadableDigest(err); digest != "" && spec.RecoverReceiptDigest == digest {
				if err := quarantine(path, digest); err != nil {
					return err
				}
				found = false
			} else {
				return err
			}
		}
		if found && validateReceipt(previous, now) != nil {
			return errors.New("backup clock precedes stored receipt")
		}
		if found && previous.State == "running" && (spec.RecoverRunID != previous.RunID || spec.RecoverStartedAt != previous.StartedAt || !validTimestamp(spec.RecoverStartedAt, now)) {
			return ErrRunning
		}
		next := Receipt{Schema: schema, JobID: spec.JobID, TargetID: spec.TargetID, PolicyDigest: spec.PolicyDigest, RunID: spec.RunID, Operation: spec.Operation, State: "running", StartedAt: now.UTC().Format(time.RFC3339Nano)}
		if found && previous.JobID == spec.JobID && previous.TargetID == spec.TargetID && previous.PolicyDigest == spec.PolicyDigest && previous.Operation == spec.Operation {
			next.LastSuccessAt = previous.LastSuccessAt
		}
		if err := validateReceipt(next, now); err != nil {
			return err
		}
		if err := writeDurableJSON(path, next); err != nil {
			return err
		}
		run.Receipt = next
		return nil
	}); err != nil {
		return nil, err
	}
	return run, nil
}

// Finish cannot accept a copied Receipt: it consumes the Run which still holds
// the OS lease established by Begin.
func Finish(run *Run, result Result, now time.Time) (out Receipt, retErr error) {
	if run == nil {
		return Receipt{}, ErrOwnershipLost
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.lock == nil || run.Receipt.State != "running" || validateReceipt(run.Receipt, now) != nil {
		return Receipt{}, ErrOwnershipLost
	}
	if (result.State != "succeeded" && result.State != "failed" && result.State != "diverged") || (result.LocalOID != "" && !safeToken(result.LocalOID, maxDigest)) || (result.RemoteOID != "" && !safeToken(result.RemoteOID, maxDigest)) {
		return Receipt{}, errors.New("invalid backup result")
	}
	defer func() { retErr = errors.Join(retErr, run.closeLocked()) }()
	path, err := checkedPath(run.stateDir, receiptPath(run.stateDir, run.Receipt.JobID))
	if err != nil {
		return Receipt{}, err
	}
	var next Receipt
	err = leasefile.WithGuard(path, func() error {
		current, found, err := loadReceipt(path, run.Receipt.JobID)
		if err != nil || !found || current.State != "running" || current.RunID != run.Receipt.RunID || current.TargetID != run.Receipt.TargetID || current.PolicyDigest != run.Receipt.PolicyDigest || current.Operation != run.Receipt.Operation {
			return ErrOwnershipLost
		}
		next = current
		next.State, next.FinishedAt = result.State, now.UTC().Format(time.RFC3339Nano)
		next.LocalOID, next.RemoteOID = result.LocalOID, result.RemoteOID
		if result.State == "succeeded" {
			next.LastSuccessAt, next.FailureCode, next.SafeDetail = next.FinishedAt, "", ""
		} else {
			next.FailureCode = safeFailureCode(result.FailureCode)
			if result.State == "diverged" {
				next.FailureCode = "diverged"
			}
			next.SafeDetail = safeMessage(next.FailureCode)
		}
		if err := validateReceipt(next, now); err != nil {
			return err
		}
		return writeDurableJSON(path, next)
	})
	return next, err
}

// LeaseLive probes the exact held OS lock used by Begin; it never creates one.
func LeaseLive(stateDir, jobID string) (bool, error) {
	if !validJobID(jobID) {
		return false, errors.New("invalid backup job id")
	}
	path, err := checkedPath(stateDir, runLockPath(stateDir, jobID))
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := lockRun(f); err != nil {
		return true, nil
	}
	if err := unlockRun(f); err != nil {
		return false, err
	}
	return false, nil
}

func loadReceipt(path, expectedJobID string) (Receipt, bool, error) {
	b, err := readRecord(path, "backup receipt")
	if errors.Is(err, os.ErrNotExist) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("read backup receipt: %w", err)
	}
	var receipt Receipt
	if json.Unmarshal(b, &receipt) != nil || receipt.JobID != expectedJobID || validateReceipt(receipt, time.Now()) != nil {
		return Receipt{}, false, &UnreadableError{Kind: "backup receipt", Digest: digestBytes(b)}
	}
	return receipt, true, nil
}

func canonicalTarget(target Target) Target {
	target.AuthorizedReaderIDs = append([]string(nil), target.AuthorizedReaderIDs...)
	sort.Strings(target.AuthorizedReaderIDs)
	return target
}
func sameTarget(a, b Target) bool {
	a, b = canonicalTarget(a), canonicalTarget(b)
	if a.Schema != b.Schema || a.ID != b.ID || a.Provider != b.Provider || a.Repository != b.Repository || a.RemoteDigest != b.RemoteDigest || a.Visibility != b.Visibility || len(a.AuthorizedReaderIDs) != len(b.AuthorizedReaderIDs) {
		return false
	}
	for i := range a.AuthorizedReaderIDs {
		if a.AuthorizedReaderIDs[i] != b.AuthorizedReaderIDs[i] {
			return false
		}
	}
	return true
}
func digestBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func quarantine(path, digest string) error {
	id, err := newRunID()
	if err != nil {
		return err
	}
	dest := path + ".corrupt-" + digest[:16] + "-" + id
	// No replacement: preserve every observed corrupt record, including repeats.
	if err := os.Link(path, dest); err != nil {
		return err
	}
	if err := atomicio.SyncDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return atomicio.SyncDir(filepath.Dir(path))
}

func readRecord(path, kind string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxRecordBytes {
		return nil, &UnreadableError{Kind: kind}
	}
	return b, nil
}
func writeDurableJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(path, append(b, '\n'), 0o600)
}

func preparePath(root, path string) (string, error) {
	checked, err := checkedPath(root, path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(checked), 0o700); err != nil {
		return "", err
	}
	return checkedPath(root, checked)
}

// checkedPath resolves aliases above root once (notably macOS /var) while
// rejecting a linked root, descendants, reparse points, FIFOs, and devices.
func checkedPath(root, path string) (string, error) {
	if root == "" {
		return "", errors.New("missing backup state directory")
	}
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(root); err == nil {
		unsafe, reparseErr := unsafeReparse(root, info)
		if !info.IsDir() || unsafeMode(info.Mode()) || reparseErr != nil || unsafe {
			return "", errors.New("backup output root is linked or unsafe")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if canonical, err := filepath.EvalSymlinks(root); err == nil {
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			rel, err = filepath.Rel(canonical, path)
		}
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", errors.New("backup output path escapes state directory")
		}
		root, path = canonical, filepath.Join(canonical, rel)
	} else if !errors.Is(err, os.ErrNotExist) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("backup output path escapes state directory")
	}
	current := root
	for {
		if info, err := os.Lstat(current); err == nil {
			unsafe, reparseErr := unsafeReparse(current, info)
			if unsafeMode(info.Mode()) || reparseErr != nil || unsafe || (current != path && !info.IsDir()) || (current == path && !info.Mode().IsRegular()) {
				return "", errors.New("backup output path is linked or unsafe")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if current == path {
			break
		}
		rest, _ := filepath.Rel(current, path)
		if rest == "." || rest == "" {
			break
		}
		current = filepath.Join(current, strings.Split(rest, string(filepath.Separator))[0])
	}
	return path, nil
}
