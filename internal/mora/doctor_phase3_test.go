package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestDoctorAmbiguousSQLiteFailureIsCauseUnverified(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	source := Source{Name: "applecalendar", Type: "applecalendar", Enabled: genericutil.Ptr(true)}
	if err := saveSources(cfg, []Source{source}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	setDoctorClock(t, now)
	if err := memory.SaveStatus(syncStatusPathFor(cfg, source), &memory.SyncStatus{
		Source: source.Name, LastAttemptAt: now.Format(time.RFC3339),
		LastError: "unable to open database file: out of memory (14)", ErrorCode: errCodeConnectorUnclassified, ErrorCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := cmdDoctor(context.Background(), []string{"--json"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var report doctorReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, diagnosis := range report.Diagnosis {
		if diagnosis.Subject == "source:applecalendar" {
			found = diagnosis.Code == "cause_unverified" && diagnosis.ErrorCode == errCodeConnectorUnclassified
		}
		if diagnosis.Code == "permission_missing" {
			t.Fatalf("ambiguous failure claimed permission state: %+v", diagnosis)
		}
	}
	if !found || strings.Contains(strings.ToLower(output.String()), "full disk access") {
		t.Fatalf("report did not preserve uncertainty: %s", output.String())
	}
	if report.Observed == nil || report.Diagnosis == nil || report.RepairPlan == nil || report.Verification == nil {
		t.Fatalf("doctor additive fields must be non-null: %+v", report)
	}
}
