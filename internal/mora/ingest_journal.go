package mora

import (
	ingestpkg "github.com/pyranthus-hq/mora/internal/ingest"
	"os"
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

func ingestSourceKey(provider, account string) string           { return ingestpkg.SourceKey(provider, account) }
func ingestJournalPath(cfg Config, key string) string           { return ingestpkg.JournalPath(cfg, key) }
func ingestLeasePath(cfg Config, key string) string             { return ingestpkg.LeasePath(cfg, key) }
func ingestJournalStatus(cfg Config) (bool, int, string, error) { return ingestpkg.JournalStatus(cfg) }

var removeIngestJournalFile = os.Remove

// ingestStateRootErr guards the journal/lease WRITE seam. An empty or relative
// StateDir would resolve the journal root against the process cwd and scatter
// runtime files there — under `go test`, into the package source tree (#184;
// PR #182 nearly committed such a lease file). Refuse loudly before the first
// MkdirAll instead of publishing state to a location nobody will ever read.

// ingestSourceKey names one journal per connector instance. SafeFilename maps the
// path-hostile characters an account label could carry.

// ingestLeasePath is the live-run marker beside a source's journal. Its presence
// with a LIVE owner pid tells a concurrent committed rebuild that files are still
// landing for this run, so it must NOT retire the run's header yet (A3 rule d).

// ensureIngestLease marks a source's ingest run live, keyed by the CURRENT pid.
// Called at the same mark-before-visible chokepoint that writes the journal header,
// so it uses the exact sourceKey the journal is stored under (no drift). It is a
// no-op once this process's lease is present. A missed release is self-healing:
// ingestLeaseHeld reclaims a lease whose owner pid is dead (a SIGKILLed run), so a
// crash never pins the index dirty forever — killed-ingest recovery (matrix 34a)
// still fires on the next rebuild. Not fsync'd: a lost lease only lets a covered
// header retire slightly sooner, never a false-clean (the journal header is the
// durable dirty signal).

// ingestLeaseHeld reports whether a LIVE ingest run holds sourceKey's lease. A lease
// owned by a dead pid is stale: it is removed and reported not-held, so a killed
// ingest's leftover lease cannot block the recovery rebuild from retiring its header.

// releaseIngestLeaseOwnedHere drops only sourceKey's lease. In-process
// concurrent ingests share a PID, so releasing every lease when one source
// finishes would make the other source appear abandoned mid-run.

// releaseIngestLeasesOwnedHere drops every ingest lease this process owns. Called at
// the end of an ingest run (before cmdIngest's terminal rebuild), so a covered
// journal retires promptly instead of waiting for process exit. Best-effort.

// appendJournalDurable appends one line and forces both crash-durability barriers
// (f.Sync before return + parent-dir sync), reusing the same seams as
// atomicWriteDurable so the durability call-trace tests can record them. Used for
// the header line, whose loss would be a false-clean.
func ingestLeaseSeams() ingestpkg.LeaseSeams {
	return ingestpkg.LeaseSeams{PID: os.Getpid, ProcessAlive: processAlive}
}
func ensureIngestLease(cfg Config, key string) error {
	return ingestpkg.EnsureLease(cfg, key, ingestLeaseSeams())
}
func ingestLeaseHeld(cfg Config, key string) bool {
	return ingestpkg.LeaseHeld(cfg, key, ingestLeaseSeams())
}
func releaseIngestLeaseOwnedHere(cfg Config, key string) {
	ingestpkg.ReleaseLeaseOwnedHere(cfg, key, ingestLeaseSeams())
}
func releaseIngestLeasesOwnedHere(cfg Config) {
	ingestpkg.ReleaseLeasesOwnedHere(cfg, ingestLeaseSeams())
}

func ingestRecoverySeams() ingestpkg.RecoverySeams {
	return ingestpkg.RecoverySeams{CleanPathSet: cleanPathSet, CleanPath: cleanVaultPath, LeaseHeld: func(cfg Config, key string) bool { return ingestLeaseHeld(cfg, key) }, Remove: removeIngestJournalFile, ValidToken: validOperationToken}
}
func recoverIngestJournals(cfg Config, files []string) (ingestJournalRecovery, error) {
	return ingestpkg.RecoverJournals(cfg, files, ingestRecoverySeams())
}

func ingestPublishSeams() ingestpkg.PublishSeams {
	return ingestpkg.PublishSeams{ValidToken: validOperationToken, NewID: newID, Clock: indexClock, CleanPath: cleanVaultPath, Lease: ingestLeaseSeams()}
}
func ensureIngestJournalHeader(cfg Config, key string) error {
	return ingestpkg.EnsureJournalHeader(cfg, key, ingestPublishSeams())
}
func journalPublishedPath(cfg Config, key, path string) {
	ingestpkg.RecordPublishedPath(cfg, key, path, ingestPublishSeams())
}

// ensureIngestJournalHeader writes the durable "run <op_id> <marked_at>" header the
// FIRST time a source publishes in a run — BEFORE the file becomes visible. It is a
// stat-cheap no-op once the header exists, so a whole backfill pays one durable
// write, not one per memory. MUST be called before the atomicWrite that publishes.

// journalPublishedPath records a just-published file. Best-effort and NOT synced
// per line: the durable header already keeps the index dirty, and the recovery
// rebuild lists every file on disk regardless — so a lost path line only costs a
// covered file being re-listed, never a false-clean.

// ingestJournalStatus reports the aggregate ingest-journal state — the B1-rule-4
// half that lives outside StateDir/pending/. dirty is true if ANY source has an
// uncleared journal (a header line, or content with no parseable header — fail
// closed); paths is the total journaled published-path line count (banner detail);
// oldest is the earliest header marked_at (DirtySince). Cheap: a ReadDir of
// StateDir/ingest plus a scan per source journal.

// scanJournal returns the header marked_at (if any), the number of published-path
// lines, and whether the journal file is PRESENT (openable). Presence — not content
// — is the dirty signal: a zero-byte or header-less file is a crash state, never
// "absent" (Finding 4). A missing/unreadable file is not present.

// recoverIngestJournals runs after a committed rebuild. For each source journal it
// drops every published-path line the rebuild LISTED (rule d: listed, deliberately
// NOT parsed — an ingest of thousands of files must not be pinned dirty forever by
// one malformed file; the manifest's unparseable count owns that signal) or whose
// file no longer exists. When no path lines remain, the whole journal (header
// included) is removed — the run is fully covered. Cleanup errors are returned:
// the database may already be committed, but the rebuild must report partial failure
// and must not publish a false completed activity.
type ingestJournalRecovery = ingestpkg.Recovery
