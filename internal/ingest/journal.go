// Package ingest owns connector-ingest journal identity and status primitives.
package ingest

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func JournalRoot(cfg config.Config) string { return filepath.Join(cfg.StateDir, "ingest") }

func ValidateStateRoot(cfg config.Config) error {
	if cfg.StateDir == "" || !filepath.IsAbs(cfg.StateDir) {
		return fmt.Errorf("ingest journal requires an absolute state_dir, got %q", cfg.StateDir)
	}
	return nil
}

func SourceKey(provider, account string) string {
	key := provider
	if account != "" {
		key += "@" + account
	}
	if key == "" {
		key = "unknown"
	}
	return memory.SafeFilename(key)
}

func JournalPath(cfg config.Config, sourceKey string) string {
	return filepath.Join(JournalRoot(cfg), sourceKey, "journal.log")
}

func LeasePath(cfg config.Config, sourceKey string) string {
	return filepath.Join(JournalRoot(cfg), sourceKey, "lease")
}

func JournalStatus(cfg config.Config) (dirty bool, paths int, oldest string, err error) {
	entries, rerr := os.ReadDir(JournalRoot(cfg))
	if errors.Is(rerr, os.ErrNotExist) {
		return false, 0, "", nil
	}
	if rerr != nil {
		return false, 0, "", rerr
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		hdr, n, present := ScanJournal(JournalPath(cfg, e.Name()))
		if !present {
			continue
		}
		// A PRESENT journal file is dirty regardless of its content. A committed
		// rebuild REMOVES a fully-covered journal (recoverIngestJournals), so any
		// lingering journal.log means an ingest run started and was not covered — and
		// a zero-byte or header-less file is a real crash state: appendJournalDurable
		// creates/opens the file BEFORE writing and syncing the header, so a SIGKILL in
		// that window leaves a present-but-empty journal. Treating it as absence (the
		// old hasContent==false path) was a false-clean — a truncated/malformed journal
		// must fail closed, not be indistinguishable from "no run" (A3 rule d).
		dirty = true
		paths += n
		if hdr != "" && (oldest == "" || hdr < oldest) {
			oldest = hdr
		}
	}
	return dirty, paths, oldest, nil
}

func ScanJournal(path string) (header string, pathLines int, present bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false
	}
	present = true
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "run" {
			header = fields[2]
			continue
		}
		pathLines++
	}
	return header, pathLines, present
}

func HeaderRunID(header string, validToken func(string) bool) string {
	fields := strings.Fields(header)
	if len(fields) == 3 && fields[0] == "run" && validToken(fields[1]) {
		return fields[1]
	}
	return ""
}

// AppendDurable appends a line with file and parent-directory durability barriers.
func AppendDurable(path, line string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return err
	}
	if err := atomicio.MarkerSyncFn(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return atomicio.SyncDirFn(dir)
}
