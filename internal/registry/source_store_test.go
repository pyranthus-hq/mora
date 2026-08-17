package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"github.com/pyranthus-hq/mora/internal/memory"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testSourceStore(cfg config.Config) SourceStore { return SourceStore{ConfigDir: cfg.ConfigDir} }
func acquireSourcesLock(cfg config.Config, now time.Time) (func(), error) {
	return testSourceStore(cfg).Acquire(now)
}
func mutateSources(cfg config.Config, fn func([]memory.Source) ([]memory.Source, error)) error {
	return testSourceStore(cfg).Mutate(fn)
}
func sourcesLockPath(cfg config.Config) string { return testSourceStore(cfg).LockPath() }
func setSourceEnabledByName(cfg config.Config, name string, enabled bool) error {
	return testSourceStore(cfg).Mutate(func(s []memory.Source) ([]memory.Source, error) {
		for i := range s {
			if s[i].Name == name {
				s[i].Enabled = genericutil.Ptr(enabled)
				return s, nil
			}
		}
		return s, fmt.Errorf("source %q not found", name)
	})
}
func reapStaleLockTTL(path string, now time.Time, ttl time.Duration) (bool, error) {
	return leasefile.Reap(path, now, ttl, leasefile.DefaultRemovalOptions())
}

// sources_lock_test.go pins the P3 fix: the sources.json read-modify-write is
// serialized by a crash-safe cross-process lease (mutateSources /
// acquireSourcesLock), so two concurrent writers can no longer lose an update.

func sourcesNameSet(sources []memory.Source) map[string]bool {
	set := make(map[string]bool, len(sources))
	for _, s := range sources {
		set[s.Name] = true
	}
	return set
}

// TestSourcesLockSerializesRMW is the deterministic interleaving proof: writer A
// holds the lease mid-RMW while writer B attempts a full mutateSources cycle. B
// must BLOCK on the lease; once A commits its own mutation and releases, B
// reloads INSIDE the lease (seeing A's write) and commits its own. BOTH survive
// — the classic lost update (last-writer-wins) is closed. The outcome is
// deterministic regardless of scheduler timing; only the "B is still blocked"
// guard uses a short poll (well under B's ~2s acquire budget).
func TestSourcesLockSerializesRMW(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}
	if err := SaveSources(cfg, nil); err != nil {
		t.Fatalf("seed empty registry: %v", err)
	}

	// A takes the lease and holds it (an in-progress RMW that has not saved yet).
	relA, err := acquireSourcesLock(cfg, time.Now())
	if err != nil {
		t.Fatalf("A acquire: %v", err)
	}

	bDone := make(chan error, 1)
	go func() {
		bDone <- mutateSources(cfg, func(s []memory.Source) ([]memory.Source, error) {
			return append(s, memory.Source{Name: "B", Type: "filesystem", Enabled: genericutil.Ptr(true)}), nil
		})
	}()

	// While A holds the lease, B must not proceed — that is the serialization
	// contract. A completion here means the lease failed to exclude.
	select {
	case err := <-bDone:
		relA()
		t.Fatalf("B completed while A held the lease (serialization broken): %v", err)
	case <-time.After(60 * time.Millisecond):
		// expected: B is spinning on the lease
	}

	// A does its own mutation (the interleave), saves, then releases.
	aSources, err := LoadSources(cfg)
	if err != nil {
		relA()
		t.Fatalf("A load: %v", err)
	}
	aSources = append(aSources, memory.Source{Name: "A", Type: "filesystem", Enabled: genericutil.Ptr(true)})
	if err := SaveSources(cfg, aSources); err != nil {
		relA()
		t.Fatalf("A save: %v", err)
	}
	relA()

	// B unblocks, reloads inside the lease (sees A), appends B, saves.
	if err := <-bDone; err != nil {
		t.Fatalf("B RMW: %v", err)
	}

	got, err := LoadSources(cfg)
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	names := sourcesNameSet(got)
	if !names["A"] || !names["B"] {
		t.Fatalf("lost update: both A and B must survive the interleaved RMW; got %v", names)
	}
}

// TestSourcesLockReapsStaleLock is the crash-recovery proof: a SIGKILL'd holder
// leaves its .lock behind (release never ran). A later RMW must reap the
// abandoned lease once it is older than the TTL and proceed — a crash never
// wedges the registry — while preserving the pre-existing row.
func TestSourcesLockReapsStaleLock(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}
	if err := SaveSources(cfg, []memory.Source{{Name: "pre", Type: "filesystem", Enabled: genericutil.Ptr(true)}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Plant an abandoned lease with an acquired_at older than the TTL.
	lockPath := sourcesLockPath(cfg)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	staleAt := time.Now().Add(-2 * SourceLockTTL).UTC().Format(time.RFC3339)
	body, _ := json.Marshal(leasefile.Body{PID: 999999, AcquiredAt: staleAt})
	if err := os.WriteFile(lockPath, body, 0o600); err != nil {
		t.Fatalf("plant stale lock: %v", err)
	}

	if err := mutateSources(cfg, func(s []memory.Source) ([]memory.Source, error) {
		return append(s, memory.Source{Name: "after-crash", Type: "filesystem", Enabled: genericutil.Ptr(true)}), nil
	}); err != nil {
		t.Fatalf("mutateSources over a stale lease should reap and proceed: %v", err)
	}

	got, err := LoadSources(cfg)
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	names := sourcesNameSet(got)
	if !names["pre"] || !names["after-crash"] {
		t.Fatalf("stale-lock reap must preserve 'pre' and add 'after-crash'; got %v", names)
	}
	// A clean RMW releases the lease — no leftover .lock wedges the next writer.
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lease should be released after a clean RMW; stat err=%v", err)
	}
}

