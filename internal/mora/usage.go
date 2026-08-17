package mora

import (
	"context"
	"encoding/json"
	"errors"
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

func cmdUsage(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch {
	case len(args) >= 1 && args[0] == "off":
		return atomicWrite(filepath.Join(cfg.StateDir, "usage", "OFF"), []byte("off\n"), 0o600)
	case len(args) >= 1 && args[0] == "on":
		return os.Remove(filepath.Join(cfg.StateDir, "usage", "OFF"))
	case len(args) >= 1 && args[0] == "report":
		return usageReport(cfg, stdout)
	case len(args) >= 1 && args[0] == "queries":
		// Opt in/out of retaining the raw query string in the local usage log
		// (default OFF). Mirrors the OFF-marker pattern; never affects egress.
		marker := filepath.Join(cfg.StateDir, "usage", "QUERIES")
		switch {
		case len(args) >= 2 && args[1] == "on":
			return atomicWrite(marker, []byte("on\n"), 0o600)
		case len(args) >= 2 && args[1] == "off":
			if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		default:
			return errors.New("usage: mora usage queries <on|off>")
		}
	default:
		return errors.New("usage: mora usage report|off|on|queries <on|off>")
	}
}
func usageReport(cfg Config, stdout io.Writer) error {
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
