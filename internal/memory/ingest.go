package memory

import (
	"fmt"
	"time"
)

// IngestParams drives a snapshot ingestion. Write persists one memory (the mora
// wiring supplies it). A nil-safe Status is required.
type IngestParams struct {
	Fetcher    Fetcher
	Kind       ItemKind
	Window     FetchWindow
	Scope      string
	BodyBudget int
	Status     *SyncStatus
	Write      func(MappedMemory) error

	// Map optionally overrides how a fetched Item becomes a MappedMemory. When nil,
	// the shared MapItem (start-keep truncation) is used — Gmail/Calendar behavior. A
	// connector needing different mapping (e.g. iMessage's newest-first truncation via
	// its own mapConversation over Item.Payload) supplies it here.
	Map func(Item, string, int) MappedMemory
}

type IngestResult struct {
	Status *SyncStatus
}

// Ingest pages through the Fetcher from the checkpoint cursor, maps each Item,
// and calls Write. Per-item Write failures are counted and never abort the
// loop (the remaining items still land), but a completed run that dropped
// items returns a non-nil error — returning nil told callers the run was
// healthy while memories were silently missing. The checkpoint advances per
// page so a crash resumes instead of restarting; it is cleared on completed
// paging (so a re-run re-attempts dropped items from the start). Page-fetch
// errors stop the run but preserve the checkpoint for resume.
func Ingest(p IngestParams) (IngestResult, error) {
	if p.Status == nil {
		p.Status = &SyncStatus{}
	}
	mapFn := p.Map
	if mapFn == nil {
		mapFn = MapItem
	}
	cursor := p.Status.Checkpoint
	// Snapshot the prior error tally so the clean-completion reset only clears
	// errors carried in from a PRIOR run — a run that itself accumulates per-item
	// write errors is not a clean attempt and keeps its errors (M-3: health is the
	// last attempt's outcome, not a "paging finished" signal).
	errorsBefore := p.Status.ErrorCount
	for {
		page, err := p.Fetcher.FetchPage(p.Kind, p.Window, cursor)
		if err != nil {
			p.Status.ErrorCount++
			p.Status.LastError = err.Error()
			// This package writes PROSE and never claims a typed code: the error
			// taxonomy lives in internal/mora, which this package must not import.
			// Clearing ErrorCode here means a code left by an EARLIER, different
			// failure can never be read as this one's cause. internal/mora's
			// persistSyncStatus types the record a moment later, at the boundary
			// that owns both the status and the taxonomy.
			p.Status.ErrorCode = ""
			// Stamp the attempt but NOT success: a failed attempt records when it
			// was tried while leaving LastSuccessAt untouched, so the digest (M-3 /
			// D-03) can tell "never succeeded" from "succeeded but stale".
			p.Status.LastAttemptAt = time.Now().UTC().Format(time.RFC3339)
			// Keep checkpoint = cursor so the next run resumes this page.
			p.Status.Checkpoint = cursor
			return IngestResult{Status: p.Status}, err
		}
		for _, it := range page.Items {
			m := mapFn(it, p.Scope, p.BodyBudget)
			m.LastSynced = time.Now().UTC().Format(time.RFC3339)
			if werr := p.Write(m); werr != nil {
				p.Status.ErrorCount++
				p.Status.LastError = werr.Error()
				p.Status.ErrorCode = "" // prose only here; see the fetch-failure branch above
				continue
			}
			p.Status.ItemCount++
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		p.Status.Checkpoint = cursor // advance checkpoint per page
	}
	p.Status.Checkpoint = ""
	// Paging finished. Every completed attempt — clean OR partial — stamps
	// LastAttemptAt (when it was tried). The SUCCESS timestamps are stamped only
	// below, and only on a genuinely clean run, so a partial-failure run never
	// looks fresh/healthy.
	now := time.Now().UTC().Format(time.RFC3339)
	p.Status.LastAttemptAt = now

	// Partial-failure honesty (M-3): a run that dropped items is NOT a success.
	// Leave LastSynced/LastSuccessAt at their prior values (so search doesn't
	// report incomplete data as fresh and skip-if-fresh still retries) and keep
	// this run's error tally. The cleared checkpoint means a re-run re-fetches
	// from the start and re-attempts the dropped items. The caller decides how
	// loudly to surface the returned error.
	if dropped := p.Status.ErrorCount - errorsBefore; dropped > 0 {
		return IngestResult{Status: p.Status}, fmt.Errorf("%d item(s) failed to write and were dropped (last error: %s) — successfully written items are saved; re-run the sync to retry", dropped, p.Status.LastError)
	}

	// Clean completion: model health as a LAST-ATTEMPT outcome (M-3). One instant
	// shared across LastSynced / LastSuccessAt / LastAttemptAt keeps them
	// consistent. Reset the error tally so a source that errored on a PRIOR run
	// and recovered stops reading "unavailable" forever (which would invert SC#3
	// once the digest derives "broken" from these fields) — safe here because a
	// nonzero `dropped` already returned above, so this path is genuinely clean.
	p.Status.LastSynced = now
	p.Status.LastSuccessAt = now
	p.Status.ErrorCount = 0
	p.Status.LastError = ""
	// ErrorCode belongs to the SAME last-attempt reset as ErrorCount/LastError.
	// Without this line a source that failed with connector.unauthorized and then
	// recovered kept reporting that code on a `fresh` record — a receipt telling
	// an agent a healthy source is broken, which inverts the very thing the typed
	// code exists to communicate.
	p.Status.ErrorCode = ""
	return IngestResult{Status: p.Status}, nil
}
