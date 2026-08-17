package sharing

// attempt.go — Packet H4: ONE durable, singular, owner-fenced attempt.json
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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

// Attempt is the durable health record for one subscription import run.
type Attempt struct {
	RunID       string `json:"run_id"`
	State       string `json:"state"` // active | succeeded | failed
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at,omitempty"`
	Seq         int    `json:"seq,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

// ErrAttemptOwnershipLost means a successor published its own attempt record
// between this run's read and its claim; the stale transition is discarded.
var ErrAttemptOwnershipLost = errors.New("share attempt: a successor owns the record; discarding stale transition")

// AttemptStore owns attempt.json publication, recovery, and owner-fenced terminal transitions.
type AttemptStore struct {
	DataDir        string
	FileSync       func(*os.File) error
	StartDirSync   func(string) error
	DirSync        func(string) error
	ClaimExclusive func(string, string) error
	RenameClaim    func(string, string) error
	Now            func() time.Time
}

func (s AttemptStore) root(name string) string { return SubscriptionRoot(s.DataDir, name) }
func (s AttemptStore) path(name string) string { return filepath.Join(s.root(name), "attempt.json") }
func (s AttemptStore) fileSync(f *os.File) error {
	if s.FileSync != nil {
		return s.FileSync(f)
	}
	return f.Sync()
}
func (s AttemptStore) startDirSync(dir string) error {
	if s.StartDirSync != nil {
		return s.StartDirSync(dir)
	}
	return s.dirSync(dir)
}
func (s AttemptStore) dirSync(dir string) error {
	if s.DirSync != nil {
		return s.DirSync(dir)
	}
	return atomicio.SyncDir(dir)
}
func (s AttemptStore) claim(temp, dest string) error {
	if s.ClaimExclusive != nil {
		return s.ClaimExclusive(temp, dest)
	}
	return atomicio.ClaimExclusiveDurable(temp, dest)
}
func (s AttemptStore) renameClaim(oldPath, newPath string) error {
	if s.RenameClaim != nil {
		return s.RenameClaim(oldPath, newPath)
	}
	return atomicio.RenameReplaceWithRetry(oldPath, newPath)
}
func (s AttemptStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s AttemptStore) ClaimPaths(name string) ([]string, error) {
	dir := s.root(name)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	prefix := filepath.Base(s.path(name)) + ".claim-"
	var claims []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			claims = append(claims, filepath.Join(dir, entry.Name()))
		}
	}
	return claims, nil
}

func (s AttemptStore) Load(name string) (Attempt, bool, error) {
	claims, err := s.ClaimPaths(name)
	if err != nil {
		return Attempt{}, false, fmt.Errorf("share %q: checking attempt transition debris: %w", name, err)
	}
	if len(claims) > 0 {
		return Attempt{}, false, fmt.Errorf("share %q: attempt transition is incomplete; run `mora share pull %s`", name, name)
	}
	b, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return Attempt{}, false, nil
	}
	if err != nil {
		return Attempt{}, false, fmt.Errorf("share %q: reading attempt record: %w", name, err)
	}
	var a Attempt
	if err := json.Unmarshal(b, &a); err != nil {
		return Attempt{}, false, fmt.Errorf("share %q: attempt record is unreadable: %w", name, err)
	}
	if a.RunID == "" || a.StartedAt == "" {
		return Attempt{}, false, fmt.Errorf("share %q: attempt record is incomplete", name)
	}
	switch a.State {
	case "active", "failed":
	case "succeeded":
		if a.Seq <= 0 {
			return Attempt{}, false, fmt.Errorf("share %q: succeeded attempt has no committed sequence", name)
		}
	default:
		return Attempt{}, false, fmt.Errorf("share %q: attempt record has invalid state %q", name, a.State)
	}
	return a, true, nil
}

// AttemptStore.Start publishes the durable {run_id,state:active,started_at} record
// with atomicWriteDurable's exact write → fsync → rename → syncDir sequence
// BEFORE the transport's first write. Every error aborts before fetch/build.
func (s AttemptStore) Start(name, runID string, now time.Time) error {
	a := Attempt{RunID: runID, State: "active", StartedAt: now.UTC().Format(time.RFC3339)}
	body, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return s.writeStartDurable(s.path(name), append(body, '\n'))
}

// writeStartDurable publishes the active record through its own
// explicit write -> file sync -> rename -> parent-dir sync sequence. Fetch/build
// cannot begin until this function returns.
func (s AttemptStore) writeStartDurable(path string, body []byte) error {
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
	if err := s.fileSync(f); err != nil {
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
	return s.startDirSync(dir)
}

// AttemptStore.RecoverClaims repairs the only crash debris the owner-CAS can
// leave. Under the import lease, a lone claim is authoritative when attempt.json
// is absent; when attempt.json exists, it is the newer authoritative record and
// every old claim is discarded. Ambiguous multiple lone claims fail closed.
func (s AttemptStore) RecoverClaims(name string) error {
	claims, err := s.ClaimPaths(name)
	if err != nil || len(claims) == 0 {
		return err
	}
	path := s.path(name)
	dir := s.root(name)
	_, statErr := os.Lstat(path)
	switch {
	case statErr == nil:
		return s.cleanupDebrisDurably(dir, claims...)
	case !errors.Is(statErr, os.ErrNotExist):
		return fmt.Errorf("share %q: checking attempt record during recovery: %w", name, statErr)
	case len(claims) != 1:
		return fmt.Errorf("share %q: %d orphaned attempt claims are ambiguous; refusing to fetch", name, len(claims))
	}

	claim := claims[0]
	if err := s.claim(claim, path); err != nil {
		return fmt.Errorf("share %q: restoring interrupted attempt record: %w", name, err)
	}
	// First make the restored authoritative name durable, then make removal of
	// its claim alias durable. A hardlink-unsupported fallback may already have
	// consumed the claim path; cleanup treats that as success.
	if err := s.dirSync(dir); err != nil {
		return err
	}
	return s.cleanupDebrisDurably(dir, claim)
}

func (s AttemptStore) cleanupDebrisDurably(dir string, paths ...string) error {
	var cleanupErr error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	cleanupErr = errors.Join(cleanupErr, s.dirSync(dir))
	return cleanupErr
}

// AttemptStore.Finish transitions the matching active record to a terminal state
// via the owner compare-and-claim. It writes+fsyncs the terminal body to a
// unique same-dir temp, requires the observed active record's run_id to be ours,
// rename-claims attempt.json, byte-compares the claimed inode with what it read,
// and only then publishes the terminal record with a create-exclusive primitive.
// If a successor replaced the record in the window, the claimed bytes differ, are
// restored, and this stale transition is discarded (never clobbering the successor).
func (s AttemptStore) Finish(name, runID string, terminal Attempt) error {
	path := s.path(name)
	dir := s.root(name)
	if terminal.State != "succeeded" && terminal.State != "failed" {
		return fmt.Errorf("share %q: invalid terminal attempt state %q", name, terminal.State)
	}

	observed, err := os.ReadFile(path)
	if err != nil {
		return ErrAttemptOwnershipLost // absent: never resurrect a removed/replaced record
	}
	var obs Attempt
	if json.Unmarshal(observed, &obs) != nil {
		return fmt.Errorf("share %q: attempt record unreadable", name)
	}
	if obs.RunID != runID || obs.State != "active" {
		return ErrAttemptOwnershipLost // a successor already owns it
	}
	terminal.RunID = runID
	terminal.StartedAt = obs.StartedAt
	if terminal.CompletedAt == "" {
		terminal.CompletedAt = s.now().UTC().Format(time.RFC3339)
	}
	body, merr := json.MarshalIndent(terminal, "", "  ")
	if merr != nil {
		return merr
	}
	temp, terr := s.writeTemp(dir, append(body, '\n'))
	if terr != nil {
		return terr
	}
	defer func() { _ = os.Remove(temp) }()

	claim := path + ".claim-" + runID
	if rerr := s.renameClaim(path, claim); rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return ErrAttemptOwnershipLost
		}
		return rerr
	}
	cur, rerr := os.ReadFile(claim)
	if rerr != nil || !bytes.Equal(cur, observed) {
		// A successor published between our read and claim: restore the claimed
		// bytes (only if the path is still absent) and discard our stale transition.
		restoreErr := s.claim(claim, path)
		if restoreErr != nil && !errors.Is(restoreErr, os.ErrExist) {
			// Keep the claim for preflight recovery; deleting it here would lose the
			// only authoritative attempt bytes.
			return errors.Join(ErrAttemptOwnershipLost, fmt.Errorf("restore claimed attempt: %w", restoreErr))
		}
		if syncErr := s.dirSync(dir); syncErr != nil {
			return errors.Join(ErrAttemptOwnershipLost, syncErr)
		}
		return errors.Join(ErrAttemptOwnershipLost, s.cleanupDebrisDurably(dir, claim, temp))
	}
	pubErr := s.claim(temp, path)
	if errors.Is(pubErr, os.ErrExist) {
		return errors.Join(ErrAttemptOwnershipLost, s.cleanupDebrisDurably(dir, claim, temp))
	}
	if pubErr != nil {
		// attempt.json is absent and claim retains the active bytes. Leave the
		// claim for the next lease holder's preflight recovery.
		return pubErr
	}
	// The terminal record must be authoritative and directory-durable before
	// claim/temp cleanup. A second directory sync makes that cleanup durable.
	if err := s.dirSync(dir); err != nil {
		return err
	}
	return s.cleanupDebrisDurably(dir, claim, temp)
}

// writeTemp writes body to a unique same-dir temp, fsyncs, and closes it.
func (s AttemptStore) writeTemp(dir string, body []byte) (string, error) {
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
