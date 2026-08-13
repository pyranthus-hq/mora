package ingest

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pyranthus-hq/mora/internal/config"
)

// LeaseSeams supplies process identity and liveness without platform coupling.
type LeaseSeams struct {
	PID          func() int
	ProcessAlive func(int) bool
}

func EnsureLease(cfg config.Config, sourceKey string, seams LeaseSeams) error {
	if err := ValidateStateRoot(cfg); err != nil {
		return err
	}
	lp := LeasePath(cfg, sourceKey)
	if b, err := os.ReadFile(lp); err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && pid == seams.PID() {
			return nil // our lease is already present
		}
	}
	if err := os.MkdirAll(filepath.Dir(lp), 0o700); err != nil {
		return err
	}
	return os.WriteFile(lp, []byte(strconv.Itoa(seams.PID())), 0o644)
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
