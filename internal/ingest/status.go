package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
)

const (
	GoogleStatusThreshold = 24 * time.Hour
	LocalStatusThreshold  = 48 * time.Hour
	StateNever            = "never"
	StateFailed           = "failed"
	StateStale            = "stale"
	StateFresh            = "fresh"
)

// StatusPath returns the canonical per-source sync status path.
func StatusPath(cfg config.Config, prefix, name string) string {
	return filepath.Join(cfg.StateDir, "sync", prefix+"-"+name+".json")
}

// StatusFileThreshold classifies remote/API and local source status filenames.
func StatusFileThreshold(fileName string) time.Duration {
	switch {
	case strings.HasPrefix(fileName, "google-"), strings.HasPrefix(fileName, "applecal-"), strings.HasPrefix(fileName, "github-"):
		return GoogleStatusThreshold
	default:
		return LocalStatusThreshold
	}
}

// StatusFileState classifies the latest attempt without hiding failures.
func StatusFileState(st *memory.SyncStatus, threshold time.Duration, now time.Time) string {
	if st.LastSuccessAt == "" {
		return StateNever
	}
	if st.LastError != "" || st.ErrorCount > 0 {
		return StateFailed
	}
	t, err := time.Parse(time.RFC3339, st.LastSuccessAt)
	if err != nil {
		return StateNever
	}
	age := now.Sub(t)
	if age < 0 {
		age = 0
	}
	if age > threshold {
		return StateStale
	}
	return StateFresh
}

// PersistStatus returns the save failure separately for presentation and preserves the ingest error when both fail.
func PersistStatus(statusPath string, st *memory.SyncStatus, ingErr error) (saveErr error, result error) {
	saveErr = memory.SaveStatus(statusPath, st)
	if saveErr != nil && ingErr == nil {
		return saveErr, fmt.Errorf("persisting sync status: %w", saveErr)
	}
	return saveErr, ingErr
}

// SourceFreshness reads best-effort last-sync timestamps keyed by status Source or legacy filename.
func SourceFreshness(cfg config.Config) map[string]string {
	out := map[string]string{}
	dir := filepath.Join(cfg.StateDir, "sync")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		st, err := memory.LoadStatus(filepath.Join(dir, e.Name()))
		if err != nil || st == nil {
			continue
		}
		key := st.Source
		if key == "" {
			key = strings.TrimSuffix(e.Name(), ".json")
			key = strings.TrimPrefix(key, "google-")
			key = strings.TrimPrefix(key, "imessage-")
		}
		out[key] = st.LastSynced
	}
	return out
}
