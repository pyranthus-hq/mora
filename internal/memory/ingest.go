package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// IngestParams drives a snapshot ingestion. Write persists one memory (the mora
// wiring supplies it). A nil-safe Status is required.
type IngestParams struct {
	Context    context.Context
	Fetcher    Fetcher
	Kind       ItemKind
	Window     FetchWindow
	Scope      string
	BodyBudget int
	Status     *SyncStatus
	Write      func(MappedMemory) error
	// WriteResult reports whether the mapped record actually changed durable
	// state. When set it supersedes Write, so unchanged content-hash skips do not
	// inflate materialized counts or trigger unnecessary rebuild work.
	WriteResult func(MappedMemory) (bool, error)
	// Checkpoint durably publishes an advanced page cursor before the next page
	// is fetched. A failure stops ingestion, preserving resumability rather than
	// claiming progress that only existed in memory.
	Checkpoint func(*SyncStatus) error
	Limits     IngestLimits

	// Map optionally overrides how a fetched Item becomes a MappedMemory. When nil,
	// the shared MapItem (start-keep truncation) is used — Gmail/Calendar behavior. A
	// connector needing different mapping (e.g. iMessage's newest-first truncation via
	// its own mapConversation over Item.Payload) supplies it here.
	Map func(Item, string, int) MappedMemory
}

type IngestLimits struct {
	MaxRecordBytes int
	MaxBatchItems  int
	MaxBatchBytes  int
	MaxRecords     int
	MaxPages       int
	MaxRetries     int
	MaxRuntime     time.Duration
}

func (l IngestLimits) bounded() IngestLimits {
	if l.MaxRecordBytes <= 0 {
		l.MaxRecordBytes = 4 << 20
	}
	if l.MaxBatchItems <= 0 {
		l.MaxBatchItems = 500
	}
	if l.MaxBatchBytes <= 0 {
		l.MaxBatchBytes = 64 << 20
	}
	if l.MaxRecords <= 0 {
		l.MaxRecords = 1_000_000
	}
	if l.MaxPages <= 0 {
		l.MaxPages = 100_000
	}
	if l.MaxRetries < 0 {
		l.MaxRetries = 0
	} else if l.MaxRetries == 0 {
		l.MaxRetries = 2
	}
	if l.MaxRuntime <= 0 {
		l.MaxRuntime = 15 * time.Minute
	}
	return l
}

type IngestStages struct {
	FetchMS    int64 `json:"fetch_ms"`
	MapWriteMS int64 `json:"map_write_ms"`
	TotalMS    int64 `json:"total_ms"`
	Pages      int   `json:"pages"`
	Bytes      int64 `json:"bytes"`
	Retries    int   `json:"retries"`
}

func retryableFetchError(err error) bool {
	var retryable interface{ Retryable() bool }
	if errors.As(err, &retryable) {
		return retryable.Retryable()
	}
	var temporary interface{ Temporary() bool }
	return errors.As(err, &temporary) && temporary.Temporary()
}

func fetchPageBounded(ctx context.Context, p IngestParams, window FetchWindow, cursor string, maxRetries int) (Page, int, error) {
	for attempt := 0; ; attempt++ {
		page, err := fetchPageContext(ctx, p.Fetcher, p.Kind, window, cursor)
		if err == nil || attempt >= maxRetries || !retryableFetchError(err) {
			return page, attempt, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Page{}, attempt, ctx.Err()
		case <-timer.C:
		}
	}
}

func fetchPageContext(ctx context.Context, fetcher Fetcher, kind ItemKind, window FetchWindow, cursor string) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if contextual, ok := fetcher.(ContextFetcher); ok {
		return contextual.FetchPageContext(ctx, kind, window, cursor)
	}
	return fetcher.FetchPage(kind, window, cursor)
}

type IngestResult struct {
	Status       *SyncStatus
	Examined     int
	Materialized int
	Failed       int
	Unchanged    int
	Missing      int
	Stages       IngestStages
	Incremental  bool
}

