package mora

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestObservabilityFreshRequiresTimestampInsideBudget(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	st := &memory.SyncStatus{LastSuccessAt: now.Add(-time.Minute).Format(time.RFC3339), ObservedAt: now.Add(-25 * time.Hour).Format(time.RFC3339), FreshnessBudgetSeconds: 24 * 60 * 60}
	if got := syncStatusFileState(st, 24*time.Hour, now); got == healthFresh {
		t.Fatal("fresh accepted an observation outside its declared budget")
	}
	st.ObservedAt = now.Add(-time.Minute).Format(time.RFC3339)
	if got := syncStatusFileState(st, 24*time.Hour, now); got != healthFresh {
		t.Fatalf("recent timestamped observation = %q, want fresh", got)
	}
}

func TestObservabilitySourceReceiptCarriesCompleteSLO(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	st := &memory.SyncStatus{Source: "gmail", LastSuccessAt: now.Format(time.RFC3339), LastAttemptAt: now.Format(time.RFC3339), ObservedAt: now.Format(time.RFC3339), NextScheduledAt: now.Add(time.Hour).Format(time.RFC3339), DurationMS: 42, FreshnessBudgetSeconds: 3600, ConsecutiveFailureCount: 0, CorrelationID: "op_trace"}
	if err := memory.SaveStatus(filepath.Join(dir, "google-gmail.json"), st); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	rows := syncStatusReceiptSources(entries, dir, now)
	if len(rows) != 1 || rows[0].ObservedAt == "" || rows[0].LastSuccessAt == "" || rows[0].LastAttemptAt == "" || rows[0].NextScheduledAt == "" || rows[0].DurationMS != 42 || rows[0].FreshnessBudgetSeconds != 3600 || rows[0].CorrelationID != "op_trace" {
		t.Fatalf("incomplete source SLO receipt: %+v", rows)
	}
}

func TestObservabilityTraceLinksRetrievalToIngestAndDoctor(t *testing.T) {
	m := Memory{ID: "gmail_thread/1", Provider: "gmail", CreatedAt: "2026-08-24T12:00:00Z", Meta: map[string]any{"ingest_correlation_id": "op_ingest"}}
	manifest := evidenceManifest([]Memory{m}, []Memory{m})
	if len(manifest) != 1 || manifest[0].IngestCorrelationID != "op_ingest" {
		t.Fatalf("manifest trace link: %+v", manifest)
	}
	_, diagnoses := buildDoctorDiagnostics(nil, []sourceHealth{{Key: "gmail", State: healthFailed, ErrorCode: errCodeConnectorUnavailable, DiagnosticEvidenceID: "op_ingest"}}, time.Now())
	if len(diagnoses) != 1 {
		t.Fatalf("doctor diagnoses: %+v", diagnoses)
	}
	observed, _ := buildDoctorDiagnostics(nil, []sourceHealth{{Key: "gmail", State: healthFailed, ErrorCode: errCodeConnectorUnavailable, DiagnosticEvidenceID: "op_ingest"}}, time.Now())
	if len(observed) != 1 || observed[0].DiagnosticEvidenceID != "op_ingest" {
		t.Fatalf("doctor evidence link: %+v", observed)
	}
}

func TestObservabilityStructuredTraceUsesStableStages(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	for _, stage := range []string{traceStageSchedule, traceStageConnector, traceStageIngestion, traceStageIndex, traceStageQuery} {
		if err := appendTraceEvent(cfg, traceEvent{CorrelationID: "op_trace", Stage: stage, Status: "completed"}); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(cfg.StateDir, "observability", "traces.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytesLines(b)
	if len(lines) != 5 {
		t.Fatalf("trace lines=%d", len(lines))
	}
	info, err := os.Stat(filepath.Join(cfg.StateDir, "observability", "traces.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("trace permissions=%v", info.Mode().Perm())
	}
	for _, line := range lines {
		var event traceEvent
		if err := json.Unmarshal(line, &event); err != nil || event.SchemaVersion != 1 || event.CorrelationID != "op_trace" || event.ObservedAt == "" {
			t.Fatalf("trace event=%s err=%v", line, err)
		}
	}
}

func bytesLines(b []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(line) > 0 {
			out = append(out, line)
		}
	}
	return out
}

func TestProductionReadinessConnectorFixtures(t *testing.T) {
	b, err := os.ReadFile("testdata/connectors/observability-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Fixtures []struct {
			Name    string `json:"name"`
			Records int    `json:"records"`
			Bounded bool   `json:"bounded"`
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	want := []string{"fresh", "stale", "unavailable", "malformed", "duplicate", "high-volume"}
	if len(doc.Fixtures) != len(want) {
		t.Fatalf("fixtures=%d", len(doc.Fixtures))
	}
	for i, name := range want {
		if doc.Fixtures[i].Name != name {
			t.Fatalf("fixture[%d]=%q want %q", i, doc.Fixtures[i].Name, name)
		}
	}
	if last := doc.Fixtures[len(doc.Fixtures)-1]; last.Records < 100000 || !last.Bounded {
		t.Fatalf("high-volume fixture is not bounded: %+v", last)
	}
}
