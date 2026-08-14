package mora

import (
	recencypkg "github.com/pyranthus-hq/mora/internal/recency"
	"time"
)

func rfc3339Instant(s string) (time.Time, bool)    { return recencypkg.Instant(s) }
func indexedAtOf(m Memory) (string, bool)          { return recencypkg.IndexedAt(m) }
func ingestRecencyOf(m Memory) (time.Time, bool)   { return recencypkg.IngestTime(m) }
func byIngestRecency(a, b Memory) bool             { return recencypkg.Before(a, b) }
func eventStartOf(m Memory) (string, bool)         { return recencypkg.EventStart(m) }
func decorateBrowseRecency(mems []Memory) []Memory { return recencypkg.Decorate(mems) }
