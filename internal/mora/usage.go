package mora

import (
	"context"
	"errors"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	usagestore "github.com/pyranthus-hq/mora/internal/usage"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

func cmdUsage(ctx context.Context, args []string, stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch {
	case len(args) >= 1 && args[0] == "off":
		return atomicio.Write(filepath.Join(cfg.StateDir, "usage", "OFF"), []byte("off\n"), 0o600)
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
			return atomicio.Write(marker, []byte("on\n"), 0o600)
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

func usageConfig(cfg Config) usagestore.Config { return usagestore.Config{StateDir: cfg.StateDir} }

type usageEvent = usagestore.Event
type usagePhaseTimings = usagestore.PhaseTimings

func usageReport(cfg Config, stdout io.Writer) error {
	return usagestore.Report(usageConfig(cfg), stdout)
}
func logUsage(cfg Config, event usageEvent) { usagestore.Log(usageConfig(cfg), event) }
