package ingest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

// LeaseSeams supplies process identity and liveness without platform coupling.
type LeaseSeams struct {
	PID          func() int
	ProcessAlive func(int) bool
}

// maxLeaseAge bounds how long a lease can claim a live PID. A sync is capped far
// below this, so an older lease whose PID is alive is PID reuse, not a sync.
const maxLeaseAge = 12 * time.Hour

// LeaseHeldError says another live process is already syncing this source.
type LeaseHeldError struct {
	Source string
	PID    int
}

func (e *LeaseHeldError) Error() string {
	return fmt.Sprintf("%s is already syncing in process %d", e.Source, e.PID)
}

// readLease returns the holder PID (-1 when unreadable) and owner nonce.
func readLease(lp string) (pid int, nonce string, exists bool) {
	b, err := os.ReadFile(lp)
	if err != nil {
		return -1, "", false
	}
	pid = -1
	if f := strings.Fields(string(b)); len(f) > 0 {
		if p, perr := strconv.Atoi(f[0]); perr == nil {
			pid = p
		}
		if len(f) > 1 {
			nonce = f[1]
		}
	}
	return pid, nonce, true
}

// leaseLive reports whether the lease at lp still names a running holder.
func leaseLive(lp string, pid int, seams LeaseSeams) bool {
	if pid <= 0 || !seams.ProcessAlive(pid) {
		return false
	}
	fi, err := os.Stat(lp)
	return err == nil && time.Since(fi.ModTime()) < maxLeaseAge
}

// takeLease creates the lease under the path guard. Every creator and every
// stale-lease remover holds the guard, so no reader under it sees a half-made
// file and two contenders cannot both replace one stale lease.
func takeLease(cfg config.Config, sourceKey string, seams LeaseSeams, nonce string) error {
	if err := ValidateStateRoot(cfg); err != nil {
		return err
	}
	lp := LeasePath(cfg, sourceKey)
	me := seams.PID()
	return leasefile.WithGuard(lp, func() error {
		for range 2 {
			err := atomicio.CreateExclusive(lp, []byte(strconv.Itoa(me)+" "+nonce), 0o644)
			if err == nil || !errors.Is(err, fs.ErrExist) {
				return err
			}
			pid, _, exists := readLease(lp)
			if !exists {
				continue // released between create and read
			}
			if pid == me {
				return nil // re-entrant: this process already owns it
			}
			if leaseLive(lp, pid, seams) {
				return &LeaseHeldError{Source: sourceKey, PID: pid}
			}
			_ = os.Remove(lp)
		}
		return fmt.Errorf("lease for %s is contended", sourceKey)
	})
}

// EnsureLease takes the per-source lease or returns *LeaseHeldError when another
// live process holds it. The same PID re-enters freely.
func EnsureLease(cfg config.Config, sourceKey string, seams LeaseSeams) error {
	return takeLease(cfg, sourceKey, seams, newNonce())
}

func newNonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// leaseSlots serializes same-lease-key runs inside one process: a sibling waits
// for the holder instead of sharing (and then releasing) its lease.
var leaseSlots sync.Map // lease path -> chan struct{}

// AcquireLease waits for in-process siblings on the same key, then takes the
// file lease. The returned release frees it only if this acquisition still owns
// it. Callers must call release exactly when the sync ends.
func AcquireLease(ctx context.Context, cfg config.Config, sourceKey string, seams LeaseSeams) (release func(), err error) {
	lp := LeasePath(cfg, sourceKey)
	v, _ := leaseSlots.LoadOrStore(lp, make(chan struct{}, 1))
	slot := v.(chan struct{})
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	nonce := newNonce()
	if err := takeLease(cfg, sourceKey, seams, nonce); err != nil {
		<-slot
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if pid, n, ok := readLease(lp); ok && pid == seams.PID() && n == nonce {
				_ = os.Remove(lp)
			}
			<-slot
		})
	}, nil
}

func LeaseHeld(cfg config.Config, sourceKey string, seams LeaseSeams) bool {
	lp := LeasePath(cfg, sourceKey)
	pid, _, exists := readLease(lp)
	if !exists {
		return false
	}
	if leaseLive(lp, pid, seams) {
		return true
	}
	// Reclaim a stale lease under the guard, re-reading so a creator that just
	// replaced it is not undone.
	held := true
	if err := leasefile.WithGuard(lp, func() error {
		pid, _, exists := readLease(lp)
		held = exists && leaseLive(lp, pid, seams)
		if exists && !held {
			_ = os.Remove(lp)
		}
		return nil
	}); err != nil {
		return true // cannot prove it stale; keep the journal header
	}
	return held
}

func ReleaseLeaseOwnedHere(cfg config.Config, sourceKey string, seams LeaseSeams) {
	lp := LeasePath(cfg, sourceKey)
	if pid, _, ok := readLease(lp); ok && pid == seams.PID() {
		_ = os.Remove(lp)
	}
}

func ReleaseLeasesOwnedHere(cfg config.Config, seams LeaseSeams) {
	entries, err := os.ReadDir(JournalRoot(cfg))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		lp := LeasePath(cfg, e.Name())
		if pid, _, ok := readLease(lp); ok && pid == seams.PID() {
			_ = os.Remove(lp)
		}
	}
}
