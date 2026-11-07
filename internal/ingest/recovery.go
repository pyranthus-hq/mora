package ingest

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/leasefile"
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

// SnapshotJournals records each journal's identity and size just before a
// rebuild lists the vault. A line is written after its file is published, so a
// line inside that size proves the listing saw the publish. A source with a live
// ingest is left out: it can publish at any moment, so its journal waits for a
// rebuild that starts after the run ended.
func SnapshotJournals(cfg config.Config, seams RecoverySeams) (map[string]os.FileInfo, error) {
	entries, err := os.ReadDir(JournalRoot(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	snapshot := map[string]os.FileInfo{}
	for _, e := range entries {
		if !e.IsDir() || seams.LeaseHeld(cfg, e.Name()) {
			continue
		}
		info, err := os.Stat(JournalPath(cfg, e.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		snapshot[e.Name()] = info
	}
	return snapshot, nil
}

// RecoverJournals retires only what snapshot proves the rebuild's listing saw.
func RecoverJournals(cfg config.Config, files []string, snapshot map[string]os.FileInfo, seams RecoverySeams) (Recovery, error) {
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
		seen, ok := snapshot[e.Name()]
		if !e.IsDir() || !ok {
			continue
		}
		retired, err := CompactJournal(cfg, e.Name(), listed, seen, seams)
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

// CompactJournal retires the lines of one journal. seen bounds them to a rebuild's
// snapshot; a nil seen means the whole file. The guard covers the read through the
// replace or remove, and every creator and appender takes it, so a line cannot land
// between the bytes read here and the journal written back.
func CompactJournal(cfg config.Config, sourceKey string, listed map[string]bool, seen os.FileInfo, seams RecoverySeams) (retired string, err error) {
	err = leasefile.WithGuard(JournalPath(cfg, sourceKey), func() error {
		var cerr error
		retired, cerr = compactJournal(cfg, sourceKey, listed, seen, seams)
		return cerr
	})
	return retired, err
}

func compactJournal(cfg config.Config, sourceKey string, listed map[string]bool, seen os.FileInfo, seams RecoverySeams) (string, error) {
	path := JournalPath(cfg, sourceKey)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	cut := len(b)
	if seen != nil {
		// A journal rewritten since the snapshot has other lines at those offsets,
		// so nothing in it is proven: leave it for the next rebuild.
		now, err := os.Stat(path)
		if err != nil || !os.SameFile(seen, now) || int64(len(b)) < seen.Size() {
			return "", nil
		}
		cut = bytes.LastIndexByte(b[:seen.Size()], '\n') + 1
		if cut == 0 && len(b) > 0 {
			return "", nil // every line arrived after the snapshot
		}
	}
	// Case-folding only finds candidates; file identity must confirm coverage.
	// Keep Linux's exact-path behavior, and never merge distinct files on a
	// case-sensitive Darwin/Windows volume.
	folded := map[string][]string{}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		for p, covered := range listed {
			if covered {
				folded[strings.ToLower(p)] = append(folded[strings.ToLower(p)], p)
			}
		}
	}
	var header string
	var keptPaths []string
	for _, raw := range strings.Split(string(b[:cut]), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "run ") {
			header = line
			continue
		}
		p := seams.CleanPath(line)
		covered := listed[p]
		if !covered {
			if candidates := folded[strings.ToLower(p)]; len(candidates) > 0 {
				if info, err := os.Stat(p); err == nil {
					for _, candidate := range candidates {
						other, err := os.Stat(candidate)
						if err == nil && os.SameFile(info, other) {
							covered = true
							break
						}
					}
				}
			}
		}
		if covered {
			continue // covered by this rebuild
		}
		if _, statErr := os.Stat(p); errors.Is(statErr, os.ErrNotExist) {
			continue // the file is gone; nothing to recover
		}
		keptPaths = append(keptPaths, line)
	}
	// Lines past the snapshot may record a publish the listing missed, even for a
	// listed path, so they stay whatever the path looks like now.
	for _, raw := range strings.Split(string(b[cut:]), "\n") {
		if line := strings.TrimSpace(raw); line != "" {
			keptPaths = append(keptPaths, line)
		}
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
