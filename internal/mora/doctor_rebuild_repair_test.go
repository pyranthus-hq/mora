package mora

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ingestpkg "github.com/pyranthus-hq/mora/internal/ingest"
)

func TestDoctorRebuildRepairUncoveredJournal(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside vault"), 0600); err != nil {
		t.Fatal(err)
	}
	journal := ingestpkg.JournalPath(cfg, ingestpkg.SourceKey("filesystem", "uncovered"))
	if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
		t.Fatal(err)
	}
	contents := "run uncovered-run 2026-09-20T00:00:00Z\n" + outside + "\n"
	if err := os.WriteFile(journal, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		var output bytes.Buffer
		err := cmdDoctor(testCtx(t), []string{"--json", "--repair", "--yes"}, &output, io.Discard)
		var report struct {
			Checks       []doctorCheck `json:"checks"`
			Verification []struct {
				ActionID string `json:"action_id"`
				After    string `json:"after"`
				Verified bool   `json:"verified"`
				Detail   string `json:"detail"`
			} `json:"verification"`
		}
		if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
			t.Fatalf("decode: %v; command: %v; output: %s", decodeErr, err, output.String())
		}
		if !doctorCheckFailed(report.Checks, "index_fresh") {
			t.Fatal("fixture must retain failing index_fresh")
		}
		found := false
		for _, result := range report.Verification {
			if result.ActionID != "rebuild_index" {
				continue
			}
			found = true
			if result.Verified || result.After != "failed" {
				t.Fatalf("attempt %d claimed repair succeeded: %+v", attempt, result)
			}
			if !strings.Contains(result.Detail, "index_fresh") || !strings.Contains(result.Detail, "re-plan") {
				t.Fatalf("missing failure/re-planning explanation: %+v", result)
			}
		}
		if !found {
			t.Fatal("expected rebuild verification")
		}
		if err == nil {
			t.Fatal("unresolved repair must return an error")
		}
		if got := indexHealthOf(cfg, doctorClock()); got.State != idxDirty {
			t.Fatalf("index state = %s", got.State)
		}
		if data, err := os.ReadFile(journal); err != nil || !bytes.Contains(data, []byte(outside)) {
			t.Fatalf("uncovered evidence lost: %s, %v", data, err)
		}
	}
}

func TestDoctorRebuildRepairMissingIndex(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := os.Remove(dbPath(cfg)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		var output bytes.Buffer
		if err := cmdDoctor(testCtx(t), []string{"--json", "--repair", "--yes"}, &output, io.Discard); err != nil {
			t.Fatal(err)
		}
		var report doctorReport
		if err := json.Unmarshal(output.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, result := range report.Verification {
			if result.ActionID == "rebuild_index" {
				found = true
				if !result.Verified || result.After != "passed" {
					t.Fatalf("repair did not verify: %+v", result)
				}
			}
		}
		if found != (attempt == 0) {
			t.Fatalf("attempt %d: rebuild present = %v", attempt, found)
		}
	}
}

func TestDoctorRebuildRepairCoveredJournalConverges(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	memory := filepath.Join(memoriesRoot(cfg), "covered.md")
	if err := os.MkdirAll(filepath.Dir(memory), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memory, []byte("---\nid: covered\ntitle: Covered memory\ntype: note\n---\nA journaled memory.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	journal := ingestpkg.JournalPath(cfg, "filesystem:covered")
	if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("run covered-run 2026-09-20T00:00:00Z\n"+memory+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := indexHealthOf(cfg, doctorClock()); got.State != idxDirty {
		t.Fatalf("fixture index state = %s, want dirty", got.State)
	}
	for attempt := 0; attempt < 2; attempt++ {
		var output bytes.Buffer
		// A nil Run error is the CLI's exit-0 path.
		if err := Run(testCtx(t), []string{"doctor", "--json", "--repair", "--yes"}, &output, io.Discard, strings.NewReader("")); err != nil {
			t.Fatalf("attempt %d: %v; output: %s", attempt, err, output.String())
		}
		var report doctorReport
		if err := json.Unmarshal(output.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			found := false
			for _, result := range report.Verification {
				if result.ActionID == "rebuild_index" {
					found = true
					if !result.Verified || result.After != "passed" {
						t.Fatalf("repair did not verify: %+v", result)
					}
				}
			}
			if !found {
				t.Fatal("expected rebuild verification")
			}
			if _, err := os.Stat(journal); !os.IsNotExist(err) {
				t.Fatalf("covered journal was not retired: %v", err)
			}
		} else if len(report.RepairPlan) != 0 || len(report.Verification) != 0 {
			t.Fatalf("repair did not converge: plan=%+v verification=%+v", report.RepairPlan, report.Verification)
		}
	}
}

func TestDoctorRepairsContinueAfterRebuildVerificationFailure(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside vault"), 0600); err != nil {
		t.Fatal(err)
	}
	journal := ingestpkg.JournalPath(cfg, "filesystem:uncovered")
	if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("run uncovered-run 2026-09-20T00:00:00Z\n"+outside+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tokens := filepath.Join(t.TempDir(), "tokens")
	results, err := applyDoctorRepairs(testCtx(t), cfg, []doctorRepairAction{
		{ID: "rebuild_index", Target: dbPath(cfg), Safe: true},
		{ID: "create_token_dir", Target: tokens, Safe: true},
	})
	if err == nil || !strings.Contains(err.Error(), "index_fresh") {
		t.Fatalf("expected rebuild verification error, got %v", err)
	}
	if len(results) != 2 || results[0].Verified || results[0].Detail == "" || !results[1].Verified || results[1].After != "passed" {
		t.Fatalf("expected failed rebuild followed by successful safe repair: %+v", results)
	}
	if info, err := os.Stat(tokens); err != nil || !info.IsDir() {
		t.Fatalf("later safe repair did not create token directory: %v", err)
	}
}
