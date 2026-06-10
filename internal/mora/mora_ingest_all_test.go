package mora

import (
	"bytes"
	"context"
	"errors"
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
// warn per failure, keep going, always rebuild, return an aggregate error
// (honest non-zero exit — never swallow sync errors).
func TestIngestRunAllContinuesPastFailingSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := saveSources(cfg, []Source{
		{Name: "bad", Type: "filesystem", Scope: "global", Enabled: ptr(true)},
		{Name: "good", Type: "filesystem", Scope: "global", Enabled: ptr(true)},
	}); err != nil {
		t.Fatalf("saveSources: %v", err)
	}

	orig := ingestSourceFn
	t.Cleanup(func() { ingestSourceFn = orig })
	var calls []string
	ingestSourceFn = func(cfg Config, s Source, out io.Writer) (int, error) {
		calls = append(calls, s.Name)
		if s.Name == "bad" {
			return 0, errors.New("boom: connector down")
		}
		return 2, nil
	}

	var buf bytes.Buffer
	err = cmdIngest(context.Background(), []string{"run", "--all"}, &buf)
	if err == nil {
		t.Fatalf("expected an aggregate error when a source fails (never swallow sync errors); output:\n%s", buf.String())
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
		{Name: "bad", Type: "filesystem", Scope: "global", Enabled: ptr(true)},
	}); err != nil {
		t.Fatalf("saveSources: %v", err)
	}
	orig := ingestSourceFn
	t.Cleanup(func() { ingestSourceFn = orig })
	ingestSourceFn = func(cfg Config, s Source, out io.Writer) (int, error) {
		return 0, errors.New("boom")
	}
	var buf bytes.Buffer
	if err := cmdIngest(context.Background(), []string{"run", "--source", "bad"}, &buf); err == nil {
		t.Fatalf("named-source failure must abort with the error")
	}
}
