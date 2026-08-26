package mora

import (
	"context"
	"sync"
	"testing"
	"time"

	configstore "github.com/pyranthus-hq/mora/internal/config"
)

// testctx_test.go — per-test isolation without process-global state.
//
// History: this package's tests isolated themselves by calling t.Setenv to
// point HOME/USERPROFILE (and sometimes MORA_CONFIG_DIR / MORA_VAULT) at a temp
// directory. Go forbids t.Setenv in parallel tests, so isolation and
// parallelism were mutually exclusive by construction: 5 of ~1,500 tests ran
// parallel (#319).
//
// The mechanism now is context injection. A test binds a testEnv to its
// *testing.T; every CLI/MCP entry point in the package resolves its Config via
// configstore.LoadFrom(testCtx(t)), which carries the injected root, clock,
// embedder preference, and reconciler seam through context values. Nothing
// process-global is written per-test, so tests that inject distinct roots can
// run concurrently. Tests that genuinely exercise env resolution keep using
// t.Setenv (withTempHomeSetenv and friends) and stay serial — Go's harness
// enforces that, exactly as before.

// testEnv is one test's isolation bundle.
type testEnv struct {
	home         string // WithHomeRoot-equivalent root (HOME-style layout)
	configRoot   string // WithConfigRoot-equivalent root (MORA_CONFIG_DIR-style layout)
	clock        func() time.Time
	reconciler   func(context.Context, Config) error
	embedderPref string
	embedderSet  bool
}

// ctx materializes the bundle as context values for configstore.LoadFrom.
func (e *testEnv) ctx() context.Context {
	ctx := context.Background()
	if e.configRoot != "" {
		ctx = configstore.WithConfigRoot(ctx, e.configRoot)
	} else {
		ctx = configstore.WithHomeRoot(ctx, e.home)
	}
	if e.clock != nil {
		ctx = configstore.WithOperationClock(ctx, e.clock)
	}
	if e.reconciler != nil {
		ctx = configstore.WithAuthoredReconciler(ctx, e.reconciler)
	}
	if e.embedderSet {
		ctx = configstore.WithEmbedderPref(ctx, e.embedderPref)
	}
	return ctx
}

var testEnvRegistry sync.Map // *testing.T -> *testEnv

func bindTestEnv(t *testing.T, e *testEnv) {
	testEnvRegistry.Store(t, e)
	t.Cleanup(func() { testEnvRegistry.Delete(t) })
}

func lookupTestEnv(t *testing.T) *testEnv {
	if v, ok := testEnvRegistry.Load(t); ok {
		return v.(*testEnv)
	}
	return nil
}

// testCtx returns the context CLI/MCP entry invocations must use. It carries
// the caller's injected environment when one is bound; otherwise it is plain
// background — the hermeticity tripwire in loadConfig/loadConfigFor fails loud
// if such a call path then resolves the REAL home layout.
func testCtx(t *testing.T) context.Context {
	if e := lookupTestEnv(t); e != nil {
		return e.ctx()
	}
	return context.Background()
}

// subRun is t.Run with environment inheritance: a subtest gets a fresh
// *testing.T, which would miss the parent's registry entry; subRun re-binds the
// parent's env under the child handle first. Every t.Run in this package goes
// through it so nested CLI calls stay inside the parent's sandbox.
func subRun(t *testing.T, name string, f func(t *testing.T)) {
	t.Helper()
	t.Run(name, func(st *testing.T) {
		if e := lookupTestEnv(t); e != nil {
			// Value-copy: a child adjusting its bundle must never mutate the
			// parent's (or a sibling's) environment out from under them.
			child := *e
			bindTestEnv(st, &child)
		}
		f(st)
	})
}

// pinOperationClockForTest pins the operation-activity time source for the
// caller's environment instead of swapping a package global, so two tests may
// pin different clocks concurrently.
func pinOperationClockForTest(t *testing.T, base time.Time) {
	t.Helper()
	e := lookupTestEnv(t)
	if e == nil {
		t.Fatalf("pinOperationClockForTest: no test environment bound; call withTempHome/sandboxCfg first")
		return
	}
	var mu sync.Mutex
	var tick int64
	e.clock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		tick++
		return base.Add(time.Duration(tick))
	}
}

// withTempHomeSetenv is the LEGACY env-based wrapper, kept for the genuinely
// env-global tests that assert how Mora reads process environment variables.
// It sets BOTH HOME and USERPROFILE because os.UserHomeDir reads USERPROFILE on
// Windows and HOME elsewhere; blanking MORA_CONFIG_DIR/MORA_VAULT keeps a
// developer's exported overrides from leaking a real vault into the test.
// Tests using it cannot run in parallel — Go's harness forbids the combination.
func withTempHomeSetenv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // os.UserHomeDir reads %USERPROFILE% on Windows
	t.Setenv("MORA_CONFIG_DIR", "")
	t.Setenv("MORA_VAULT", "")
}
