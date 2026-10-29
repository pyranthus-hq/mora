package mora

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRetentionIntegrityUsesCurrentTeachProjection(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	var original writeReceipt
	if err := json.Unmarshal([]byte(run(t, "write", "--scope", "project:integrity", "--type", "note", "--title", "Original", "--text", "Use npm", "--json")), &original); err != nil {
		t.Fatal(err)
	}
	var corrected struct {
		Decision    govEntry `json:"decision"`
		Replacement Memory   `json:"replacement"`
	}
	if err := json.Unmarshal([]byte(run(t, "teach", "memory", "correct", "--id", original.ID, "--title", "Correction", "--text", "Use pnpm", "--yes", "--json")), &corrected); err != nil {
		t.Fatal(err)
	}
	assertIntegrity := func(want, historical int) {
		t.Helper()
		got, err := verifyRetentionIntegrity(context.Background(), cfg)
		if err != nil || !got.Healthy || got.VaultCount != want || got.IndexCount != want || got.HistoricalCount != historical {
			t.Fatalf("normal Teach state flagged corrupt: %+v %v", got, err)
		}
	}
	assertIntegrity(1, 1)
	run(t, "teach", "memory", "retract", "--id", corrected.Replacement.ID, "--yes")
	assertIntegrity(0, 2)
	history := run(t, "teach", "history", "--memory-id", original.ID, "--json")
	var log teachHistoryReceipt
	if err := json.Unmarshal([]byte(history), &log); err != nil {
		t.Fatal(err)
	}
	for _, e := range log.Entries {
		if e.Decision == teachMemoryRetract {
			run(t, "teach", "undo", e.ID)
		}
	}
	assertIntegrity(1, 1)
	run(t, "teach", "undo", corrected.Decision.ID)
	assertIntegrity(1, 1)
}

func TestRetentionIntegrityStillDetectsUnreadableHistory(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	var original writeReceipt
	if err := json.Unmarshal([]byte(run(t, "write", "--scope", "project:integrity", "--type", "note", "--title", "Original", "--text", "Use npm", "--json")), &original); err != nil {
		t.Fatal(err)
	}
	run(t, "teach", "memory", "correct", "--id", original.ID, "--title", "Correction", "--text", "Use pnpm", "--yes")
	m, err := findMemoryRaw(cfg, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Path, []byte("not a memory document"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := verifyRetentionIntegrity(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Healthy || len(got.InvalidMemoryPaths) != 1 || len(got.MissingIndexIDs) != 0 {
		t.Fatalf("history damage hidden or mislabelled as missing index: %+v", got)
	}
	if strings.Contains(got.InvalidMemoryPaths[0], cfg.VaultDir) {
		t.Fatalf("expected relative path: %+v", got)
	}
}
