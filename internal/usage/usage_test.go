package usage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPercentile(t *testing.T) {
	if got := Percentile(nil, 50); got != 0 {
		t.Fatalf("empty = %d", got)
	}
	values := []int64{5, 1, 4, 2, 3}
	for p, want := range map[int]int64{0: 1, 50: 3, 100: 5} {
		if got := Percentile(values, p); got != want {
			t.Fatalf("p%d = %d, want %d", p, got, want)
		}
	}
}

func TestQueryLoggingRequiresOptIn(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	t.Setenv("MORA_LOG_QUERIES", "")
	if QueryLoggingEnabled(cfg) {
		t.Fatal("default must be off")
	}
	t.Setenv("MORA_LOG_QUERIES", "1")
	if !QueryLoggingEnabled(cfg) {
		t.Fatal("env opt-in ignored")
	}
	t.Setenv("MORA_LOG_QUERIES", "")
	marker := filepath.Join(cfg.StateDir, "usage", "QUERIES")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !QueryLoggingEnabled(cfg) {
		t.Fatal("marker opt-in ignored")
	}
}

func TestLogStripsQueryAndHonorsDoNotTrack(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	t.Setenv("DO_NOT_TRACK", "")
	Log(cfg, Event{Tool: "search_memory", Query: "secret", Results: 1})
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, "usage", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatal("query leaked without opt-in")
	}
	t.Setenv("DO_NOT_TRACK", "1")
	before := string(raw)
	Log(cfg, Event{Tool: "search_memory", Results: 2})
	after, _ := os.ReadFile(filepath.Join(cfg.StateDir, "usage", "events.jsonl"))
	if string(after) != before {
		t.Fatal("DO_NOT_TRACK appended an event")
	}
}

func TestCoreA_Percentile(t *testing.T) {
	if got := Percentile(nil, 50); got != 0 {
		t.Errorf("Percentile(empty) = %d, want 0", got)
	}
	if got := Percentile([]int64{5}, 50); got != 5 {
		t.Errorf("Percentile([5],50) = %d, want 5", got)
	}
	v := []int64{5, 1, 4, 2, 3} // unsorted on purpose
	if got := Percentile(v, 50); got != 3 {
		t.Errorf("Percentile(p50) = %d, want 3", got)
	}
	if got := Percentile(v, 0); got != 1 {
		t.Errorf("Percentile(p0) = %d, want 1", got)
	}
	if got := Percentile(v, 100); got != 5 {
		t.Errorf("Percentile(p100) = %d, want 5", got)
	}
}

func TestCoreB_UtilQueryLoggingEnabled(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	t.Setenv("MORA_LOG_QUERIES", "")

	if QueryLoggingEnabled(cfg) {
		t.Fatal("query logging must default OFF")
	}

	t.Setenv("MORA_LOG_QUERIES", "1")
	if !QueryLoggingEnabled(cfg) {
		t.Fatal("MORA_LOG_QUERIES=1 must enable query logging")
	}
	t.Setenv("MORA_LOG_QUERIES", "")

	// Marker file also enables it.
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "usage"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "usage", "QUERIES"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !QueryLoggingEnabled(cfg) {
		t.Fatal("usage/QUERIES marker must enable query logging")
	}
}