// TestSourcesConcurrentRMWNoLostUpdate is the in-process regression guard: N
// goroutines each flip a DISTINCT row's enable bit at the same time. With the
// lease + reload-inside-lock every flip survives; a bare load->mutate->save would
// keep only the last writer's flip. Run under -race in CI. (Cross-process replay:
// TestSourcesRMWNoLostUpdateAcrossProcesses.)
func TestSourcesConcurrentRMWNoLostUpdate(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}
	const n = 8
	seed := make([]memory.Source, n)
	for i := range seed {
		seed[i] = memory.Source{Name: fmt.Sprintf("s%d", i), Type: "filesystem", Enabled: genericutil.Ptr(false)}
	}
	if err := SaveSources(cfg, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = setSourceEnabledByName(cfg, fmt.Sprintf("s%d", i), true)
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("s%d enable: %v", i, e)
		}
	}
	got, err := LoadSources(cfg)
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	if len(got) != n {
		t.Fatalf("row count changed under concurrent RMW: got %d want %d", len(got), n)
	}
	for _, s := range got {
		if s.Enabled == nil || !*s.Enabled {
			t.Fatalf("lost update: %s not enabled after %d concurrent RMWs", s.Name, n)
		}
	}
}

// TestSourcesRMWNoLostUpdateAcrossProcesses is the historical sources.json
// lost-update incident replay through REAL PROCESSES (not goroutines). N
// subprocesses each flip a distinct enable bit against one shared ConfigDir;
// with the lease every flip survives.
func TestSourcesRMWNoLostUpdateAcrossProcesses(t *testing.T) {
	if role := os.Getenv("MORA_SOURCES_MP_ROLE"); role != "" {
		cfgDir := os.Getenv("MORA_SOURCES_MP_CONFIG")
		name := os.Getenv("MORA_SOURCES_MP_NAME")
		cfg := config.Config{ConfigDir: cfgDir}
		if err := setSourceEnabledByName(cfg, name, true); err != nil {
			fmt.Fprintf(os.Stderr, "enable %s: %v\n", name, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	cfg := config.Config{ConfigDir: t.TempDir()}
	const n = 8
	seed := make([]memory.Source, n)
	for i := range seed {
		seed[i] = memory.Source{Name: fmt.Sprintf("s%d", i), Type: "filesystem", Enabled: genericutil.Ptr(false)}
	}
	if err := SaveSources(cfg, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	outs := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestSourcesRMWNoLostUpdateAcrossProcesses$", "-test.count=1")
			cmd.Env = append(os.Environ(),
				"MORA_SOURCES_MP_ROLE=1",
				"MORA_SOURCES_MP_CONFIG="+cfg.ConfigDir,
				fmt.Sprintf("MORA_SOURCES_MP_NAME=s%d", i),
			)
			out, err := cmd.CombinedOutput()
			outs[i] = string(out)
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("child s%d: %v\n%s", i, e, outs[i])
		}
	}
	got, err := LoadSources(cfg)
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	if len(got) != n {
		t.Fatalf("row count changed under cross-process RMW: got %d want %d", len(got), n)
	}
	for _, s := range got {
		if s.Enabled == nil || !*s.Enabled {
			t.Fatalf("lost update across processes: %s not enabled", s.Name)
		}
	}
}

// TestAcquireSourcesLockReapsExpired isolates the reap-on-acquire path with an
// injected now (mirrors loop_test's TIER D): an expired planted lease is reaped
// and the lease becomes ours.
func TestAcquireSourcesLockReapsExpired(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}
	lockPath := sourcesLockPath(cfg)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now()
	expired, _ := json.Marshal(leasefile.Body{PID: 999999, AcquiredAt: now.Add(-SourceLockTTL - time.Second).UTC().Format(time.RFC3339)})
	if err := os.WriteFile(lockPath, expired, 0o600); err != nil {
		t.Fatalf("plant: %v", err)
	}

	release, err := acquireSourcesLock(cfg, now)
	if err != nil {
		t.Fatalf("acquire over expired lease should succeed: %v", err)
	}
	defer release()

	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lease: %v", err)
	}
	var lb leasefile.Body
	if err := json.Unmarshal(data, &lb); err != nil {
		t.Fatalf("unmarshal lease: %v", err)
	}
	if lb.PID != os.Getpid() {
		t.Fatalf("after reap the lease should be ours (pid %d), got pid %d", os.Getpid(), lb.PID)
	}
}