func itemApproxBytes(item Item) int {
	n := len(item.ProviderID) + len(item.Title) + len(item.Body)
	for _, tag := range item.Tags {
		n += len(tag)
	}
	for _, attachment := range item.Attachments {
		n += len(attachment.Filename) + len(attachment.MimeType) + len(attachment.Path)
	}
	if item.Meta != nil {
		if b, err := json.Marshal(item.Meta); err == nil {
			n += len(b)
		}
	}
	return n
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
	ctx := p.Context
	if ctx == nil {
		ctx = context.Background()
	}
	limits := p.Limits.bounded()
	ctx, cancel := context.WithTimeout(ctx, limits.MaxRuntime)
	defer cancel()
	started := time.Now()
	if p.Status == nil {
		p.Status = &SyncStatus{}
	}
	mapFn := p.Map
	if mapFn == nil {
		mapFn = MapItem
	}
	cursor := p.Status.Checkpoint
	result := IngestResult{Status: p.Status, Incremental: cursor == "" && p.Status.IncrementalCursor != ""}
	if cursor == "" {
		p.Window.SyncCursor = p.Status.IncrementalCursor
	}
	nextSyncCursor := p.Status.IncrementalCursor
	fallbackUsed := false
	// Snapshot the prior error tally so the clean-completion reset only clears
	// errors carried in from a PRIOR run — a run that itself accumulates per-item
	// write errors is not a clean attempt and keeps its errors (M-3: health is the
	// last attempt's outcome, not a "paging finished" signal).
	errorsBefore := p.Status.ErrorCount
	finish := func() IngestResult {
		p.Status.ObservedAt = time.Now().UTC().Format(time.RFC3339)
		p.Status.DurationMS = time.Since(started).Milliseconds()
		result.Missing = result.Examined - result.Materialized - result.Unchanged
		result.Stages.TotalMS = time.Since(started).Milliseconds()
		return result
	}
	for {
		if result.Stages.Pages >= limits.MaxPages {
			return finish(), fmt.Errorf("ingest page limit exceeded (%d)", limits.MaxPages)
		}
		fetchStarted := time.Now()
		page, retries, err := fetchPageBounded(ctx, p, p.Window, cursor, limits.MaxRetries)
		result.Stages.Retries += retries
		result.Stages.FetchMS += time.Since(fetchStarted).Milliseconds()
		if err != nil && !fallbackUsed && p.Window.SyncCursor != "" && errors.Is(err, ErrIncrementalCursorExpired) {
			fallbackUsed = true
			p.Window.SyncCursor = ""
			p.Status.IncrementalCursor = ""
			nextSyncCursor = ""
			cursor = ""
			p.Status.Checkpoint = ""
			continue
		}
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
			return finish(), err
		}
		result.Stages.Pages++
		if len(page.Items) > limits.MaxBatchItems {
			return finish(), fmt.Errorf("ingest batch limit exceeded: %d > %d", len(page.Items), limits.MaxBatchItems)
		}
		batchBytes := 0
		itemSizes := make([]int, len(page.Items))
		for i, item := range page.Items {
			itemSizes[i] = itemApproxBytes(item)
			batchBytes += itemSizes[i]
		}
		if batchBytes > limits.MaxBatchBytes {
			return finish(), fmt.Errorf("ingest batch memory limit exceeded: %d > %d bytes", batchBytes, limits.MaxBatchBytes)
		}
		if result.Examined+len(page.Items) > limits.MaxRecords {
			return finish(), fmt.Errorf("ingest record limit exceeded (%d)", limits.MaxRecords)
		}
		mapStarted := time.Now()
		if page.SyncCursor != "" {
			nextSyncCursor = page.SyncCursor
		}
		for itemIndex, it := range page.Items {
			result.Examined++
			itemBytes := itemSizes[itemIndex]
			result.Stages.Bytes += int64(itemBytes)
			if itemBytes > limits.MaxRecordBytes {
				result.Failed++
				p.Status.ErrorCount++
				p.Status.LastError = fmt.Sprintf("record %s exceeds %d-byte ingest limit", it.ProviderID, limits.MaxRecordBytes)
				p.Status.ErrorCode = ""
				continue
			}
			m := mapFn(it, p.Scope, p.BodyBudget)
			m.LastSynced = time.Now().UTC().Format(time.RFC3339)
			if err := ctx.Err(); err != nil {
				p.Status.ErrorCount++
				p.Status.LastError = err.Error()
				p.Status.ErrorCode = ""
				p.Status.LastAttemptAt = time.Now().UTC().Format(time.RFC3339)
				p.Status.Checkpoint = cursor
				return finish(), err
			}
			wrote := true
			var werr error
			if p.WriteResult != nil {
				wrote, werr = p.WriteResult(m)
			} else if p.Write != nil {
				werr = p.Write(m)
			}
			if werr != nil {
				result.Failed++
				p.Status.ErrorCount++
				p.Status.LastError = werr.Error()
				p.Status.ErrorCode = "" // prose only here; see the fetch-failure branch above
				continue
			}
			if wrote {
				result.Materialized++
				p.Status.ItemCount++
			} else {
				result.Unchanged++
			}
		}
		result.Stages.MapWriteMS += time.Since(mapStarted).Milliseconds()
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		p.Status.Checkpoint = cursor // advance checkpoint per page
		if p.Checkpoint != nil {
			if err := p.Checkpoint(p.Status); err != nil {
				p.Status.ErrorCount++
				p.Status.LastError = err.Error()
				p.Status.ErrorCode = ""
				p.Status.LastAttemptAt = time.Now().UTC().Format(time.RFC3339)
				return finish(), fmt.Errorf("persist ingest checkpoint: %w", err)
			}
		}
	}
	p.Status.Checkpoint = ""
	p.Status.IncrementalCursor = nextSyncCursor
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
		p.Status.ConsecutiveFailureCount++
		return finish(), fmt.Errorf("%d item(s) failed to write and were dropped (last error: %s) — successfully written items are saved; re-run the sync to retry", dropped, p.Status.LastError)
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
	p.Status.ConsecutiveFailureCount = 0
	return finish(), nil
}
