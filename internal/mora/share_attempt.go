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
	"os"
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

func loadShareAttempt(cfg Config, name string) (shareAttempt, bool) {
	b, err := os.ReadFile(shareAttemptPath(cfg, name))
	if err != nil {
		return shareAttempt{}, false
	}
	var a shareAttempt
	if json.Unmarshal(b, &a) != nil {
		return shareAttempt{}, false
	}
	return a, true
}

// startShareAttempt publishes the durable {run_id,state:active,started_at} record
// with atomicWriteDurable (write → fsync → rename → syncDir) BEFORE the transport's
// first write. Every error aborts before fetch/build.
func startShareAttempt(cfg Config, name, runID string, now time.Time) error {
	a := shareAttempt{RunID: runID, State: "active", StartedAt: now.UTC().Format(time.RFC3339)}
	body, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteDurable(shareAttemptPath(cfg, name), append(body, '\n'), 0o600)
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

	observed, err := os.ReadFile(path)
	if err != nil {
		return errAttemptOwnershipLost // absent: never resurrect a removed/replaced record
	}
	var obs shareAttempt
	if json.Unmarshal(observed, &obs) != nil {
		return fmt.Errorf("share %q: attempt record unreadable", name)
	}
	if obs.RunID != runID {
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
	defer os.Remove(temp)

	claim := path + ".claim-" + runID
	if rerr := renameReplaceWithRetry(path, claim); rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return errAttemptOwnershipLost
		}
		return rerr
	}
	cur, rerr := os.ReadFile(claim)
	if rerr != nil || !bytes.Equal(cur, observed) {
		// A successor published between our read and claim: restore the claimed
		// bytes (only if the path is still absent) and discard our stale transition.
		_ = claimExclusiveDurable(claim, path)
		_ = os.Remove(claim)
		return errAttemptOwnershipLost
	}
	pubErr := claimExclusiveDurable(temp, path)
	_ = os.Remove(claim)
	if errors.Is(pubErr, os.ErrExist) {
		return errAttemptOwnershipLost
	}
	if pubErr != nil {
		return pubErr
	}
	return syncDir(dir)
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
