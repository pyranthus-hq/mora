package mora

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

var removeIngestJournalFile = os.Remove

func ingestJournalRoot(cfg Config) string { return filepath.Join(cfg.StateDir, "ingest") }

// ingestStateRootErr guards the journal/lease WRITE seam. An empty or relative
// StateDir would resolve the journal root against the process cwd and scatter
// runtime files there — under `go test`, into the package source tree (#184;
// PR #182 nearly committed such a lease file). Refuse loudly before the first
// MkdirAll instead of publishing state to a location nobody will ever read.
func ingestStateRootErr(cfg Config) error {
	if cfg.StateDir == "" || !filepath.IsAbs(cfg.StateDir) {
		return fmt.Errorf("ingest journal requires an absolute state_dir, got %q", cfg.StateDir)
	}
	return nil
}

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

// ingestLeasePath is the live-run marker beside a source's journal. Its presence
// with a LIVE owner pid tells a concurrent committed rebuild that files are still
// landing for this run, so it must NOT retire the run's header yet (A3 rule d).
func ingestLeasePath(cfg Config, sourceKey string) string {
	return filepath.Join(ingestJournalRoot(cfg), sourceKey, "lease")
}

// ensureIngestLease marks a source's ingest run live, keyed by the CURRENT pid.
// Called at the same mark-before-visible chokepoint that writes the journal header,
// so it uses the exact sourceKey the journal is stored under (no drift). It is a
// no-op once this process's lease is present. A missed release is self-healing:
// ingestLeaseHeld reclaims a lease whose owner pid is dead (a SIGKILLed run), so a
// crash never pins the index dirty forever — killed-ingest recovery (matrix 34a)
// still fires on the next rebuild. Not fsync'd: a lost lease only lets a covered
// header retire slightly sooner, never a false-clean (the journal header is the
// durable dirty signal).
func ensureIngestLease(cfg Config, sourceKey string) error {
	if err := ingestStateRootErr(cfg); err != nil {
		return err
	}
	lp := ingestLeasePath(cfg, sourceKey)
	if b, err := os.ReadFile(lp); err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && pid == os.Getpid() {
			return nil // our lease is already present
		}
	}
	if err := os.MkdirAll(filepath.Dir(lp), 0o700); err != nil {
		return err
	}
	return os.WriteFile(lp, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// ingestLeaseHeld reports whether a LIVE ingest run holds sourceKey's lease. A lease
// owned by a dead pid is stale: it is removed and reported not-held, so a killed
// ingest's leftover lease cannot block the recovery rebuild from retiring its header.
func ingestLeaseHeld(cfg Config, sourceKey string) bool {
	b, err := os.ReadFile(ingestLeasePath(cfg, sourceKey))
	if err != nil {
		return false
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if perr != nil || !processAlive(pid) {
		_ = os.Remove(ingestLeasePath(cfg, sourceKey)) // reclaim a stale lease
		return false
	}
	return true
}

// releaseIngestLeaseOwnedHere drops only sourceKey's lease. In-process
// concurrent ingests share a PID, so releasing every lease when one source
// finishes would make the other source appear abandoned mid-run.
func releaseIngestLeaseOwnedHere(cfg Config, sourceKey string) {
	lp := ingestLeasePath(cfg, sourceKey)
	me := strconv.Itoa(os.Getpid())
	if b, err := os.ReadFile(lp); err == nil && strings.TrimSpace(string(b)) == me {
		_ = os.Remove(lp)
	}
}

// releaseIngestLeasesOwnedHere drops every ingest lease this process owns. Called at
// the end of an ingest run (before cmdIngest's terminal rebuild), so a covered
// journal retires promptly instead of waiting for process exit. Best-effort.
func releaseIngestLeasesOwnedHere(cfg Config) {
	entries, err := os.ReadDir(ingestJournalRoot(cfg))
	if err != nil {
		return
	}
	me := strconv.Itoa(os.Getpid())
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		lp := ingestLeasePath(cfg, e.Name())
		if b, rerr := os.ReadFile(lp); rerr == nil && strings.TrimSpace(string(b)) == me {
			_ = os.Remove(lp)
		}
	}
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
	// Take the live-run lease FIRST (before any file is published), so a concurrent
	// committed rebuild that lists this run's files cannot retire the header out from
	// under a still-in-flight publish (A3 rule d / Finding 2). Idempotent per process.
	if err := ensureIngestLease(cfg, sourceKey); err != nil {
		return err
	}
	jp := ingestJournalPath(cfg, sourceKey)
	if _, err := os.Stat(jp); err == nil {
		return nil // header already written this run
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	runID := cfg.operationRunID
	if !validOperationToken(runID) {
		runID = newID() // legacy/direct caller: still mint a durable dirty identity
	}
	return appendJournalDurable(jp, "run "+runID+" "+indexClock().UTC().Format(time.RFC3339)+"\n")
}

// journalPublishedPath records a just-published file. Best-effort and NOT synced
// per line: the durable header already keeps the index dirty, and the recovery
// rebuild lists every file on disk regardless — so a lost path line only costs a
// covered file being re-listed, never a false-clean.
func journalPublishedPath(cfg Config, sourceKey, path string) {
	if ingestStateRootErr(cfg) != nil {
		return // the durable header guard already failed loudly for this run
	}
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
		hdr, n, present := scanJournal(ingestJournalPath(cfg, e.Name()))
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

// scanJournal returns the header marked_at (if any), the number of published-path
// lines, and whether the journal file is PRESENT (openable). Presence — not content
// — is the dirty signal: a zero-byte or header-less file is a crash state, never
// "absent" (Finding 4). A missing/unreadable file is not present.
func scanJournal(path string) (header string, pathLines int, present bool) {
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

// recoverIngestJournals runs after a committed rebuild. For each source journal it
// drops every published-path line the rebuild LISTED (rule d: listed, deliberately
// NOT parsed — an ingest of thousands of files must not be pinned dirty forever by
// one malformed file; the manifest's unparseable count owns that signal) or whose
// file no longer exists. When no path lines remain, the whole journal (header
// included) is removed — the run is fully covered. Cleanup errors are returned:
// the database may already be committed, but the rebuild must report partial failure
// and must not publish a false completed activity.
type ingestJournalRecovery struct {
	RetiredRunIDs []string
}

func recoverIngestJournals(cfg Config, files []string) (ingestJournalRecovery, error) {
	listed := cleanPathSet(files)
	entries, err := os.ReadDir(ingestJournalRoot(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return ingestJournalRecovery{}, nil
	}
	if err != nil {
		return ingestJournalRecovery{}, err
	}
	var result ingestJournalRecovery
	var recoveryErr error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		retired, err := compactIngestJournal(cfg, e.Name(), listed)
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

func compactIngestJournal(cfg Config, sourceKey string, listed map[string]bool) (string, error) {
	path := ingestJournalPath(cfg, sourceKey)
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
		// A3 rule d: retire the header ONLY when no LIVE ingest lease is held for this
		// source. Otherwise a run publishing more files (item N landed AFTER this
		// rebuild listed) would lose its dirty signal, and a SIGKILL before item N's
		// path line appended would be a false-clean (Finding 2). Keep just the header
		// so the index stays dirty; a later rebuild (after the lease releases) retires
		// it. A stale lease (dead owner) is reclaimed by ingestLeaseHeld, so a killed
		// run never pins the index dirty forever.
		if header != "" && ingestLeaseHeld(cfg, sourceKey) {
			if err := atomicWrite(path, []byte(header+"\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		}
		if err := removeIngestJournalFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return journalHeaderRunID(header), nil
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
	if err := atomicWrite(path, []byte(b2.String()), 0o644); err != nil {
		return "", err
	}
	return "", nil
}

func journalHeaderRunID(header string) string {
	fields := strings.Fields(header)
	if len(fields) == 3 && fields[0] == "run" && validOperationToken(fields[1]) {
		return fields[1]
	}
	return ""
}
