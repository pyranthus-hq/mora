package mora

import (
	"fmt"
	"io"
	"time"

	healthpkg "github.com/pyranthus-hq/mora/internal/health"
)

// health_envelope.go keeps composition and rendering adapters in Mora while
// internal/health owns the bounded deterministic projection.
const (
	compactSourceCap      = healthpkg.CompactSourceCap
	compactSourceBytesCap = healthpkg.CompactSourceBytesCap
)

type compactHealth = healthpkg.Compact

func compactHealthFrom(h Health) compactHealth { return healthpkg.ProjectCompact(h) }
func healthFromParts(sources []sourceHealth, idx indexHealth, producers []producerHealth) Health {
	return healthpkg.FromParts(sources, idx, producers)
}
func compactHealthOf(cfg Config, now time.Time) compactHealth {
	return compactHealthFrom(healthOf(cfg, now))
}
func printHealthBannerLine(w io.Writer, cfg Config, now time.Time) {
	if banner := healthBanner(cfg, now); banner != "" {
		fmt.Fprintln(w, banner)
	}
}
