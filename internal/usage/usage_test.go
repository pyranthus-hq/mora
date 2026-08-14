package usage

import (
	"bytes"
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

func TestPercentileUsesNearestRankForTail(t *testing.T) {
	values := []int64{1, 2, 3, 4}
	if got := Percentile(values, 95); got != 4 {
		t.Fatalf("p95 = %d, want tail value 4", got)
	}
	if got := Percentile(values, -1); got != 1 {
		t.Fatalf("negative percentile = %d, want minimum", got)
	}
	if got := Percentile(values, 101); got != 4 {
		t.Fatalf("over-100 percentile = %d, want maximum", got)
	}
}

func TestReportPerToolScorecardIsStableAndHonestAboutLegacyCoverage(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	dir := filepath.Join(cfg.StateDir, "usage")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The first row models an older content-free event without output_bytes.
	// The query is intentionally present to prove reporting never re-emits it.
	body := strings.Join([]string{
		`{"tool":"write_memory","query":"WRITE_SECRET","results":1,"millis":80,"output_bytes":40}`,
		`{"tool":"search_memory","query":"SEARCH_SECRET","results":0,"millis":100}`,
		`{"tool":"search_memory","query":"SECOND_SECRET","results":2,"millis":10,"output_bytes":400}`,
		`{"tool":"read_memory","results":1,"millis":50,"output_bytes":100}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Report(cfg, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, secret := range []string{"WRITE_SECRET", "SEARCH_SECRET", "SECOND_SECRET"} {
		if strings.Contains(got, secret) {
			t.Fatalf("report leaked content %q: %s", secret, got)
		}
	}
	for _, want := range []string{
		"total calls: 4\n",
		"empty-result rate: 25%\n",
		"latency p50: 50ms\n",
		"per-tool scorecard:\n",
		"  read_memory: calls=1 empty=0% latency_p50/p95=50ms/50ms output_tokens_p50/p95=25/25 tok (1/1 events)\n",
		"  search_memory: calls=2 empty=50% latency_p50/p95=10ms/100ms output_tokens_p50/p95=100/100 tok (1/2 events)\n",
		"  write_memory: calls=1 empty=n/a latency_p50/p95=80ms/80ms output_tokens_p50/p95=10/10 tok (1/1 events)\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report missing %q:\n%s", want, got)
		}
	}
	if read, search, write := strings.Index(got, "read_memory:"), strings.Index(got, "search_memory:"), strings.Index(got, "write_memory:"); !(read < search && search < write) {
		t.Fatalf("scorecard is not tool-sorted:\n%s", got)
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
