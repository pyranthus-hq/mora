package resource

import (
	"fmt"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

// AppendEvent records a local receipt. TrackingDisabled is an explicit no-op
// for callers honoring DO_NOT_TRACK or `mora usage off`.
func AppendEvent(stateRoot string, event Event) error {
	if event.TrackingDisabled {
		return nil
	}
	if event.Type != EventRecommended && event.Type != EventLoaded && event.Type != EventOutcome || !validID(event.ResourceID) || !validDigest(event.RevisionDigest) || len(event.Evidence) > 512 || !validScope(event.ProjectScope) || ((event.Type == EventLoaded || event.Type == EventOutcome) && strings.TrimSpace(event.Evidence) == "") {
		return fmt.Errorf("%w: event", ErrInvalidCandidate)
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	} else {
		event.OccurredAt = event.OccurredAt.UTC()
	}
	path := stateRoot + "/resources/events/events.jsonl"
	line := string(mustJSON(event)) + "\n"
	if strings.Contains(line, "\n") && strings.Count(line, "\n") != 1 {
		return fmt.Errorf("%w: event line", ErrInvalidCandidate)
	}
	return leasefile.WithGuard(path+".lock", func() error { return atomicio.AppendFile(path, line) })
}
