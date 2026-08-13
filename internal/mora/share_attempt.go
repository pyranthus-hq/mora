package mora

// share_attempt.go — Packet H4: ONE durable, singular, owner-fenced attempt.json
// per subscription. It records import health only — it NEVER fences a generation
// write or selects/suppresses served bytes. It transitions active→terminal only
// through a run-id owner compare-and-claim, is never cleared, and accepts
// "succeeded" only after its matching commit is directory-durable. A stale
// completer therefore cannot replace a successor's active record.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type shareAttempt struct {
	RunID       string `json:"run_id"`
	State       string `json:"state"` // active | succeeded | failed
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at,omitempty"`
	Seq         int    `json:"seq,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

// errAttemptOwnershipLost means a successor published its own attempt record
// between this run's read and its claim; the stale transition is discarded.
var errAttemptOwnershipLost = errors.New("share attempt: a successor owns the record; discarding stale transition")

// These targeted seams make the attempt-start durability calls observable to
// row 51b/51c without weakening production. The mutations delete the calls in
// writeShareAttemptStartDurable; tests only wrap the real functions to record
// their order.
var (
	shareAttemptStartFileSyncFn = (*os.File).Sync
	shareAttemptStartDirSyncFn  = atomicio.SyncDir
)

func shareAttemptClaimPaths(cfg Config, name string) ([]string, error) {
	dir := shareSubRoot(cfg, name)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	prefix := filepath.Base(shareAttemptPath(cfg, name)) + ".claim-"
	var claims []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			claims = append(claims, filepath.Join(dir, entry.Name()))
		}
	}
	return claims, nil
}

func loadShareAttempt(cfg Config, name string) (shareAttempt, bool, error) {
	claims, err := shareAttemptClaimPaths(cfg, name)
	if err != nil {
		return shareAttempt{}, false, fmt.Errorf("share %q: checking attempt transition debris: %w", name, err)
	}
	if len(claims) > 0 {
		return shareAttempt{}, false, fmt.Errorf("share %q: attempt transition is incomplete; run `mora share pull %s`", name, name)
	}
	b, err := os.ReadFile(shareAttemptPath(cfg, name))
	if errors.Is(err, os.ErrNotExist) {
		return shareAttempt{}, false, nil
	}
	if err != nil {
		return shareAttempt{}, false, fmt.Errorf("share %q: reading attempt record: %w", name, err)
	}
	var a shareAttempt
	if err := json.Unmarshal(b, &a); err != nil {
		return shareAttempt{}, false, fmt.Errorf("share %q: attempt record is unreadable: %w", name, err)
	}
	if a.RunID == "" || a.StartedAt == "" {
		return shareAttempt{}, false, fmt.Errorf("share %q: attempt record is incomplete", name)
	}
	switch a.State {
	case "active", "failed":
	case "succeeded":
		if a.Seq <= 0 {
			return shareAttempt{}, false, fmt.Errorf("share %q: succeeded attempt has no committed sequence", name)
		}
	default:
		return shareAttempt{}, false, fmt.Errorf("share %q: attempt record has invalid state %q", name, a.State)
	}
	return a, true, nil
}

// startShareAttempt publishes the durable {run_id,state:active,started_at} record
// with atomicWriteDurable's exact write → fsync → rename → syncDir sequence
// BEFORE the transport's first write. Every error aborts before fetch/build.
func startShareAttempt(cfg Config, name, runID string, now time.Time) error {
	a := shareAttempt{RunID: runID, State: "active", StartedAt: now.UTC().Format(time.RFC3339)}
	body, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return writeShareAttemptStartDurable(shareAttemptPath(cfg, name), append(body, '\n'))
}

// writeShareAttemptStartDurable publishes the active record through its own
// explicit write -> file sync -> rename -> parent-dir sync sequence. Fetch/build
// cannot begin until this function returns.
func writeShareAttemptStartDurable(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := shareAttemptStartFileSyncFn(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := atomicio.RenameReplaceWithRetry(tmp, path); err != nil {
		return err
	}
	return shareAttemptStartDirSyncFn(dir)
}

// recoverShareAttemptClaims repairs the only crash debris the owner-CAS can
// leave. Under the import lease, a lone claim is authoritative when attempt.json
// is absent; when attempt.json exists, it is the newer authoritative record and
// every old claim is discarded. Ambiguous multiple lone claims fail closed.
func recoverShareAttemptClaims(cfg Config, name string) error {
	claims, err := shareAttemptClaimPaths(cfg, name)
	if err != nil || len(claims) == 0 {
		return err
	}
	path := shareAttemptPath(cfg, name)
	dir := shareSubRoot(cfg, name)
	_, statErr := os.Lstat(path)
	switch {
	case statErr == nil:
		return cleanupAttemptDebrisDurably(dir, claims...)
	case !errors.Is(statErr, os.ErrNotExist):
		return fmt.Errorf("share %q: checking attempt record during recovery: %w", name, statErr)
	case len(claims) != 1:
		return fmt.Errorf("share %q: %d orphaned attempt claims are ambiguous; refusing to fetch", name, len(claims))
	}

	claim := claims[0]
	if err := claimExclusiveDurable(claim, path); err != nil {
		return fmt.Errorf("share %q: restoring interrupted attempt record: %w", name, err)
	}
	// First make the restored authoritative name durable, then make removal of
	// its claim alias durable. A hardlink-unsupported fallback may already have
	// consumed the claim path; cleanup treats that as success.
	if err := atomicio.SyncDir(dir); err != nil {
		return err
	}
	return cleanupAttemptDebrisDurably(dir, claim)
}

func cleanupAttemptDebrisDurably(dir string, paths ...string) error {
	var cleanupErr error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	cleanupErr = errors.Join(cleanupErr, atomicio.SyncDir(dir))
	return cleanupErr
}

// finishShareAttempt transitions the matching active record to a terminal state
// via the owner compare-and-claim. It writes+fsyncs the terminal body to a
// unique same-dir temp, requires the observed active record's run_id to be ours,
// rename-claims attempt.json, byte-compares the claimed inode with what it read,
// and only then publishes the terminal record with a create-exclusive primitive.
// If a successor replaced the record in the window, the claimed bytes differ, are
// restored, and this stale transition is discarded (never clobbering the successor).
func finishShareAttempt(cfg Config, name, runID string, terminal shareAttempt) error {
	path := shareAttemptPath(cfg, name)
	dir := shareSubRoot(cfg, name)
	if terminal.State != "succeeded" && terminal.State != "failed" {
		return fmt.Errorf("share %q: invalid terminal attempt state %q", name, terminal.State)
	}

	observed, err := os.ReadFile(path)
	if err != nil {
		return errAttemptOwnershipLost // absent: never resurrect a removed/replaced record
	}
	var obs shareAttempt
	if json.Unmarshal(observed, &obs) != nil {
		return fmt.Errorf("share %q: attempt record unreadable", name)
	}
	if obs.RunID != runID || obs.State != "active" {
		return errAttemptOwnershipLost // a successor already owns it
	}
	terminal.RunID = runID
	terminal.StartedAt = obs.StartedAt
	if terminal.CompletedAt == "" {
		terminal.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	}
	body, merr := json.MarshalIndent(terminal, "", "  ")
	if merr != nil {
		return merr
	}
	temp, terr := writeAttemptTemp(dir, append(body, '\n'))
	if terr != nil {
		return terr
	}
	defer func() { _ = os.Remove(temp) }()

	claim := path + ".claim-" + runID
	if rerr := atomicio.RenameReplaceWithRetry(path, claim); rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return errAttemptOwnershipLost
		}
		return rerr
	}
	cur, rerr := os.ReadFile(claim)
	if rerr != nil || !bytes.Equal(cur, observed) {
		// A successor published between our read and claim: restore the claimed
		// bytes (only if the path is still absent) and discard our stale transition.
		restoreErr := claimExclusiveDurable(claim, path)
		if restoreErr != nil && !errors.Is(restoreErr, os.ErrExist) {
			// Keep the claim for preflight recovery; deleting it here would lose the
			// only authoritative attempt bytes.
			return errors.Join(errAttemptOwnershipLost, fmt.Errorf("restore claimed attempt: %w", restoreErr))
		}
		if syncErr := atomicio.SyncDir(dir); syncErr != nil {
			return errors.Join(errAttemptOwnershipLost, syncErr)
		}
		return errors.Join(errAttemptOwnershipLost, cleanupAttemptDebrisDurably(dir, claim, temp))
	}
	pubErr := claimExclusiveDurable(temp, path)
	if errors.Is(pubErr, os.ErrExist) {
		return errors.Join(errAttemptOwnershipLost, cleanupAttemptDebrisDurably(dir, claim, temp))
	}
	if pubErr != nil {
		// attempt.json is absent and claim retains the active bytes. Leave the
		// claim for the next lease holder's preflight recovery.
		return pubErr
	}
	// The terminal record must be authoritative and directory-durable before
	// claim/temp cleanup. A second directory sync makes that cleanup durable.
	if err := atomicio.SyncDir(dir); err != nil {
		return err
	}
	return cleanupAttemptDebrisDurably(dir, claim, temp)
}

// writeAttemptTemp writes body to a unique same-dir temp, fsyncs, and closes it.
func writeAttemptTemp(dir string, body []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".attempt-*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if _, err := f.Write(body); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}
