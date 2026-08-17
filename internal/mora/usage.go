package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// usageAppendMu keeps each in-process event as one independent JSONL record.
// MCP stdio is sequential today, but the loopback HTTP bridge and tests can call
// the same dispatcher concurrently through separate file descriptors.
var usageAppendMu sync.Mutex

// usageStateReceipt is the machine form of a usage-tracking toggle. These
// commands were silent on success; the receipt is the only stdout they produce,
// and only under --json.
type usageStateReceipt struct {
	Setting string `json:"setting"`
	Enabled bool   `json:"enabled"`
}

func cmdUsage(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	jsonOut := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" && (len(args) == 0 || args[0] != "report") {
			jsonOut = true
			continue
		}
		rest = append(rest, a)
	}
	if len(args) > 0 && args[0] != "report" {
		args = rest
	}
	emitState := func(setting string, enabled bool, err error) error {
		if err != nil {
			return err
		}
		if !jsonOut {
			return nil
		}
		return emitReceipt(stdout, "mora.usage."+setting, 1, usageStateReceipt{Setting: setting, Enabled: enabled})
	}
	switch {
	case len(args) >= 1 && args[0] == "off":
		return emitState("off", false, atomicWrite(filepath.Join(cfg.StateDir, "usage", "OFF"), []byte("off\n"), 0o600))
	case len(args) >= 1 && args[0] == "on":
		if err := os.Remove(filepath.Join(cfg.StateDir, "usage", "OFF")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return emitState("on", true, nil)
	case len(args) >= 1 && args[0] == "report":
		fs := flag.NewFlagSet("usage report", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "emit JSON")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return newMoraError(errCodeUsageUnknownFlag, "usage", parseErr, "%v", parseErr)
		}
		if fs.NArg() != 0 {
			return newMoraError(errCodeUsageUnknownValue, "usage", nil, "unexpected argument %q", fs.Arg(0))
		}
		return usageReport(cfg, stdout, *jsonOut)
	case len(args) >= 1 && args[0] == "queries":
		// Opt in/out of retaining the raw query string in the local usage log
		// (default OFF). Mirrors the OFF-marker pattern; never affects egress.
		marker := filepath.Join(cfg.StateDir, "usage", "QUERIES")
		switch {
		case len(args) >= 2 && args[1] == "on":
			return emitState("queries.on", true, atomicWrite(marker, []byte("on\n"), 0o600))
		case len(args) >= 2 && args[1] == "off":
			if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return emitState("queries.off", false, nil)
		default:
			return errors.New("usage: mora usage queries <on|off>")
		}
	default:
		return errors.New("usage: mora usage report|off|on|queries <on|off>")
	}
}

type usageReportPayload struct {
	Window           string         `json:"window"`
	TotalCalls       int            `json:"total_calls"`
	CallsByTool      map[string]int `json:"calls_by_tool"`
	EmptyResultRate  int            `json:"empty_result_rate_percent"`
	LatencyP50Millis int64          `json:"latency_p50_millis"`
	TrackingDisabled bool           `json:"tracking_disabled"`
}

func usageReport(cfg Config, stdout io.Writer, jsonOutput ...bool) error {
	jsonOut := len(jsonOutput) > 0 && jsonOutput[0]
	b, err := os.ReadFile(filepath.Join(cfg.StateDir, "usage", "events.jsonl"))
	if err != nil {
		if jsonOut {
			return emitReceipt(stdout, "mora.usage.report", 1, usageReportPayload{
				Window: "all recorded usage", CallsByTool: make(map[string]int), TrackingDisabled: !usageEnabled(cfg),
			})
		}
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
		var e usageEvent
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		byTool[e.Tool]++
		total++
		if e.Results == 0 {
			empty++
		}
		latencies = append(latencies, e.Millis)
	}
	if jsonOut {
		payload := usageReportPayload{
			Window: "all recorded usage", TotalCalls: total, CallsByTool: byTool,
			TrackingDisabled: !usageEnabled(cfg),
		}
		if total > 0 {
			payload.EmptyResultRate = empty * 100 / total
			payload.LatencyP50Millis = percentile(latencies, 50)
		}
		return emitReceipt(stdout, "mora.usage.report", 1, payload)
	}
	fmt.Fprintf(stdout, "Mora usage (content-free)\n")
	fmt.Fprintf(stdout, "total calls: %d\n", total)
	for tool, n := range byTool {
		fmt.Fprintf(stdout, "  %s: %d\n", tool, n)
	}
	if total > 0 {
		fmt.Fprintf(stdout, "empty-result rate: %d%%\n", empty*100/total)
		fmt.Fprintf(stdout, "latency p50: %dms\n", percentile(latencies, 50))
	}
	return nil
}
func percentile(v []int64, p int) int64 {
	if len(v) == 0 {
		return 0
	}
	sorted := append([]int64(nil), v...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (p * (len(sorted) - 1)) / 100
	return sorted[idx]
}
func usageEnabled(cfg Config) bool {
	if os.Getenv("DO_NOT_TRACK") == "1" {
		return false
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "usage", "OFF")); err == nil {
		return false
	}
	return true
}

// queryLoggingEnabled reports whether the raw query string may be retained in the
// local usage log. OFF by default — the log keeps tool, scope, result counts and
// timing, but NOT what you searched for, unless you opt in with `mora usage queries
// on` (or MORA_LOG_QUERIES=1). This governs on-disk retention only; Mora never
// transmits the usage log anywhere.
func queryLoggingEnabled(cfg Config) bool {
	if os.Getenv("MORA_LOG_QUERIES") == "1" {
		return true
	}
	_, err := os.Stat(filepath.Join(cfg.StateDir, "usage", "QUERIES"))
	return err == nil
}
func logUsage(cfg Config, e usageEvent) {
	if !usageEnabled(cfg) {
		return
	}
	if !queryLoggingEnabled(cfg) {
		// Privacy default: drop the raw query text (search strings, and a person's
		// name on graph/get_entity) so "no telemetry" isn't undercut by a local log
		// of what you searched. Counts/timing/scope still recorded for `usage report`.
		e.Query = ""
	}
	e.TS = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	usageAppendMu.Lock()
	defer usageAppendMu.Unlock()
	_ = appendFile(filepath.Join(cfg.StateDir, "usage", "events.jsonl"), string(b)+"\n")
}
