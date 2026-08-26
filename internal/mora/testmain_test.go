package mora

import (
	"os"
	"testing"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	configstore "github.com/pyranthus-hq/mora/internal/config"
)

// TestMain replaces the two crash-durability barriers (`atomicio.WriteDurable`'s
// pre-rename data fsync and its post-rename parent-directory fsync) with no-ops
// for this package's tests, unless MORA_TEST_REAL_FSYNC=1 asks for the real
// ones.
//
// WHY this does not weaken any assertion:
//
//   - An fsync is UNOBSERVABLE from userspace. A process crash preserves
//     page-cache data, so no in-process test can distinguish a synced write
//     from an unsynced one — which is exactly why the durability gates are
//     CALL-TRACE assertions through the MarkerSyncFn/SyncDirFn seams rather
//     than real crash tests. Those traces still fire here: withMarkerTrace
//     (gate2_durable_test.go) captures the CURRENT value and wraps it, so it
//     records the same fsync/dirsync event ORDER against a no-op inner
//     function. Deleting the production call in WriteDurable still turns those
//     gates RED, so the mutation-matrix rows stay load-bearing.
//   - The tests that stub a barrier to an ERROR (loop_test.go,
//     update_policy_test.go, update_unattended_test.go) assign their own
//     function outright and are unaffected by the default.
//   - The REAL barriers are still exercised, in internal/atomicio's own package
//     tests, where these seams keep their production defaults.
//
// WHY it matters: on darwin `(*os.File).Sync` is `fcntl(F_FULLFSYNC)` — a true
// device barrier costing milliseconds. A single `mora init` performs 24 of them
// (12 file + 12 directory), which measured at ~118 ms of its ~136 ms cost;
// ~651 tests call `run(t, "init")`. Paying for device-level crash durability
// against a directory that `t.TempDir()` deletes moments later buys nothing.
func TestMain(m *testing.M) {
	// Capture the REAL resolved layout before any test can touch the process
	// environment; loadConfig/loadConfigFor use it as the hermeticity tripwire
	// (a test resolving this exact layout fails loud instead of silently
	// touching the developer's real vault/index/state). A process that STARTS
	// with MORA_CONFIG_DIR pinned or the subprocess marker set (child helpers
	// re-exec the test binary with HOME pointed at the parent's sandbox) is
	// already explicitly sandboxed — capturing its pinned layout would make
	// every legitimate resolution look like a leak, so the tripwire stays
	// unarmed.
	if os.Getenv("MORA_CONFIG_DIR") == "" && os.Getenv("MORA_TEST_SUBPROCESS") == "" {
		if cfg, err := configstore.Load(); err == nil {
			realHomeConfig.Store(&cfg)
		}
	}
	if os.Getenv("MORA_TEST_REAL_FSYNC") != "1" {
		atomicio.MarkerSyncFn = func(*os.File) error { return nil }
		atomicio.SyncDirFn = func(string) error { return nil }
	}
	os.Exit(m.Run())
}
