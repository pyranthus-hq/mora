package mora

import ingestpkg "github.com/pyranthus-hq/mora/internal/ingest"

func ingestSourceKey(provider, account string) string           { return ingestpkg.SourceKey(provider, account) }
func ingestJournalPath(cfg Config, key string) string           { return ingestpkg.JournalPath(cfg, key) }
func ingestLeasePath(cfg Config, key string) string             { return ingestpkg.LeasePath(cfg, key) }
func ingestJournalStatus(cfg Config) (bool, int, string, error) { return ingestpkg.JournalStatus(cfg) }
func ensureIngestJournalHeader(cfg Config, key string) error {
	return ingestpkg.EnsureJournalHeader(cfg, key, ingestPublishSeams())
}

func releaseIngestLeasesOwnedHere(cfg Config) {
	ingestpkg.ReleaseLeasesOwnedHere(cfg, ingestLeaseSeams())
}
