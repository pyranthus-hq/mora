package mora

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// benchSeedIndexedVault writes n authored memories into a fresh sandbox vault,
// binds identity, and builds the full index once. It returns the config with the
// index already at steady state, ready for either a single incremental upsert or a
// full rebuild to be measured against it.
func benchSeedIndexedVault(b *testing.B, n int) Config {
	b.Helper()
	b.Setenv("MORA_EMBEDDER", "") // deterministic static embedder
	// Inline sandbox setup (sandboxCfg takes *testing.T; benchmarks pass *testing.B).
	b.Setenv("MORA_CONFIG_DIR", b.TempDir())
	cfg := defaultConfig()
	for _, d := range []string{cfg.VaultDir, cfg.DataDir, cfg.StateDir, cfg.ConfigDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		m := coreBIdxmem(fmt.Sprintf("mem_bench_%05d", i), "global", "insight",
			fmt.Sprintf("Bench %05d", i),
			fmt.Sprintf("body number %d referencing [[Topic %d]] and [[Shared Topic]]", i, i%16))
		if err := writeMemory(cfg, m); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_bench"); err != nil {
		b.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		b.Fatal(err)
	}
	return cfg
}

// BenchmarkIndexUpsert1k measures the steady-state cost of reflecting ONE authored
// write into an index that already holds ~1k memories — the hot path this feature
// replaces. It repeatedly upserts a single already-present id (replace-in-place).
// Each iteration still includes a full FTS delete-scan and a COUNT(*) over the ~1k
// rows, so this is the large constant-factor win over a full rebuild, not an
// asymptotically O(1) per-write cost (compare BenchmarkRebuildIndex1k).
func BenchmarkIndexUpsert1k(b *testing.B) {
	cfg := benchSeedIndexedVault(b, 1000)
	ctx := context.Background()
	m := coreBIdxmem("mem_bench_00000", "global", "insight", "Bench 00000",
		"updated body referencing [[Topic 0]] and [[Shared Topic]]")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := indexUpsert(ctx, cfg, m); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRebuildIndex1k measures the cost of the OLD write path: a full
// DELETE-then-reinsert rebuild of the whole ~1k-memory vault (memories + FTS + entity
// graph + vectors) — the work every `mora write` used to trigger. Compare against
// BenchmarkIndexUpsert1k to see the per-write asymptotic win.
func BenchmarkRebuildIndex1k(b *testing.B) {
	cfg := benchSeedIndexedVault(b, 1000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rebuildIndex(ctx, cfg); err != nil {
			b.Fatal(err)
		}
	}
}
