package mora

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func retentionEmptyFixture(t *testing.T) (Config, retentionReport, time.Time) {
	t.Helper()
	cfg := coreBIngestInitCfg(t)
	m := Memory{ID: "last-current", Scope: "personal", Type: "note", Title: "Last", Text: "Synthetic last memory", CreatedAt: "2024-01-01T00:00:00Z"}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	report, err := buildRetentionReport(cfg, now, 365, 30)
	if err != nil {
		t.Fatal(err)
	}
	report, err = decideRetentionCandidate(cfg, report.ReportID, m.ID, retentionDecision{Action: "delete"})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, report, now
}

func retentionSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		body := []byte(nil)
		if !d.IsDir() {
			body, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		out[path] = fmt.Sprintf("%v:%s", st.Mode(), body)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRetentionLastCurrentDeleteRefusedBeforeAnyMutation(t *testing.T) {
	cfg, report, now := retentionEmptyFixture(t)
	before := retentionSnapshot(t, lookupTestEnv(t).home)
	_, err := executeRetentionReport(context.Background(), cfg, report.ReportID, now)
	if err == nil || !strings.Contains(err.Error(), "no current memories") {
		t.Errorf("want explicit empty-result refusal, got %v", err)
	}
	after := retentionSnapshot(t, lookupTestEnv(t).home)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("empty-result refusal changed vault, index, governance, or recovery state")
	}
}

func TestRetentionHistoricalAndTombstoneFilesAreNotSurvivors(t *testing.T) {
	for _, kind := range []string{"historical", "tombstone"} {
		t.Run(kind, func(t *testing.T) {
			cfg, report, now := retentionEmptyFixture(t)
			m := Memory{ID: "not-current", Scope: "personal", Type: "note", Title: "History", Text: "Synthetic retained data", CreatedAt: "2026-10-29T00:00:00Z"}
			if kind == "tombstone" {
				m.DeletedAt = "2026-10-29T00:00:00Z"
			}
			if err := writeMemory(cfg, m); err != nil {
				t.Fatal(err)
			}
			if kind == "historical" {
				if _, err := appendGovernanceEntry(cfg, govEntry{Kind: govKindTeachMemory, Action: govActionRecord, TargetID: m.ID, Decision: teachMemoryRetract}); err != nil {
					t.Fatal(err)
				}
			}
			before := retentionSnapshot(t, lookupTestEnv(t).home)
			if _, err := executeRetentionReport(context.Background(), cfg, report.ReportID, now); err == nil || !strings.Contains(err.Error(), "no current memories") {
				t.Fatalf("%s treated as survivor: %v", kind, err)
			}
			if !reflect.DeepEqual(before, retentionSnapshot(t, lookupTestEnv(t).home)) {
				t.Fatal("refusal mutated files")
			}
		})
	}
}

func TestRetentionDeleteWithCurrentSurvivorSucceeds(t *testing.T) {
	cfg, report, now := retentionEmptyFixture(t)
	if err := writeMemory(cfg, Memory{ID: "survivor", Scope: "personal", Type: "note", Title: "Keep", Text: "Synthetic survivor", CreatedAt: "2026-10-29T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	got, err := executeRetentionReport(context.Background(), cfg, report.ReportID, now)
	if err != nil || got.IndexCount != 1 || len(got.DeletedIDs) != 1 || got.DeletedIDs[0] != "last-current" {
		t.Fatalf("untruthful delete receipt: %+v %v", got, err)
	}
	if _, err := findMemoryRaw(cfg, "last-current"); err == nil {
		t.Fatal("deleted target still exists")
	}
	if _, err := findMemoryRaw(cfg, "survivor"); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionLastCompactionCanPreserveCurrentCorpus(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		t.Run(fmt.Sprint(hidden), func(t *testing.T) {
			cfg, report, now := retentionEmptyFixture(t)
			report, err := decideRetentionCandidate(cfg, report.ReportID, "last-current", retentionDecision{Action: "compact", Summary: "Synthetic compact conclusion"})
			if err != nil {
				t.Fatal(err)
			}
			compactID := retentionCompactID(report.ReportID, "last-current")
			if hidden {
				if _, err := appendGovernanceEntry(cfg, govEntry{Kind: govKindTeachMemory, Action: govActionRecord, TargetID: compactID, Decision: teachMemoryRetract}); err != nil {
					t.Fatal(err)
				}
			}
			before := retentionSnapshot(t, lookupTestEnv(t).home)
			got, err := executeRetentionReport(context.Background(), cfg, report.ReportID, now)
			if hidden {
				if err == nil || !strings.Contains(err.Error(), "no current memories") {
					t.Fatalf("hidden compact output counted: %v", err)
				}
				if !reflect.DeepEqual(before, retentionSnapshot(t, lookupTestEnv(t).home)) {
					t.Fatal("hidden compact refusal mutated files")
				}
			} else {
				if err != nil || got.IndexCount != 1 || len(got.CompactedIDs) != 1 {
					t.Fatalf("compact receipt: %+v %v", got, err)
				}
				if _, err := findMemoryRaw(cfg, compactID); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRetentionSurvivorPreflightFailsClosedOnCorruption(t *testing.T) {
	for _, kind := range []string{"memory", "governance"} {
		t.Run(kind, func(t *testing.T) {
			cfg, report, now := retentionEmptyFixture(t)
			path := governancePath(cfg)
			if kind == "memory" {
				path = mustMemoryPath(t, cfg, Memory{ID: "invalid", Scope: "personal"})
			}
			if err := os.WriteFile(path, []byte("invalid document"), 0600); err != nil {
				t.Fatal(err)
			}
			before := retentionSnapshot(t, lookupTestEnv(t).home)
			if _, err := executeRetentionReport(context.Background(), cfg, report.ReportID, now); err == nil {
				t.Fatal("corrupt preflight accepted")
			}
			if !reflect.DeepEqual(before, retentionSnapshot(t, lookupTestEnv(t).home)) {
				t.Fatal("corruption refusal mutated files")
			}
		})
	}
}
