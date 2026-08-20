package mora

import (
	"time"

	"github.com/pyranthus-hq/mora/internal/briefartifact"
)

func briefArtifactPath(cfg Config, now time.Time) string {
	return briefartifact.Path(cfg.VaultDir, now)
}

func writeBriefArtifact(cfg Config, d Digest, now time.Time) (string, error) {
	return writeBriefArtifactAt(cfg, d, now, contextDefaultTokens(cfg)*charsPerToken)
}

func writeBriefArtifactAt(cfg Config, d Digest, now time.Time, budgetChars int) (string, error) {
	return briefartifact.Write(cfg.VaultDir, now, []byte(renderDigest(d, budgetChars)))
}
