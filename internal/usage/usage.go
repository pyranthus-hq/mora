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

// Report writes aggregate, content-free usage statistics. The original
// headline fields stay first for backwards-compatible terminal parsing; the
// scorecard below them makes per-tool regressions visible without retaining
// request or response content.
func Report(cfg Config, stdout io.Writer) error {
	b, err := os.ReadFile(filepath.Join(cfg.StateDir, "usage", "events.jsonl"))
	if err != nil {
		fmt.Fprintln(stdout, "no usage recorded")
		return nil
	}
	byTool := map[string]*toolScore{}
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
		score := byTool[event.Tool]
		if score == nil {
			score = &toolScore{name: event.Tool}
			byTool[event.Tool] = score
		}
		score.calls++
		if emptyResultMeaningful(event.Tool) {
			score.emptyCalls += boolInt(event.Results == 0)
		}
		score.latencies = append(score.latencies, event.Millis)
		if event.OutputBytes > 0 {
			score.outputTokens = append(score.outputTokens, estimateTokens(event.OutputBytes))
		}
		total++
		if event.Results == 0 {
			empty++
		}
		latencies = append(latencies, event.Millis)
	}
	fmt.Fprintln(stdout, "Mora usage (content-free)")
	fmt.Fprintf(stdout, "total calls: %d\n", total)
	tools := make([]string, 0, len(byTool))
	for tool := range byTool {
		tools = append(tools, tool)
	}
	sort.Strings(tools)
	for _, tool := range tools {
		fmt.Fprintf(stdout, "  %s: %d\n", tool, byTool[tool].calls)
	}
	if total > 0 {
		fmt.Fprintf(stdout, "empty-result rate: %d%%\n", empty*100/total)
		fmt.Fprintf(stdout, "latency p50: %dms\n", Percentile(latencies, 50))
	}
	if len(tools) > 0 {
		fmt.Fprintln(stdout, "per-tool scorecard:")
		for _, tool := range tools {
			score := byTool[tool]
			fmt.Fprintf(stdout, "  %s: calls=%d empty=%s latency_p50/p95=%dms/%dms output_tokens_p50/p95=%s\n",
				tool,
				score.calls,
				score.emptyRate(),
				Percentile(score.latencies, 50),
				Percentile(score.latencies, 95),
				score.outputTokenSummary(),
			)
		}
	}
	return nil
}

type toolScore struct {
	name         string
	calls        int
	emptyCalls   int
	latencies    []int64
	outputTokens []int64
}

func (s toolScore) emptyRate() string {
	if !emptyResultMeaningful(s.name) {
		return "n/a"
	}
	return fmt.Sprintf("%d%%", s.emptyCalls*100/s.calls)
}

func (s toolScore) outputTokenSummary() string {
	if len(s.outputTokens) == 0 {
		return fmt.Sprintf("n/a (0/%d events)", s.calls)
	}
	return fmt.Sprintf("%d/%d tok (%d/%d events)",
		Percentile(s.outputTokens, 50),
		Percentile(s.outputTokens, 95),
		len(s.outputTokens), s.calls,
	)
}

// emptyResultMeaningful identifies tools whose Results field is a returned
// collection or found/not-found receipt. Mutation and point-summary tools use
// Results for success/fallback bookkeeping, so reporting an "empty" rate for
// them would be misleading.
func emptyResultMeaningful(tool string) bool {
	switch tool {
	case "search_memory", "list_memory", "context_memory", "think", "list_entities", "read_memory", "digest", "brief", "meeting_prep":
		return true
	default:
		return false
	}
}

// estimateTokens is deliberately a byte heuristic, not a model tokenizer. It
// keeps the report deterministic and makes its output comparable with Mora's
// existing token budgeting convention.
func estimateTokens(outputBytes int) int64 {
	return int64((outputBytes + 3) / 4)
}

// Percentile returns the nearest-rank value. At p50 this keeps Mora's existing
// lower-median behavior; at p95 it correctly reports the tail even for small
// per-tool samples.
func Percentile(values []int64, percentile int) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if percentile <= 0 {
		return sorted[0]
	}
	if percentile >= 100 {
		return sorted[len(sorted)-1]
	}
	index := (percentile*len(sorted) + 99) / 100
	return sorted[index-1]
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
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
