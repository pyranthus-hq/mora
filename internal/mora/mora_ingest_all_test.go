package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
	"io"
	"strings"
	"testing"
)

// TestIngestRunAllContinuesPastFailingSource locks the scheduled-refresh
// contract: `ingest run --all` (the ingest-hourly launchd job) must NOT abort
// on the first failing source. One broken connector (e.g. iMessage without
// Full Disk Access under launchd) was killing the whole run — later sources
// never synced and the final rebuildIndex never ran, so even the sources that
// DID ingest stayed invisible to search. The fix mirrors backfillEnabledGoogle:
// warn per failure, keep going, always rebuild, and return a usable partial
// result (exit 0) when at least one source succeeded and the rebuild is trusted.
func TestIngestRunAllContinuesPastFailingSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := saveSources(cfg, []Source{
		{Name: "bad", Type: "filesystem", Scope: "global", Enabled: genericutil.Ptr(true)},
		{Name: "good", Type: "filesystem", Scope: "global", Enabled: genericutil.Ptr(true)},
	}); err != nil {
		t.Fatalf("saveSources: %v", err)
	}

	orig := ingestSourceFn
	t.Cleanup(func() { ingestSourceFn = orig })
	var calls []string
	ingestSourceFn = func(cfg Config, s Source, out io.Writer) (sourceIngestResult, error) {
		calls = append(calls, s.Name)
		if s.Name == "bad" {
			return sourceIngestResult{}, errors.New("boom: connector down")
		}
		return sourceIngestResult{Examined: 2, Materialized: 2}, nil
	}

	var buf bytes.Buffer
	err = cmdIngest(context.Background(), []string{"run", "--all"}, &buf, testStderr)
	if err != nil {
		t.Fatalf("usable partial success must exit successfully: %v; output:\n%s", err, buf.String())
	}
	if len(calls) != 2 || calls[1] != "good" {
		t.Fatalf("later sources must still ingest after an earlier failure; calls=%v", calls)
	}
	if !strings.Contains(buf.String(), "warn") || !strings.Contains(buf.String(), "bad") {
		t.Fatalf("expected a per-source warn naming the failed source, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "ingested 2 item(s)") {
		t.Fatalf("expected the run to complete (count + rebuild) despite the failure, got:\n%s", buf.String())
	}
}

func TestIsolationPartialSuccessReceiptThroughIngestAll(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{
		{Name: "bad", Type: "filesystem", Scope: "global", Path: t.TempDir(), Enabled: genericutil.Ptr(true)},
		{Name: "good", Type: "filesystem", Scope: "global", Path: t.TempDir(), Enabled: genericutil.Ptr(true)},
	}); err != nil {
		t.Fatal(err)
	}
	orig := ingestSourceFn
	t.Cleanup(func() { ingestSourceFn = orig })
	ingestSourceFn = func(_ Config, source Source, _ io.Writer) (sourceIngestResult, error) {
		if source.Name == "bad" {
			return sourceIngestResult{}, newCodedError(errCodeConnectorUnavailable, nil, "offline")
		}
		return sourceIngestResult{Examined: 2, Materialized: 2}, nil
	}
	var stdout, stderr bytes.Buffer
	if err := cmdIngest(context.Background(), []string{"run", "--all", "--json"}, &stdout, &stderr); err != nil {
		t.Fatalf("mixed run: %v\nstderr=%s", err, stderr.String())
	}
	var receipt ingestRunReceipt
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
		t.Fatalf("decode one receipt: %v\n%s", err, stdout.String())
	}
	if receipt.Status != sourceRunStatusPartial || !receipt.Usable || receipt.SuccessfulSources != 1 || receipt.FailedSources != 1 || len(receipt.Sources) != 2 || receipt.Items != 2 {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestIsolationPartialAttemptCountsThroughIngestAll(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{{
		Name: "mail", Type: "gmail", Scope: "global", Enabled: genericutil.Ptr(true),
	}}); err != nil {
		t.Fatal(err)
	}
	orig := ingestSourceFn
	t.Cleanup(func() { ingestSourceFn = orig })
	ingestSourceFn = func(Config, Source, io.Writer) (sourceIngestResult, error) {
		return sourceIngestResult{Examined: 100, Materialized: 73, Failed: 27, Missing: 27},
			newCodedError(errCodeConnectorMalformed, nil, "27 malformed items")
	}
	var stdout bytes.Buffer
	if err := cmdIngest(context.Background(), []string{"run", "--all", "--json"}, &stdout, io.Discard); err != nil {
		t.Fatalf("usable partial attempt: %v\n%s", err, stdout.String())
	}
	var receipt ingestRunReceipt
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != sourceRunStatusPartial || !receipt.Usable || receipt.Items != 73 || len(receipt.Sources) != 1 {
		t.Fatalf("aggregate = %+v", receipt)
	}
	source := receipt.Sources[0]
	if source.Status != sourceRunStatusPartial || source.Examined != 100 || source.Materialized != 73 || source.Failed != 27 || source.Missing != 27 {
		t.Fatalf("source = %+v", source)
	}
}

