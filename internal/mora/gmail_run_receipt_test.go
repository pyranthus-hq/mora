package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestGmailMetricsFailurePreservesConnectorOutcome(t *testing.T) {
	connectorErr := errors.New("connector failed")
	for _, tc := range []struct {
		name string
		err  error
	}{{"success", nil}, {"failure", connectorErr}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := gate2Vault(t)
			s := Source{Name: "gmail", Type: "gmail"}
			path := syncStatusPathFor(cfg, s)
			previous := ingestSourceDispatchFn
			t.Cleanup(func() { ingestSourceDispatchFn = previous })
			ingestSourceDispatchFn = func(_ context.Context, _ Config, _ Source, _ io.Writer) (sourceIngestResult, error) {
				if err := memory.SaveStatus(path, &memory.SyncStatus{IncrementalCursor: "committed", LastSuccessAt: "2026-11-18T00:00:00Z"}); err != nil {
					t.Fatal(err)
				}
				return sourceIngestResult{GmailRun: &google.GmailRunStats{HistoryCalls: -1}}, tc.err
			}
			var out bytes.Buffer
			_, err := ingestSourceDetailed(context.Background(), cfg, s, &out)
			if (tc.err == nil && err != nil) || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("connector outcome: %v", err)
			}
			if !strings.Contains(out.String(), "Gmail run metrics missing from operation receipt") {
				t.Fatalf("missing warning: %s", out.String())
			}
			st, err := memory.LoadStatus(path)
			if err != nil || st.IncrementalCursor != "committed" || st.LastSuccessAt != "2026-11-18T00:00:00Z" || (tc.err == nil && st.ConsecutiveFailureCount != 0) || (tc.err != nil && st.ConsecutiveFailureCount == 0) {
				t.Fatalf("status: %+v, error: %v", st, err)
			}
		})
	}
}

func TestGoogleRunReceiptInstances(t *testing.T) {
	cfg := gate2Vault(t)
	sources := []Source{{Name: "gmail", Type: "gmail"}, {Name: "work", Type: "gmail", Account: "work"}}
	if err := saveSources(cfg, sources); err != nil {
		t.Fatal(err)
	}
	previous := ingestSourceDispatchFn
	t.Cleanup(func() { ingestSourceDispatchFn = previous })
	ingestSourceDispatchFn = func(_ context.Context, _ Config, s Source, _ io.Writer) (sourceIngestResult, error) {
		st := google.GmailRunStats{HistoryCalls: 1, DeltaClass: "empty_delta"}
		if s.Account != "" {
			st.ThreadGetCalls = 2
			st.DeltaClass = "new_threads"
		}
		return sourceIngestResult{GmailRun: &st}, nil
	}
	var runs []gmailRunReceipt
	if _, err := backfillEnabledGoogleWithStats(context.Background(), cfg, io.Discard, &runs); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := emitSyncSourceResultWithGmail(cfg, &out, "google", true, 0, "", runs); err != nil {
		t.Fatal(err)
	}
	var got struct {
		GmailRuns []gmailRunReceipt `json:"gmail_runs"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	bySource := map[string]google.GmailRunStats{}
	for _, r := range got.GmailRuns {
		bySource[r.Source] = r.GmailRunStats
	}
	if len(bySource) != 2 || bySource["gmail"].DeltaClass != "empty_delta" || bySource["gmail:work"].ThreadGetCalls != 2 {
		t.Fatalf("JSON: %s", out.String())
	}
	files, err := filepath.Glob(filepath.Join(cfg.StateDir, "operations", "ingest", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			GmailRun *gmailRunReceipt `json:"gmail_run"`
		}
		if err = json.Unmarshal(b, &record); err != nil {
			t.Fatal(err)
		}
		if record.GmailRun != nil {
			r := record.GmailRun
			if want, ok := bySource[r.Source]; !ok || r.GmailRunStats != want {
				t.Fatalf("receipt mismatch: %s", b)
			}
			seen[r.Source] = true
		}
	}
	if len(seen) != 2 {
		t.Fatalf("missing per-instance operation receipts: %v", seen)
	}
}
