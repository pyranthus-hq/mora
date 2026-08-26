package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	usagestore "github.com/pyranthus-hq/mora/internal/usage"
)

// usageStateReceipt is the machine form of a usage-tracking toggle. These
// commands were silent on success; the receipt is the only stdout they produce,
// and only under --json.
type usageStateReceipt struct {
	Setting string `json:"setting"`
	Enabled bool   `json:"enabled"`
}

func cmdUsage(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := loadConfigFor(ctx)
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
		return emitState("off", false, atomicio.Write(filepath.Join(cfg.StateDir, "usage", "OFF"), []byte("off\n"), 0o600))
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
			return emitState("queries.on", true, atomicio.Write(marker, []byte("on\n"), 0o600))
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

func usageConfig(cfg Config) usagestore.Config { return usagestore.Config{StateDir: cfg.StateDir} }

type usageEvent = usagestore.Event
type usagePhaseTimings = usagestore.PhaseTimings

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
	if !jsonOut {
		return usagestore.Report(usageConfig(cfg), stdout)
	}
	b, err := os.ReadFile(filepath.Join(cfg.StateDir, "usage", "events.jsonl"))
	if err != nil {
		return emitReceipt(stdout, "mora.usage.report", 1, usageReportPayload{
			Window: "all recorded usage", CallsByTool: make(map[string]int), TrackingDisabled: !usagestore.Enabled(usageConfig(cfg)),
		})
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
	payload := usageReportPayload{
		Window: "all recorded usage", TotalCalls: total, CallsByTool: byTool,
		TrackingDisabled: !usagestore.Enabled(usageConfig(cfg)),
	}
	if total > 0 {
		payload.EmptyResultRate = empty * 100 / total
		payload.LatencyP50Millis = usagestore.Percentile(latencies, 50)
	}
	return emitReceipt(stdout, "mora.usage.report", 1, payload)
}
func logUsage(cfg Config, event usageEvent) { usagestore.Log(usageConfig(cfg), event) }
