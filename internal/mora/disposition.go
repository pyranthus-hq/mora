package mora

import (
	"fmt"
	"time"

	"github.com/pyranthus-hq/mora/internal/disposition"
)

// validateDispositionPublish is called after request shaping and before every
// publication/staging boundary. It never accepts a connector-shaped correction.
func validateDispositionPublish(cfg Config, m Memory) error {
	if m.Type != "correction" || m.Meta == nil {
		return nil
	}
	targetID, hasTarget := m.Meta["target"].(string)
	if !hasTarget || targetID == "" {
		return nil
	}
	if !disposition.IsLocalAuthored(m) {
		return newMoraError(errCodeUsageUnknownValue, "usage", nil, "typed corrections require a local authored source")
	}
	target, err := findMemory(cfg, targetID)
	if err != nil {
		return fmt.Errorf("correction target: %w", err)
	}
	if err := disposition.ValidateTarget(m, target); err != nil {
		return newMoraError(errCodeUsageUnknownValue, "usage", err, "%v", err)
	}
	return nil
}

// decorateDispositions overlays the latest visible local correction from the
// whole vault. It intentionally does not call listMemories: callers may be
// decorating listMemories results, and query/page subsets cannot decide latest.
func decorateDispositions(cfg Config, rows []Memory, now time.Time) ([]Memory, error) {
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return nil, err
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		return nil, err
	}
	all := make([]Memory, 0, len(files))
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil || m.DeletedAt != "" || !g.memoryVisible(m.ID) {
			continue
		}
		all = append(all, decorateDecision(m, now))
	}
	projected := disposition.Project(all, now)
	out := append([]Memory(nil), rows...)
	for i := range out {
		if out[i].Owner != "" {
			continue
		}
		out[i].Disposition = nil // clear a prior projection when its correction disappears
		if d, ok := projected[disposition.Key{Scope: out[i].Scope, ID: out[i].ID}]; ok {
			dCopy := d
			out[i].Disposition = &dCopy
		}
	}
	return out, nil
}
