package mora

// mora_coreA_cover5_test.go — coreA coverage worker, part 5. Final residual
// branches reachable without a TTY or the network: loadSources failures on the
// CLI read paths, the pulse entity-filter error, applySetupSelection's enable
// failure, and usageReport's malformed-line skips.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// coreACorruptSources writes an unparseable sources.json for the current install.
func coreACorruptSources(t *testing.T, cfg Config) {
	t.Helper()
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "sources.json"), []byte("{ broken"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCoreA_CorruptSourcesCLIPaths covers the loadSources() error branch in the
// commands that enumerate sources for a read/list.
func TestCoreA_CorruptSourcesCLIPaths(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	coreACorruptSources(t, mustConfig(t))

	for _, args := range [][]string{
		{"sources", "list"},
		{"connectors", "list"},
		{"ingest", "run", "--all"},
	} {
		if _, err := runErr(t, args...); err == nil {
			t.Errorf("%v should surface the corrupt-sources load error", args)
		}
	}
}

// TestCoreA_PulseEntityFilterError covers cmdPulse's entity-resolution failure:
// an unknown entity aborts before rendering.
func TestCoreA_PulseEntityFilterError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	if _, err := runErr(t, "pulse", "--digest", "--entity", "no-such-entity-xyz"); err == nil {
		t.Fatal("pulse --entity with no match must error")
	}
}

// TestCoreA_ApplySetupSelectionEnableError covers the enableConnector failure
// path inside applySetupSelection (corrupt sources => setSourceEnabled fails).
func TestCoreA_ApplySetupSelectionEnableError(t *testing.T) {
	cfg := coreADirsCfg(t)
	coreACorruptSources(t, cfg)
	var out bytes.Buffer
	err := applySetupSelection(context.Background(), cfg, []string{"filesystem"}, false, &out, testStderr, strings.NewReader(""))
	if err == nil {
		t.Fatal("applySetupSelection must propagate an enableConnector failure")
	}
}

// TestCoreA_UsageReportSkipsMalformedLines covers usageReport's blank-line and
// unparseable-line skips: both are ignored, only valid events are counted.
func TestCoreA_UsageReportSkipsMalformedLines(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	usageDir := filepath.Join(cfg.StateDir, "usage")
	if err := os.MkdirAll(usageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.Join([]string{
		`{"tool":"search_memory","results":1,"millis":5}`,
		"",                 // blank line => skipped
		"this is not json", // unparseable => skipped
		`{"tool":"think","results":0,"millis":8}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(usageDir, "events.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out := run(t, "usage", "report")
	if !strings.Contains(out, "total calls: 2") {
		t.Fatalf("usageReport should count only the 2 valid events; got:\n%s", out)
	}
}