func TestIsolationIngestReceiptCarriesStaleProvenance(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	source := Source{Name: "mail", Type: "gmail", Scope: "global", Enabled: genericutil.Ptr(true)}
	if err := saveSources(cfg, []Source{source}); err != nil {
		t.Fatal(err)
	}
	success := "2026-08-24T08:00:00Z"
	attempt := "2026-08-24T09:00:00Z"
	orig := ingestSourceFn
	t.Cleanup(func() { ingestSourceFn = orig })
	ingestSourceFn = func(cfg Config, source Source, _ io.Writer) (sourceIngestResult, error) {
		if err := memory.SaveStatus(syncStatusPathFor(cfg, source), &memory.SyncStatus{
			Source: source.Name, LastSynced: success, LastSuccessAt: success, LastAttemptAt: attempt,
			LastError: "offline", ErrorCode: errCodeConnectorUnavailable, ErrorCount: 1,
		}); err != nil {
			t.Fatal(err)
		}
		return sourceIngestResult{Examined: 2, Materialized: 1, Failed: 1, Missing: 1},
			newCodedError(errCodeConnectorUnavailable, nil, "offline")
	}
	var stdout bytes.Buffer
	if err := cmdIngest(context.Background(), []string{"run", "--all", "--json"}, &stdout, io.Discard); err != nil {
		t.Fatalf("usable partial: %v", err)
	}
	var receipt ingestRunReceipt
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	got := receipt.Sources[0]
	if got.LastSuccessAt != success || got.LastAttemptAt != attempt || !got.Stale || got.ErrorCode != errCodeConnectorUnavailable {
		t.Fatalf("source provenance = %+v", got)
	}
}

func TestIngestRunAllFailedEmitsEveryReceiptBeforeError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{
		{Name: "a", Type: "filesystem", Scope: "global", Path: t.TempDir(), Enabled: genericutil.Ptr(true)},
		{Name: "b", Type: "filesystem", Scope: "global", Path: t.TempDir(), Enabled: genericutil.Ptr(true)},
	}); err != nil {
		t.Fatal(err)
	}
	orig := ingestSourceFn
	t.Cleanup(func() { ingestSourceFn = orig })
	ingestSourceFn = func(_ Config, source Source, _ io.Writer) (sourceIngestResult, error) {
		return sourceIngestResult{}, newCodedError(errCodeConnectorUnavailable, nil, "%s offline", source.Name)
	}
	var stdout bytes.Buffer
	err := cmdIngest(context.Background(), []string{"run", "--all", "--json"}, &stdout, io.Discard)
	if err == nil {
		t.Fatal("all-failed run must return nonzero")
	}
	var receipt ingestRunReceipt
	if decodeErr := json.Unmarshal(stdout.Bytes(), &receipt); decodeErr != nil {
		t.Fatalf("decode receipt before error: %v\n%s", decodeErr, stdout.String())
	}
	if receipt.Status != sourceRunStatusFailed || receipt.Usable || receipt.FailedSources != 2 || len(receipt.Sources) != 2 {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestIngestRunSharedRebuildFailureIsNotUsable(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{{Name: "good", Type: "filesystem", Path: t.TempDir(), Enabled: genericutil.Ptr(true)}}); err != nil {
		t.Fatal(err)
	}
	origIngest, origRebuild := ingestSourceFn, rebuildIngestIndexFn
	t.Cleanup(func() { ingestSourceFn, rebuildIngestIndexFn = origIngest, origRebuild })
	ingestSourceFn = func(Config, Source, io.Writer) (sourceIngestResult, error) {
		return sourceIngestResult{Examined: 2, Materialized: 2}, nil
	}
	rebuildIngestIndexFn = func(context.Context, Config) (int, error) { return 0, errors.New("shared rebuild failed") }
	var stdout bytes.Buffer
	err := cmdIngest(context.Background(), []string{"run", "--all", "--json"}, &stdout, io.Discard)
	if err == nil {
		t.Fatal("shared rebuild failure must return nonzero")
	}
	var receipt ingestRunReceipt
	if decodeErr := json.Unmarshal(stdout.Bytes(), &receipt); decodeErr != nil {
		t.Fatalf("decode receipt: %v\n%s", decodeErr, stdout.String())
	}
	if receipt.Status != sourceRunStatusFailed || receipt.Usable || len(receipt.Sources) != 1 || receipt.Sources[0].Usable {
		t.Fatalf("shared failure receipt = %+v", receipt)
	}
}

// TestIngestRunNamedSourceStillAborts pins the single-source path: a named
// source failure is THE result of the command — it errors immediately (no
// warn-and-continue semantics to hide behind).
func TestIngestRunNamedSourceStillAborts(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := saveSources(cfg, []Source{
		{Name: "bad", Type: "filesystem", Scope: "global", Enabled: genericutil.Ptr(true)},
	}); err != nil {
		t.Fatalf("saveSources: %v", err)
	}
	orig := ingestSourceFn
	t.Cleanup(func() { ingestSourceFn = orig })
	ingestSourceFn = func(cfg Config, s Source, out io.Writer) (sourceIngestResult, error) {
		return sourceIngestResult{}, errors.New("boom")
	}
	var buf bytes.Buffer
	if err := cmdIngest(context.Background(), []string{"run", "--source", "bad"}, &buf, testStderr); err == nil {
		t.Fatalf("named-source failure must abort with the error")
	}
}
