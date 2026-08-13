package mora

import (
	"encoding/json"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"os"
	"path/filepath"
	"time"
)

// Producer-ledger read-modify-write serialization (Packet E / HEALTH-11).
//
// The producer ledger (StateDir/producers/status.json) is a SINGLE shared file
// that every producer stamps into — a manual `mora index rebuild` racing the
// scheduled `index-hourly`, a `pulse --advance` racing `doctor --pulse`, etc.
// A plain atomicWrite of that one file loses updates exactly like sources.json
// did (sources_lock.go): two processes each doing load -> mutate -> save race and
// the last rename silently drops the other's append.
//
// producer_lock.go is a SIBLING of mutateSources — NOT a reuse of
// acquireSourcesLock (which is hardcoded to sources.json, sources_lock.go:58) and
// NOT internal/memory/status.go's SaveStatus (a lease-less fixed-name temp +
// rename that survives only because each source owns a PRIVATE file; a single
// shared producer ledger has no such protection). It holds the same crash-safe,
// Windows-CI-green file-lock primitives proven in loop.go (publishLockFile's
// os.Link-atomic publish, reapStaleLockTTL's TTL/corrupt reap, loopLockReleaser)
// around the WHOLE read-modify-write and RELOADS inside the lease so a concurrent
// writer's committed change is always observed, never clobbered.

const (
	// producerLockTTL bounds a leaked lease. A real ledger RMW holds it for
	// microseconds (read a small JSON map, mutate in memory, atomicWrite) and it
	// is NEVER held across a producer's actual work (fn runs OUTSIDE the lease),
	// so 30s only ever reaps an abandoned lease. Mirrors sourcesLockTTL.
	producerLockTTL = 30 * time.Second
	// producerAcquireTimeout is the WALL-CLOCK budget for the ledger lease's
	// contention spin, mirroring sourcesAcquireTimeout's envelope (the hold is the
	// same shape: read a small JSON map, mutate, atomicWrite). It is a SEPARATE
	// constant because the producer stamp is best-effort telemetry on the tail of a
	// real job — if this path ever needs to give up sooner than a sources RMW, it
	// must be able to without shortening the registry's budget.
	producerAcquireTimeout = 2 * time.Second
)

// producersDir is the ledger directory under StateDir (rebuildable state, not
// secret): both status.json (evidence) and expected.json (expectation) live here.
func producersDir(cfg Config) string {
	return filepath.Join(cfg.StateDir, "producers")
}

func producerStatusPath(cfg Config) string {
	return filepath.Join(producersDir(cfg), "status.json")
}

func producerExpectedPath(cfg Config) string {
	return filepath.Join(producersDir(cfg), "expected.json")
}

// producerLockPath is the lease file co-located with status.json. Its persistent
// OS guard is selected deterministically by leaseGuardPath under StateDir.
func producerLockPath(cfg Config) string {
	return producerStatusPath(cfg) + ".lock"
}

// acquireProducerLock takes the producer-ledger lease for one read-modify-write.
// It mirrors acquireSourcesLock's publish/reap/wait loop (sources_lock.go) with
// the producer TTL and its own producerAcquireTimeout wall-clock acquire budget,
// and returns a real error (never a silent no-op) if the lease
// cannot be taken — a producer stamp that cannot serialize must fail loudly to its
// best-effort caller, not silently drop the write. The returned release is
// idempotent, so a deferred release is safe.
func acquireProducerLock(cfg Config, now time.Time) (release func(), err error) {
	if err := os.MkdirAll(producersDir(cfg), 0o700); err != nil {
		return nil, err
	}
	lockPath := producerLockPath(cfg)
	// run_id is unused (acquire and release live in the same scope); acquired_at
	// drives the TTL. Same body shape as the sources/loop leases.
	body, _ := json.Marshal(loopLockBody{PID: os.Getpid(), AcquiredAt: now.UTC().Format(time.RFC3339)})
	deadline := time.Now().Add(producerAcquireTimeout)
	for attempt := 0; ; attempt++ {
		published, perr := publishLockFile(lockPath, body)
		wait := sourcesAcquireBackoff(attempt)
		switch {
		case perr == nil && published:
			return loopLockReleaser(lockPath, body), nil
		case perr != nil && !atomicio.SharingViolationRetryable(perr):
			return nil, perr // a real, non-contention fs error: never interleave
		case perr == nil:
			reaped, rerr := reapStaleLockTTL(lockPath, now, producerLockTTL)
			if rerr != nil && !atomicio.SharingViolationRetryable(rerr) {
				return nil, rerr
			}
			if rerr == nil && reaped {
				wait = 0 // cleared an abandoned lease; retry publish immediately
			}
			// else: a live holder OR Windows sharing contention — back off and retry.
		}
		if !sleepWithinDeadline(wait, deadline) {
			break
		}
	}
	return nil, fmt.Errorf("producer ledger is locked by another mora process (%s); retry in a moment", lockPath)
}

