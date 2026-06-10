package memory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// SyncStatus is the per-source state surfaced by `mora sync status` and used to
// resume an interrupted backfill. Cursors are stored for a future incremental
// upgrade but are not the v1 refresh path.
type SyncStatus struct {
	Source       string `json:"source"`
	LastSynced   string `json:"last_synced"`
	ItemCount    int    `json:"item_count"`
	ErrorCount   int    `json:"error_count"`
	LastError    string `json:"last_error,omitempty"`
	Checkpoint   string `json:"checkpoint,omitempty"`    // in-progress page token (resume)
	GmailHistory string `json:"gmail_history,omitempty"` // future incremental
	CalSyncToken string `json:"cal_sync_token,omitempty"`

	// Last-attempt health (M-3). Health is the LAST attempt's outcome, not a
	// sticky lifetime tally: a clean sync resets ErrorCount/LastError and stamps
	// LastSuccessAt, so a source that errored once then recovered no longer reads
	// "unavailable" forever. LastAttemptAt is stamped on every attempt (success or
	// failure); LastSuccessAt only on a clean finish — so the digest (D-03) can
	// tell "never succeeded" from "succeeded but stale". Appended so prior on-disk
	// JSON round-trips (LoadStatus zero-values them).
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
}

func LoadStatus(path string) (*SyncStatus, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &SyncStatus{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s SyncStatus
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func SaveStatus(path string, s *SyncStatus) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
