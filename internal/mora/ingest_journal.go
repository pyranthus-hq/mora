package mora

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// ingest_journal.go — the durable ingest journal (Gate 2, A3 rule d). A killed
// connector backfill cannot write a terminal completion bit: memory.Ingest only
// persists SyncStatus AFTER it returns, so a SIGKILL mid-run leaves published .md
// files on disk with no record they were never indexed — the recorded 7-day SEV in
// miniature. So ingest keeps StateDir/ingest/<source>/journal.log: a durable header
// line written BEFORE the first file is published (mark-before-visible), plus a
// best-effort line per published path. A NON-EMPTY journal makes the index dirty
// (B1 rule 4); the next committed rebuild lists every published path — journaled or
// not — indexes them, and retires the journal. The header's durability is the only
// hard requirement: it is what keeps the index dirty across a crash, and the
// full-vault rebuild is what actually re-indexes the files.

func ingestJournalRoot(cfg Config) string { return filepath.Join(cfg.StateDir, "ingest") }

// ingestSourceKey names one journal per connector instance. SafeFilename maps the
// path-hostile characters an account label could carry.
func ingestSourceKey(provider, account string) string {
	key := provider
	if account != "" {
		key += "@" + account
	}
	if key == "" {
		key = "unknown"
	}
	return memory.SafeFilename(key)
}

func ingestJournalPath(cfg Config, sourceKey string) string {
	return filepath.Join(ingestJournalRoot(cfg), sourceKey, "journal.log")
}

// appendJournalDurable appends one line and forces both crash-durability barriers
// (f.Sync before return + parent-dir sync), reusing the same seams as
// atomicWriteDurable so the durability call-trace tests can record them. Used for
// the header line, whose loss would be a false-clean.
func appendJournalDurable(path, line string) error {
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
	if err := markerSyncFn(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDirFn(dir)
}

// ensureIngestJournalHeader writes the durable "run <op_id> <marked_at>" header the
// FIRST time a source publishes in a run — BEFORE the file becomes visible. It is a
// stat-cheap no-op once the header exists, so a whole backfill pays one durable
// write, not one per memory. MUST be called before the atomicWrite that publishes.
func ensureIngestJournalHeader(cfg Config, sourceKey string) error {
	jp := ingestJournalPath(cfg, sourceKey)
	if _, err := os.Stat(jp); err == nil {
		return nil // header already written this run
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return appendJournalDurable(jp, "run "+newID()+" "+indexClock().UTC().Format(time.RFC3339)+"\n")
}

// journalPublishedPath records a just-published file. Best-effort and NOT synced
// per line: the durable header already keeps the index dirty, and the recovery
// rebuild lists every file on disk regardless — so a lost path line only costs a
// covered file being re-listed, never a false-clean.
func journalPublishedPath(cfg Config, sourceKey, path string) {
	_ = appendFile(ingestJournalPath(cfg, sourceKey), cleanVaultPath(path)+"\n")
}

// ingestJournalStatus reports the aggregate ingest-journal state — the B1-rule-4
// half that lives outside StateDir/pending/. dirty is true if ANY source has an
// uncleared journal (a header line, or content with no parseable header — fail
// closed); paths is the total journaled published-path line count (banner detail);
// oldest is the earliest header marked_at (DirtySince). Cheap: a ReadDir of
// StateDir/ingest plus a scan per source journal.
func ingestJournalStatus(cfg Config) (dirty bool, paths int, oldest string, err error) {
	entries, rerr := os.ReadDir(ingestJournalRoot(cfg))
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
		hdr, n, hasContent := scanJournal(ingestJournalPath(cfg, e.Name()))
		if !hasContent {
			continue
		}
		dirty = true
		paths += n
		if hdr != "" && (oldest == "" || hdr < oldest) {
			oldest = hdr
		}
	}
	return dirty, paths, oldest, nil
}

// scanJournal returns the header marked_at (if any), the number of published-path
// lines, and whether the journal has any content at all.
func scanJournal(path string) (header string, pathLines int, hasContent bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		hasContent = true
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "run" {
			header = fields[2]
			continue
		}
		pathLines++
	}
	return header, pathLines, hasContent
}

// recoverIngestJournals runs after a committed rebuild. For each source journal it
// drops every published-path line the rebuild LISTED (rule d: listed, deliberately
// NOT parsed — an ingest of thousands of files must not be pinned dirty forever by
// one malformed file; the manifest's unparseable count owns that signal) or whose
// file no longer exists. When no path lines remain, the whole journal (header
// included) is removed — the run is fully covered. Best-effort throughout: a failed
// removal leaves a false-dirty the next rebuild clears.
func recoverIngestJournals(cfg Config, files []string) {
	listed := cleanPathSet(files)
	entries, err := os.ReadDir(ingestJournalRoot(cfg))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		compactIngestJournal(cfg, ingestJournalPath(cfg, e.Name()), listed)
	}
}

func compactIngestJournal(cfg Config, path string, listed map[string]bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
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
		p := cleanVaultPath(line)
		if listed[p] {
			continue // covered by this rebuild
		}
		if _, statErr := os.Stat(p); errors.Is(statErr, os.ErrNotExist) {
			continue // the file is gone; nothing to recover
		}
		keptPaths = append(keptPaths, line)
	}
	if len(keptPaths) == 0 {
		_ = os.Remove(path) // fully covered: retire header + journal
		return
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
	_ = atomicWrite(path, []byte(b2.String()), 0o644)
}
