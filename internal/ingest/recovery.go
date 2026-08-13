package ingest

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
)

// RecoverySeams supplies consistency helpers owned by adjacent index/composition boundaries.
type RecoverySeams struct {
	CleanPathSet func([]string) map[string]bool
	CleanPath    func(string) string
	LeaseHeld    func(config.Config, string) bool
	Remove       func(string) error
	ValidToken   func(string) bool
}

type Recovery struct {
	RetiredRunIDs []string
}

func RecoverJournals(cfg config.Config, files []string, seams RecoverySeams) (Recovery, error) {
	listed := seams.CleanPathSet(files)
	entries, err := os.ReadDir(JournalRoot(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return Recovery{}, nil
	}
	if err != nil {
		return Recovery{}, err
	}
	var result Recovery
	var recoveryErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		retired, err := CompactJournal(cfg, e.Name(), listed, seams)
		if err != nil {
			recoveryErr = errors.Join(recoveryErr, fmt.Errorf("retiring ingest journal: %w", err))
			continue
		}
		if retired != "" {
			result.RetiredRunIDs = append(result.RetiredRunIDs, retired)
		}
	}
	return result, recoveryErr
}
func CompactJournal(cfg config.Config, sourceKey string, listed map[string]bool, seams RecoverySeams) (string, error) {
	path := JournalPath(cfg, sourceKey)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var header string
	var keptPaths []string
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "run ") {
			header = line
			continue
		}
		p := seams.CleanPath(line)
		if listed[p] {
			continue // covered by this rebuild
		}
		if _, statErr := os.Stat(p); errors.Is(statErr, os.ErrNotExist) {
			continue // the file is gone; nothing to recover
		}
		keptPaths = append(keptPaths, line)
	}
	if len(keptPaths) == 0 {
		// A3 rule d: retire the header ONLY when no LIVE ingest lease is held for this
		// source. Otherwise a run publishing more files (item N landed AFTER this
		// rebuild listed) would lose its dirty signal, and a SIGKILL before item N's
		// path line appended would be a false-clean (Finding 2). Keep just the header
		// so the index stays dirty; a later rebuild (after the lease releases) retires
		// it. A stale lease (dead owner) is reclaimed by seams.LeaseHeld, so a killed
		// run never pins the index dirty forever.
		if header != "" && seams.LeaseHeld(cfg, sourceKey) {
			if err := atomicio.Write(path, []byte(header+"\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		}
		if err := seams.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return HeaderRunID(header, seams.ValidToken), nil
	}
	// Some published paths remain uncovered (a write raced in after this rebuild
	// listed): rewrite the journal keeping the header + the survivors, so the index
	// stays dirty until a later rebuild covers them.
	var b2 strings.Builder
	if header != "" {
		b2.WriteString(header + "\n")
	}
	for _, p := range keptPaths {
		b2.WriteString(p + "\n")
	}
	if err := atomicio.Write(path, []byte(b2.String()), 0o644); err != nil {
		return "", err
	}
	return "", nil
}
