package mora

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

const (
	traceStageSchedule  = "gen_ai.schedule"
	traceStageConnector = "gen_ai.connector"
	traceStageIngestion = "gen_ai.ingestion"
	traceStageIndex     = "gen_ai.index"
	traceStageQuery     = "gen_ai.query"
)

type traceEvent struct {
	SchemaVersion int      `json:"schema_version"`
	CorrelationID string   `json:"correlation_id"`
	Stage         string   `json:"stage"`
	ObservedAt    string   `json:"observed_at"`
	Source        string   `json:"source,omitempty"`
	Status        string   `json:"status"`
	Links         []string `json:"links,omitempty"`
}

func appendTraceEvent(cfg Config, event traceEvent) error {
	if !validOperationToken(event.CorrelationID) {
		return nil
	}
	event.SchemaVersion = 1
	if event.ObservedAt == "" {
		event.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	sort.Strings(event.Links)
	b, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return atomicio.AppendFile(filepath.Join(cfg.StateDir, "observability", "traces.jsonl"), string(b)+"\n")
}

func queryCorrelationID(seed string, links []string) string {
	ordered := append([]string(nil), links...)
	sort.Strings(ordered)
	hash := ContentHash(seed + "\x00" + strings.Join(ordered, "\x00"))
	if len(hash) > 16 {
		hash = hash[:16]
	}
	return "query_" + hash
}
