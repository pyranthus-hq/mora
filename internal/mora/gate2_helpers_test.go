package mora

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// gate2_helpers_test.go — shared fixtures for the Gate 2 (Packet A + B) tests.

// gate2Vault seeds a populated, identity-bound, freshly-rebuilt vault (the static
// embedder is forced) — a FRESH index to mutate from.
func gate2Vault(t *testing.T, mems ...Memory) Config {
	t.Helper()
	if len(mems) == 0 {
		mems = []Memory{coreBIdxmem("mem_a", "global", "insight", "Alpha", "alpha body one")}
	}
	return coreBIdxpopulatedVault(t, "v_gate2", mems)
}

// gate2ReadMeta loads the whole index_meta table.
func gate2ReadMeta(t *testing.T, cfg Config) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := readIndexMeta(db)
	if err != nil {
		t.Fatalf("read index_meta: %v", err)
	}
	return m
}

// gate2IndexState computes the index arm state against a fixed clock.
func gate2IndexState(t *testing.T, cfg Config) string {
	t.Helper()
	return indexHealthOf(cfg, time.Unix(1_700_000_000, 0)).State
}

// gate2PinClock pins indexClock to a fixed instant for the duration of a test, so
// marked_at / listing_started_at / index_meta stamps are deterministic.
func gate2PinClock(t *testing.T, at time.Time) {
	t.Helper()
	orig := indexClock
	indexClock = func() time.Time { return at }
	t.Cleanup(func() { indexClock = orig })
}

// gate2Search runs the FTS search path.
func gate2Search(t *testing.T, cfg Config, q string) []Memory {
	t.Helper()
	res, err := searchMemories(context.Background(), cfg, q, "", 8)
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	return res
}
