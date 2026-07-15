package mora

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// sources_lock_test.go pins the P3 fix: the sources.json read-modify-write is
// serialized by a crash-safe cross-process lease (mutateSources /
// acquireSourcesLock), so two concurrent writers can no longer lose an update.

func sourcesNameSet(sources []Source) map[string]bool {
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
	cfg := Config{ConfigDir: t.TempDir()}
	if err := saveSources(cfg, nil); err != nil {
		t.Fatalf("seed empty registry: %v", err)
	}

	// A takes the lease and holds it (an in-progress RMW that has not saved yet).
	relA, err := acquireSourcesLock(cfg, time.Now())
	if err != nil {
		t.Fatalf("A acquire: %v", err)
	}

	bDone := make(chan error, 1)
	go func() {
		bDone <- mutateSources(cfg, func(s []Source) ([]Source, error) {
			return append(s, Source{Name: "B", Type: "filesystem", Enabled: ptr(true)}), nil
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
	aSources, err := loadSources(cfg)
	if err != nil {
		relA()
		t.Fatalf("A load: %v", err)
	}
	aSources = append(aSources, Source{Name: "A", Type: "filesystem", Enabled: ptr(true)})
	if err := saveSources(cfg, aSources); err != nil {
		relA()
		t.Fatalf("A save: %v", err)
	}
	relA()

	// B unblocks, reloads inside the lease (sees A), appends B, saves.
	if err := <-bDone; err != nil {
		t.Fatalf("B RMW: %v", err)
	}

	got, err := loadSources(cfg)
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
	cfg := Config{ConfigDir: t.TempDir()}
	if err := saveSources(cfg, []Source{{Name: "pre", Type: "filesystem", Enabled: ptr(true)}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Plant an abandoned lease with an acquired_at older than the TTL.
	lockPath := sourcesLockPath(cfg)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	staleAt := time.Now().Add(-2 * sourcesLockTTL).UTC().Format(time.RFC3339)
	body, _ := json.Marshal(loopLockBody{PID: 999999, AcquiredAt: staleAt})
	if err := os.WriteFile(lockPath, body, 0o600); err != nil {
		t.Fatalf("plant stale lock: %v", err)
	}

	if err := mutateSources(cfg, func(s []Source) ([]Source, error) {
		return append(s, Source{Name: "after-crash", Type: "filesystem", Enabled: ptr(true)}), nil
	}); err != nil {
		t.Fatalf("mutateSources over a stale lease should reap and proceed: %v", err)
	}

	got, err := loadSources(cfg)
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
	cfg := Config{ConfigDir: t.TempDir()}
	const n = 8
	seed := make([]Source, n)
	for i := range seed {
		seed[i] = Source{Name: fmt.Sprintf("s%d", i), Type: "filesystem", Enabled: ptr(false)}
	}
	if err := saveSources(cfg, seed); err != nil {
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
	got, err := loadSources(cfg)
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
		_ = os.Setenv("MORA_CONFIG_DIR", cfgDir)
		cfg, err := loadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "loadConfig: %v\n", err)
			os.Exit(1)
		}
		if err := setSourceEnabledByName(cfg, name, true); err != nil {
			fmt.Fprintf(os.Stderr, "enable %s: %v\n", name, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	cfg := Config{ConfigDir: t.TempDir()}
	const n = 8
	seed := make([]Source, n)
	for i := range seed {
		seed[i] = Source{Name: fmt.Sprintf("s%d", i), Type: "filesystem", Enabled: ptr(false)}
	}
	if err := saveSources(cfg, seed); err != nil {
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
	got, err := loadSources(cfg)
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
	cfg := Config{ConfigDir: t.TempDir()}
	lockPath := sourcesLockPath(cfg)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now()
	expired, _ := json.Marshal(loopLockBody{PID: 999999, AcquiredAt: now.Add(-sourcesLockTTL - time.Second).UTC().Format(time.RFC3339)})
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
	var lb loopLockBody
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
	cfg := Config{ConfigDir: t.TempDir()}
	lockPath := sourcesLockPath(cfg)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Now()
	fresh, _ := json.Marshal(loopLockBody{PID: 999999, AcquiredAt: now.UTC().Format(time.RFC3339)})
	if err := os.WriteFile(lockPath, fresh, 0o600); err != nil {
		t.Fatalf("plant: %v", err)
	}
	reaped, err := reapStaleLockTTL(lockPath, now, sourcesLockTTL)
	if err != nil || reaped {
		t.Fatalf("a fresh sources lease must not be reaped: reaped=%v err=%v", reaped, err)
	}
}
