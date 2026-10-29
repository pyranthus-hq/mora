package ingest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/config"
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

// EnsureLease takes the per-source lease or returns *LeaseHeldError when another
// live process holds it. The same PID re-enters freely. The file appears with its
// PID already inside (temp file + link), so a racing reader never sees it empty.
func EnsureLease(cfg config.Config, sourceKey string, seams LeaseSeams) error {
	if err := ValidateStateRoot(cfg); err != nil {
		return err
	}
	lp := LeasePath(cfg, sourceKey)
	if err := os.MkdirAll(filepath.Dir(lp), 0o700); err != nil {
		return err
	}
	me := seams.PID()
	tmp := fmt.Sprintf("%s.%d.tmp", lp, me)
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(me)), 0o644); err != nil {
		return err
	}
	defer os.Remove(tmp)
	for range 2 {
		err := os.Link(tmp, lp)
		if err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		pid := -1
		if b, rerr := os.ReadFile(lp); rerr == nil {
			if p, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil {
				pid = p
			}
		}
		if pid == me {
			return nil
		}
		fresh := false
		if fi, serr := os.Stat(lp); serr == nil {
			fresh = time.Since(fi.ModTime()) < maxLeaseAge
		}
		if pid > 0 && fresh && seams.ProcessAlive(pid) {
			return &LeaseHeldError{Source: sourceKey, PID: pid}
		}
		// shortcut: two takers racing over one stale lease can both remove; the loser
		// then sees the winner's live PID on the retry. Upgrade if that ever shows.
		_ = os.Remove(lp)
	}
	return fmt.Errorf("lease for %s is contended", sourceKey)
}

func LeaseHeld(cfg config.Config, sourceKey string, seams LeaseSeams) bool {
	b, err := os.ReadFile(LeasePath(cfg, sourceKey))
	if err != nil {
		return false
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if perr != nil || !seams.ProcessAlive(pid) {
		_ = os.Remove(LeasePath(cfg, sourceKey)) // reclaim a stale lease
		return false
	}
	return true
}

func ReleaseLeaseOwnedHere(cfg config.Config, sourceKey string, seams LeaseSeams) {
	lp := LeasePath(cfg, sourceKey)
	me := strconv.Itoa(seams.PID())
	if b, err := os.ReadFile(lp); err == nil && strings.TrimSpace(string(b)) == me {
		_ = os.Remove(lp)
	}
}

func ReleaseLeasesOwnedHere(cfg config.Config, seams LeaseSeams) {
	entries, err := os.ReadDir(JournalRoot(cfg))
	if err != nil {
		return
	}
	me := strconv.Itoa(seams.PID())
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		lp := LeasePath(cfg, e.Name())
		if b, rerr := os.ReadFile(lp); rerr == nil && strings.TrimSpace(string(b)) == me {
			_ = os.Remove(lp)
		}
	}
}