// TestReapStaleSourcesLockFreshNotReaped pins the TTL boundary: a FRESH lease
// (acquired_at within the TTL) is never reaped, so a live holder is not stolen —
// liveness of the holder's pid is irrelevant, only the TTL bounds abandonment.
func TestReapStaleSourcesLockFreshNotReaped(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}
	lockPath := sourcesLockPath(cfg)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now()
	fresh, _ := json.Marshal(leasefile.Body{PID: 999999, AcquiredAt: now.UTC().Format(time.RFC3339)})
	if err := os.WriteFile(lockPath, fresh, 0o600); err != nil {
		t.Fatalf("plant: %v", err)
	}
	reaped, err := reapStaleLockTTL(lockPath, now, SourceLockTTL)
	if err != nil || reaped {
		t.Fatalf("a fresh sources lease must not be reaped: reaped=%v err=%v", reaped, err)
	}
}

func TestSourceStoreDeadlineAndErrorBranches(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	bad := SourceStore{ConfigDir: filepath.Join(parent, "config")}
	if _, err := bad.Acquire(time.Now()); err == nil {
		t.Fatal("mkdir error swallowed")
	}
	if err := bad.Mutate(func(s []memory.Source) ([]memory.Source, error) { return s, nil }); err == nil {
		t.Fatal("mutate acquire error swallowed")
	}
	boom := errors.New("boom")
	if _, err := (SourceStore{ConfigDir: t.TempDir(), Publish: func(string, []byte) (bool, error) { return false, boom }}).Acquire(time.Now()); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wall := base
	reapErr := SourceStore{ConfigDir: t.TempDir(), WallNow: func() time.Time { return wall }, Sleep: func(d time.Duration) { wall = wall.Add(d) }, Backoff: func(int) time.Duration { return time.Second }, PID: func() int { return 7 }, Publish: func(string, []byte) (bool, error) { return false, nil }, Reap: func(string, time.Time, time.Duration, leasefile.RemovalOptions) (bool, error) { return false, boom }}
	if _, err := reapErr.Acquire(base); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	dir := t.TempDir()
	holder := SourceStore{ConfigDir: dir}
	release, err := holder.Acquire(base)
	if err != nil {
		t.Fatal(err)
	}
	wall = base
	waiter := SourceStore{ConfigDir: dir, WallNow: func() time.Time { return wall }, Sleep: func(d time.Duration) { wall = wall.Add(d) }, Backoff: func(int) time.Duration { return time.Second }, PID: func() int { return 8 }}
	if _, err := waiter.Acquire(base); err == nil || !strings.Contains(err.Error(), "sources.json is locked by another mora process ("+waiter.LockPath()+"); retry in a moment") {
		t.Fatal(err)
	}
	wall = base
	oversleep := SourceStore{ConfigDir: dir, WallNow: func() time.Time { return wall }, Sleep: func(d time.Duration) { wall = wall.Add(d + 2*time.Second) }, Backoff: func(int) time.Duration { return time.Millisecond }}
	if _, err := oversleep.Acquire(base); err == nil {
		t.Fatal("overslept deadline retried")
	}
	release()
}
func TestSourceStoreMutationErrorBranches(t *testing.T) {
	callback := SourceStore{ConfigDir: t.TempDir()}
	if err := callback.Mutate(func(s []memory.Source) ([]memory.Source, error) { return s, errors.New("stop") }); err == nil {
		t.Fatal("callback error swallowed")
	}
	corrupt := SourceStore{ConfigDir: t.TempDir()}
	if err := os.MkdirAll(corrupt.ConfigDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corrupt.ConfigDir, "sources.json"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := corrupt.Mutate(func(s []memory.Source) ([]memory.Source, error) { return s, nil }); err == nil {
		t.Fatal("load error swallowed")
	}
	saveFail := SourceStore{ConfigDir: t.TempDir()}
	if err := SaveSources(saveFail.cfg(), nil); err != nil {
		t.Fatal(err)
	}
	if err := saveFail.Mutate(func(s []memory.Source) ([]memory.Source, error) {
		if err := os.RemoveAll(saveFail.ConfigDir); err != nil {
			return nil, err
		}
		if err := os.WriteFile(saveFail.ConfigDir, []byte("x"), 0600); err != nil {
			return nil, err
		}
		return s, nil
	}); err == nil {
		t.Fatal("save error swallowed")
	}
	cfg := config.Config{ConfigDir: t.TempDir()}
	if err := os.Mkdir(filepath.Join(cfg.ConfigDir, "sources.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSources(cfg); err == nil {
		t.Fatal("read error swallowed")
	}
	if got := LoadSourcesOrEmpty(cfg); got != nil {
		t.Fatal(got)
	}
	cfg2 := config.Config{ConfigDir: t.TempDir()}
	if err := SaveSources(cfg2, []memory.Source{{Name: "ok"}}); err != nil {
		t.Fatal(err)
	}
	if got := LoadSourcesOrEmpty(cfg2); len(got) != 1 || got[0].Name != "ok" {
		t.Fatal(got)
	}
}
