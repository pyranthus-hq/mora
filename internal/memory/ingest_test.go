package memory

import (
	"path/filepath"
	"testing"
	"time"
)

func twoPageFetcher() *fakeFetcher {
	return &fakeFetcher{pages: map[string]Page{
		"": {Items: []Item{
			{Kind: kindGmailThread, ProviderID: "t1", Title: "A", Body: "a", OccurredAt: time.Now()},
			{Kind: kindGmailThread, ProviderID: "t2", Title: "B", Body: "b", OccurredAt: time.Now()},
		}, NextCursor: "p2"},
		"p2": {Items: []Item{
			{Kind: kindGmailThread, ProviderID: "t3", Title: "C", Body: "c", OccurredAt: time.Now()},
		}, NextCursor: ""},
	}}
}

func requireStatus(t *testing.T, res IngestResult) *SyncStatus {
	t.Helper()
	if res.Status == nil {
		t.Fatal("expected result status")
	}
	return res.Status
}

func TestIngestAllPages(t *testing.T) {
	f := twoPageFetcher()
	var written []MappedMemory
	res, err := Ingest(IngestParams{
		Fetcher: f, Kind: kindGmailThread, Scope: "personal", BodyBudget: 1000,
		Status: &SyncStatus{Source: "gmail"},
		Write:  func(m MappedMemory) error { written = append(written, m); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	status := requireStatus(t, res)
	if len(written) != 3 || status.ItemCount != 3 {
		t.Fatalf("expected 3 written, got %d (status %d)", len(written), status.ItemCount)
	}
	if status.Checkpoint != "" {
		t.Fatalf("checkpoint should be cleared on success, got %q", status.Checkpoint)
	}
	if written[0].Scope != "personal" || written[0].ProviderID != "t1" {
		t.Fatalf("expected mapped item with scope personal and provider t1, got %+v", written[0])
	}
}

func TestIngestResumesFromCheckpoint(t *testing.T) {
	f := twoPageFetcher()
	var written []MappedMemory
	res, err := Ingest(IngestParams{
		Fetcher: f, Kind: kindGmailThread, Scope: "personal", BodyBudget: 1000,
		Status: &SyncStatus{Source: "gmail", Checkpoint: "p2"},
		Write:  func(m MappedMemory) error { written = append(written, m); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || written[0].ProviderID != "t3" {
		t.Fatalf("resume should write only t3, got %+v", written)
	}
	if f.calls[0] != "p2" {
		t.Fatalf("resume should request cursor p2 first, got %v", f.calls)
	}
	requireStatus(t, res)
}

func TestIngestWriteErrorIsCounted(t *testing.T) {
	f := twoPageFetcher()
	res, err := Ingest(IngestParams{
		Fetcher: f, Kind: kindGmailThread, Scope: "personal", BodyBudget: 1000,
		Status: &SyncStatus{Source: "gmail"},
		Write: func(m MappedMemory) error {
			if m.ProviderID == "t2" {
				return errWrite
			}
			return nil
		},
	})
	if err == nil {
		t.Fatal("a run that dropped an item must return a non-nil error (the loop still must not abort — TestIngestWriteErrorContinues)")
	}
	status := requireStatus(t, res)
	if status.ErrorCount != 1 {
		t.Fatalf("expected 1 error counted, got %d", status.ErrorCount)
	}
}

func TestIngestWriteErrorContinues(t *testing.T) {
	f := twoPageFetcher()
	var written []string
	res, err := Ingest(IngestParams{
		Fetcher: f, Kind: kindGmailThread, Scope: "personal", BodyBudget: 1000,
		Status: &SyncStatus{Source: "gmail"},
		Write: func(m MappedMemory) error {
			if m.ProviderID == "t2" {
				return errWrite
			}
			written = append(written, m.ProviderID)
			return nil
		},
	})
	if err == nil {
		t.Fatal("a run that dropped an item must return a non-nil error; the assertions below pin that it still did NOT abort")
	}
	status := requireStatus(t, res)
	if status.ItemCount != 2 {
		t.Fatalf("expected 2 successful writes counted, got %d", status.ItemCount)
	}
	if status.LastError != errWrite.Error() {
		t.Fatalf("expected last error %q, got %q", errWrite.Error(), status.LastError)
	}
	if len(written) != 2 || written[0] != "t1" || written[1] != "t3" {
		t.Fatalf("write error should not stop later items, got %v", written)
	}
	if status.Checkpoint != "" {
		t.Fatalf("checkpoint should be cleared on success, got %q", status.Checkpoint)
	}
}

func TestIngestFetchErrorPreservesCheckpoint(t *testing.T) {
	f := twoPageFetcher()
	f.errOnCursor = map[string]error{"p2": errFetch}
	var written []MappedMemory
	res, err := Ingest(IngestParams{
		Fetcher: f, Kind: kindGmailThread, Scope: "personal", BodyBudget: 1000,
		Status: &SyncStatus{Source: "gmail"},
		Write:  func(m MappedMemory) error { written = append(written, m); return nil },
	})
	if err != errFetch {
		t.Fatalf("expected fetch error %v, got %v", errFetch, err)
	}
	status := requireStatus(t, res)
	if status.Checkpoint != "p2" {
		t.Fatalf("checkpoint should preserve failed fetch cursor p2, got %q", status.Checkpoint)
	}
	if status.ErrorCount != 1 {
		t.Fatalf("expected fetch error counted once, got %d", status.ErrorCount)
	}
	if len(written) != 2 {
		t.Fatalf("expected first page items written before fetch error, got %d", len(written))
	}
}

// M-3 regression: a source that errored on a prior run then completes a clean
// sync must read healthy (ErrorCount=0, LastError=""), not "unavailable" forever.
// D-03 derives "unavailable" from exactly these fields.
func TestIngestCleanSyncResetsErrorState(t *testing.T) {
	f := twoPageFetcher()
	res, err := Ingest(IngestParams{
		Fetcher: f, Kind: kindGmailThread, Scope: "personal", BodyBudget: 1000,
		// Simulate a source that errored twice on a prior run, then recovered.
		Status: &SyncStatus{Source: "gmail", ErrorCount: 2, LastError: "boom"},
		Write:  func(m MappedMemory) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	status := requireStatus(t, res)
	if status.ErrorCount != 0 {
		t.Fatalf("clean sync must reset ErrorCount to 0, got %d", status.ErrorCount)
	}
	if status.LastError != "" {
		t.Fatalf("clean sync must clear LastError, got %q", status.LastError)
	}
}

// A clean sync stamps LastAttemptAt and LastSuccessAt as last-attempt outcome
// fields; LastSuccessAt equals LastSynced (same instant) so the digest can
// distinguish "succeeded but stale" from "never succeeded".
func TestIngestCleanSyncStampsSuccessTimestamps(t *testing.T) {
	f := twoPageFetcher()
	res, err := Ingest(IngestParams{
		Fetcher: f, Kind: kindGmailThread, Scope: "personal", BodyBudget: 1000,
		Status: &SyncStatus{Source: "gmail"},
		Write:  func(m MappedMemory) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	status := requireStatus(t, res)
	if status.LastAttemptAt == "" {
		t.Fatal("clean sync must stamp LastAttemptAt")
	}
	if status.LastSuccessAt == "" {
		t.Fatal("clean sync must stamp LastSuccessAt")
	}
	if status.LastSuccessAt != status.LastSynced {
		t.Fatalf("LastSuccessAt (%q) must equal LastSynced (%q) on a clean sync",
			status.LastSuccessAt, status.LastSynced)
	}
	if _, perr := time.Parse(time.RFC3339, status.LastSuccessAt); perr != nil {
		t.Fatalf("LastSuccessAt must be RFC3339, got %q (%v)", status.LastSuccessAt, perr)
	}
	if _, perr := time.Parse(time.RFC3339, status.LastAttemptAt); perr != nil {
		t.Fatalf("LastAttemptAt must be RFC3339, got %q (%v)", status.LastAttemptAt, perr)
	}
}

// A sync that errors out (page-fetch failure) stamps LastAttemptAt but leaves
// LastSuccessAt untouched, and does NOT reset the error tally — so "never
// succeeded" stays distinguishable from "succeeded but stale".
func TestIngestFetchErrorStampsAttemptNotSuccess(t *testing.T) {
	f := twoPageFetcher()
	f.errOnCursor = map[string]error{"p2": errFetch}
	res, err := Ingest(IngestParams{
		Fetcher: f, Kind: kindGmailThread, Scope: "personal", BodyBudget: 1000,
		Status: &SyncStatus{Source: "gmail"},
		Write:  func(m MappedMemory) error { return nil },
	})
	if err != errFetch {
		t.Fatalf("expected fetch error %v, got %v", errFetch, err)
	}
	status := requireStatus(t, res)
	if status.LastAttemptAt == "" {
		t.Fatal("a failed attempt must still stamp LastAttemptAt")
	}
	if status.LastSuccessAt != "" {
		t.Fatalf("a failed attempt must NOT stamp LastSuccessAt, got %q", status.LastSuccessAt)
	}
	if status.ErrorCount != 1 {
		t.Fatalf("error path must not reset ErrorCount, got %d", status.ErrorCount)
	}
	if status.LastError == "" {
		t.Fatal("error path must record LastError")
	}
}

// Zero-value safe: LoadStatus on a missing file returns a zero SyncStatus with
// empty new fields (no panic, no spurious "errored").
func TestLoadStatusMissingFileZeroesNewFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	s, err := LoadStatus(path)
	if err != nil {
		t.Fatalf("LoadStatus on missing file must not error, got %v", err)
	}
	if s == nil {
		t.Fatal("LoadStatus must return a zero-value status, got nil")
	}
	if s.LastAttemptAt != "" || s.LastSuccessAt != "" {
		t.Fatalf("missing-file status must zero new fields, got attempt=%q success=%q",
			s.LastAttemptAt, s.LastSuccessAt)
	}
	if s.ErrorCount != 0 || s.LastError != "" {
		t.Fatalf("missing-file status must not look errored, got count=%d err=%q",
			s.ErrorCount, s.LastError)
	}
}

func TestIngestMapsWithScopeAndBodyBudget(t *testing.T) {
	f := &fakeFetcher{pages: map[string]Page{
		"": {Items: []Item{
			{Kind: kindGmailThread, ProviderID: "t4", Title: "D", Body: "abcdef", OccurredAt: time.Now()},
		}},
	}}
	var written []MappedMemory
	res, err := Ingest(IngestParams{
		Fetcher: f, Kind: kindGmailThread, Scope: "work", BodyBudget: 3,
		Status: &SyncStatus{Source: "gmail"},
		Write:  func(m MappedMemory) error { written = append(written, m); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, res)
	if len(written) != 1 {
		t.Fatalf("expected one written item, got %d", len(written))
	}
	if written[0].Scope != "work" || written[0].Body != "abc" || !written[0].Truncated {
		t.Fatalf("expected MapItem to apply scope and body budget, got %+v", written[0])
	}
	if written[0].OriginalSize != 6 || written[0].IngestedSize != 3 {
		t.Fatalf("expected mapped body sizes 6/3, got %d/%d", written[0].OriginalSize, written[0].IngestedSize)
	}
}

// TestIngestWriteErrorsSurfaceInReturnedError pins the partial-failure
// contract: a run that dropped items must NOT return nil — nil told callers
// (and `mora sync`'s exit status) the run was healthy while memories were
// silently missing. Per-item failures still never abort the loop (the other
// items land — TestIngestWriteErrorContinues), but a completed run that
// dropped anything reports it.
func TestIngestWriteErrorsSurfaceInReturnedError(t *testing.T) {
	f := twoPageFetcher()
	res, err := Ingest(IngestParams{
		Fetcher: f, Kind: kindGmailThread, Scope: "personal", BodyBudget: 1000,
		Status: &SyncStatus{Source: "gmail"},
		Write: func(m MappedMemory) error {
			if m.ProviderID == "t2" {
				return errWrite
			}
			return nil
		},
	})
	if err == nil {
		t.Fatal("Ingest returned nil despite a dropped item; partial failure must surface to the caller")
	}
	status := requireStatus(t, res)
	if status.ItemCount != 2 || status.ErrorCount != 1 {
		t.Fatalf("expected 2 written / 1 dropped, got items=%d errors=%d", status.ItemCount, status.ErrorCount)
	}
	// Paging completed: the checkpoint clears so the NEXT run re-fetches from
	// the start and re-attempts the dropped item (instead of resuming past it).
	if status.Checkpoint != "" {
		t.Fatalf("checkpoint should be cleared on completed paging, got %q", status.Checkpoint)
	}
}

// TestIngestPartialFailurePreservesSuccessTimestamps pins the freshness-honesty
// half of the partial-failure contract: a run that dropped items is NOT a clean
// success, so it must NOT advance LastSynced/LastSuccessAt (which would make
// search report incomplete data as fresh and make skip-if-fresh suppress the
// retry for an hour). It stamps LastAttemptAt only; the prior success timestamps
// survive untouched. (Codex phase-0 review P1.)
func TestIngestPartialFailurePreservesSuccessTimestamps(t *testing.T) {
	const priorSuccess = "2020-01-02T03:04:05Z"
	f := twoPageFetcher()
	res, err := Ingest(IngestParams{
		Fetcher: f, Kind: kindGmailThread, Scope: "personal", BodyBudget: 1000,
		Status: &SyncStatus{Source: "gmail", LastSynced: priorSuccess, LastSuccessAt: priorSuccess},
		Write: func(m MappedMemory) error {
			if m.ProviderID == "t2" {
				return errWrite
			}
			return nil
		},
	})
	if err == nil {
		t.Fatal("partial failure must return a non-nil error")
	}
	status := requireStatus(t, res)
	if status.LastSuccessAt != priorSuccess {
		t.Errorf("partial failure must preserve prior LastSuccessAt %q, got %q", priorSuccess, status.LastSuccessAt)
	}
	if status.LastSynced != priorSuccess {
		t.Errorf("partial failure must preserve prior LastSynced %q (not report stale data as fresh), got %q", priorSuccess, status.LastSynced)
	}
	if status.LastAttemptAt == "" || status.LastAttemptAt == priorSuccess {
		t.Errorf("partial failure must still stamp a fresh LastAttemptAt, got %q", status.LastAttemptAt)
	}
}
