package ingest

import (
	"errors"
	"os"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
)

// PublishSeams supplies composition-owned identity, time, path, and process facts.
type PublishSeams struct {
	ValidToken func(string) bool
	NewID      func() string
	Clock      func() time.Time
	CleanPath  func(string) string
	Lease      LeaseSeams
}

// EnsureJournalHeader durably marks a source dirty before its first file is visible.
func EnsureJournalHeader(cfg config.Config, sourceKey string, seams PublishSeams) error {
	if err := EnsureLease(cfg, sourceKey, seams.Lease); err != nil {
		return err
	}
	jp := JournalPath(cfg, sourceKey)
	if _, err := os.Stat(jp); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	runID := cfg.OperationRunID()
	if !seams.ValidToken(runID) {
		runID = seams.NewID()
	}
	return AppendDurable(jp, "run "+runID+" "+seams.Clock().UTC().Format(time.RFC3339)+"\n")
}

// RecordPublishedPath best-effort records a file after publication; the durable header is the correctness signal.
func RecordPublishedPath(cfg config.Config, sourceKey, path string, seams PublishSeams) {
	if ValidateStateRoot(cfg) != nil {
		return
	}
	_ = atomicio.AppendFile(JournalPath(cfg, sourceKey), seams.CleanPath(path)+"\n")
}
