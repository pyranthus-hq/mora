package mora

import (
	"errors"
	"fmt"
	"path/filepath"
)

// Retention does not authorize emptying the current corpus. Check before even
// creating recovery keys or manifests. This is a scan of the reviewed vault,
// not a transaction against concurrent external edits or Teach operations.
func validateRetentionSurvivor(cfg Config, report retentionReport) error {
	g, err := loadGovernance(cfg)
	if err != nil {
		return err
	}
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return err
	}
	candidates := make(map[string]retentionCandidate, len(report.Candidates))
	for _, candidate := range report.Candidates {
		if candidate.Decision == nil {
			return errors.New("retention candidate has no decision")
		}
		switch candidate.Decision.Action {
		case "keep", "change-class", "compact", "delete":
		default:
			return errors.New("invalid retention decision action")
		}
		path, err := retentionCandidatePath(cfg, candidate)
		if err != nil {
			return err
		}
		candidates[filepath.Clean(path)] = candidate
	}
	survivor := false
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil {
			return fmt.Errorf("cannot establish retention survivors from an unreadable memory: %w", err)
		}
		candidate, reviewed := candidates[filepath.Clean(path)]
		if reviewed && candidate.Decision.Action == "compact" {
			// Compaction creates a new non-tombstoned document even if its source is
			// historical. A previously governed deterministic output is not a survivor.
			if g.memoryVisible(retentionCompactID(report.ReportID, candidate.ID)) {
				survivor = true
			}
		}
		if m.DeletedAt != "" || !g.memoryVisible(m.ID) {
			continue
		}
		if !reviewed || candidate.Decision.Action == "keep" || candidate.Decision.Action == "change-class" {
			survivor = true
		}
	}
	if !survivor {
		return errors.New("retention execution would leave no current memories; keep a current memory or compact a candidate before executing")
	}
	return nil
}
