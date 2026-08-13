// Package usage records content-free, local-only product usage in Mora's state directory.
package usage

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

// Config is the package-neutral state location.
type Config struct{ StateDir string }

// PhaseTimings records content-free MCP pipeline durations.
type PhaseTimings struct {
	ConfigMillis    int64  `json:"config_ms"`
	RetrievalMillis *int64 `json:"retrieval_ms,omitempty"`
	AssemblyMillis  *int64 `json:"assembly_ms,omitempty"`
	EnvelopeMillis  int64  `json:"envelope_ms"`
}

// Event records one local invocation without response or argument content.
type Event struct {
	TS              string        `json:"ts"`
	Tool            string        `json:"tool"`
	Query           string        `json:"query,omitempty"`
	Scope           string        `json:"scope,omitempty"`
	Results         int           `json:"results"`
	Millis          int64         `json:"millis"`
	OutputBytes     int           `json:"output_bytes,omitempty"`
	Mode            string        `json:"mode,omitempty"`
	Truncated       *bool         `json:"truncated,omitempty"`
	MatchCount      *int          `json:"match_count,omitempty"`
	BudgetRequested *int          `json:"budget_requested,omitempty"`
	BudgetUsed      *int          `json:"budget_used,omitempty"`
	Phases          *PhaseTimings `json:"phases,omitempty"`
}

var appendMu sync.Mutex

// Report writes aggregate, content-free usage statistics.
func Report(cfg Config, stdout io.Writer) error {
	b, err := os.ReadFile(filepath.Join(cfg.StateDir, "usage", "events.jsonl"))
	if err != nil {
		fmt.Fprintln(stdout, "no usage recorded")
		return nil
	}
	byTool := map[string]int{}
	empty, total := 0, 0
	var latencies []int64
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var event Event
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		byTool[event.Tool]++
		total++
		if event.Results == 0 {
			empty++
		}
		latencies = append(latencies, event.Millis)
	}
	fmt.Fprintln(stdout, "Mora usage (content-free)")
	fmt.Fprintf(stdout, "total calls: %d\n", total)
	for tool, count := range byTool {
		fmt.Fprintf(stdout, "  %s: %d\n", tool, count)
	}
	if total > 0 {
		fmt.Fprintf(stdout, "empty-result rate: %d%%\n", empty*100/total)
		fmt.Fprintf(stdout, "latency p50: %dms\n", Percentile(latencies, 50))
	}
	return nil
}

// Percentile returns the nearest-rank value using Mora's established integer index.
func Percentile(values []int64, percentile int) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[(percentile*(len(sorted)-1))/100]
}

// Enabled reports whether local usage recording is allowed.
func Enabled(cfg Config) bool {
	if os.Getenv("DO_NOT_TRACK") == "1" {
		return false
	}
	_, err := os.Stat(filepath.Join(cfg.StateDir, "usage", "OFF"))
	return err != nil
}

// QueryLoggingEnabled reports whether raw query retention was explicitly enabled.
func QueryLoggingEnabled(cfg Config) bool {
	if os.Getenv("MORA_LOG_QUERIES") == "1" {
		return true
	}
	_, err := os.Stat(filepath.Join(cfg.StateDir, "usage", "QUERIES"))
	return err == nil
}

// Log appends one independent JSONL event, best-effort.
func Log(cfg Config, event Event) {
	if !Enabled(cfg) {
		return
	}
	if !QueryLoggingEnabled(cfg) {
		event.Query = ""
	}
	event.TS = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(event)
	if err != nil {
		return
	}
	appendMu.Lock()
	defer appendMu.Unlock()
	_ = atomicio.AppendFile(filepath.Join(cfg.StateDir, "usage", "events.jsonl"), string(b)+"\n")
}