// mutateProducers serializes a read-modify-write on the producer ledger across
// processes. It acquires the lease, RELOADS status.json inside the lease (the crux
// of the lost-update fix — loading before the lease would reintroduce the race),
// applies mutate to the freshly loaded map, and saves the result before releasing.
// A mutate that returns an error aborts WITHOUT writing. Every producer status
// write goes through here; a load->mutate->save added outside it reopens the race.
//
// The lease is acquired with REAL wall-clock time — never a caller's injected
// logical `now`. The two clocks are different things: the injected clock governs
// stamp CONTENT (determinism), while the lease TTL is a liveness bound on a
// physically-running process. Feeding a logical clock to the TTL is a genuine
// lost-update bug: two concurrent holders whose injected `now` differ by more than
// producerLockTTL each judge the other's LIVE lock abandoned, reap it mid-RMW, and
// silently drop one another's write — precisely what the lease exists to prevent
// (TestProducerLedgerNoLostUpdateAcrossProcesses caught exactly this). mutateSources
// draws the same line (it acquires with time.Now()), and so does the acquire
// deadline itself (sleepWithinDeadline).
func mutateProducers(cfg Config, mutate func(map[string]producerStatus) error) error {
	release, err := acquireProducerLock(cfg, time.Now())
	if err != nil {
		return err
	}
	defer release()
	m, err := loadProducerStatus(cfg)
	if err != nil {
		return err
	}
	if err := mutate(m); err != nil {
		return err
	}
	return saveProducerStatus(cfg, m)
}

// loadProducerStatus reads the ledger map. A missing file is an empty ledger (not
// an error — the first producer stamp creates it); a corrupt file is a real error
// so a torn ledger fails loud rather than silently resetting every producer.
func loadProducerStatus(cfg Config) (map[string]producerStatus, error) {
	data, err := os.ReadFile(producerStatusPath(cfg))
	if os.IsNotExist(err) {
		return map[string]producerStatus{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]producerStatus{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("producer ledger %s is corrupt: %w", producerStatusPath(cfg), err)
	}
	return m, nil
}

func saveProducerStatus(cfg Config, m map[string]producerStatus) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(producerStatusPath(cfg), b, 0o600)
}

// loadExpectedProducers reads the expectation map. Like the status ledger, a
// missing file is empty (nobody has scheduled/declared/adopted a producer yet) and
// a corrupt file is a real error. The expectation lives in a SEPARATE file from
// the evidence (E2): E5's replay DELETES the status stamp to model a dead
// worktree, and an expectation inferred from the stamp would be erased by exactly
// the event it must detect — so the alarm would delete itself.
func loadExpectedProducers(cfg Config) (map[string]expectedProducer, error) {
	data, err := os.ReadFile(producerExpectedPath(cfg))
	if os.IsNotExist(err) {
		return map[string]expectedProducer{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]expectedProducer{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("producer expectation %s is corrupt: %w", producerExpectedPath(cfg), err)
	}
	return m, nil
}

func saveExpectedProducers(cfg Config, m map[string]expectedProducer) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(producerExpectedPath(cfg), b, 0o600)
}
