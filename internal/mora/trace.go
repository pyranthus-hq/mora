package mora

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	path := filepath.Join(cfg.StateDir, "observability", "traces.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(string(b) + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
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
