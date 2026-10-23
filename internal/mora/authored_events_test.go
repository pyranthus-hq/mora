package mora

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/activity"
)

func TestAuthoredEventCLIAndMCPOptIn(t *testing.T) {
	cfg := seedRecencyVault(t)
	run(t, "write", "--title", "authored probe", "--text", "synthetic probe")
	// Compare flag-off receipts for the same window, including its precise bounds.
	now := time.Now()
	oldClock := briefClock
	briefClock = func() time.Time { return now }
	t.Cleanup(func() { briefClock = oldClock })
	decode := func(raw string) map[string]any {
		t.Helper()
		var out map[string]any
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	off := run(t, "list", "--event-since-hours", "24", "--json")
	explicitOff := run(t, "list", "--event-since-hours", "24", "--include-authored-writes=false", "--json")
	if off != explicitOff {
		t.Fatal("false changed bytes")
	}
	if rows := decode(off)["memories"]; rows != nil && len(rows.([]any)) != 0 {
		t.Fatal("default admitted authored record")
	}
	on := decode(run(t, "list", "--event-since-hours", "24", "--include-authored-writes", "--limit", "1", "--json"))
	rows := on["memories"].([]any)
	if len(rows) != 1 || on["include_authored_writes"] != true {
		t.Fatalf("opt-in=%+v", on)
	}
	row := rows[0].(map[string]any)
	if row["event_source"] != "authored_write" || !sameAuthoredInstant(t, row["event_at"].(string), row["created_at"].(string)) {
		t.Fatalf("projection=%+v", row)
	}
	for _, key := range []string{"event_since_hours", "since_hours"} {
		result, err := mcpListMemory(testCtx(t), cfg, map[string]any{key: 24, "include_authored_writes": true})
		if err != nil {
			t.Fatal(err)
		}
		out := result.(map[string]any)
		rr := out["memories"].([]Memory)
		if len(rr) != 1 || rr[0].EventSource != "authored_write" || !sameAuthoredInstant(t, rr[0].EventAt, rr[0].CreatedAt) || out["include_authored_writes"] != true {
			t.Fatalf("MCP lost projection: %+v", out)
		}
	}
	for _, args := range []map[string]any{{"include_authored_writes": true}, {"event_since_hours": 24, "include_authored_writes": "true"}, {"event_since_hours": 24, "include_authored_writes": nil}} {
		if _, err := mcpListMemory(testCtx(t), cfg, args); err == nil {
			t.Fatalf("accepted %+v", args)
		}
	}
	if err := Run(testCtx(t), []string{"list", "--include-authored-writes"}, io.Discard, io.Discard, strings.NewReader("")); err == nil {
		t.Fatal("accepted opt-in without event window")
	}
}

func TestAuthoredMirrorResyncCanReenterWindow(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	if err := os.WriteFile(path, []byte("synthetic unchanged note"), 0600); err != nil {
		t.Fatal(err)
	}
	source := Source{Name: "codex-memory", Type: "filesystem", Path: root, Scope: "personal"}
	ingest := func() {
		t.Helper()
		if _, err := ingestFilesystemDetailed(testCtx(t), cfg, source, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	ingest()
	id := "src_" + ContentHash(source.Name+":note.md")
	dest := filepath.Join(sourcesRoot(cfg), source.Type, source.Name, id+".md")
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseMemoryBytes(dest, raw)
	if err != nil {
		t.Fatal(err)
	}
	// Model an existing, old imported authored record without waiting a day.
	m.CreatedAt = time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	body, err := renderMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	opts := activity.Options{IncludeAuthoredWrites: true}
	if rows := recentSourceEvents([]Memory{m}, time.Now(), 24, 10, opts); len(rows) != 0 {
		t.Fatal("old mirror in window")
	}
	// Force the same unchanged source through the real resync writer by changing mtime.
	if err := os.Chtimes(path, time.Now().Add(time.Minute), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ingest()
	raw, err = os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	after, err := parseMemoryBytes(dest, raw)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != m.ID || after.Text != m.Text || after.CreatedAt == m.CreatedAt {
		t.Fatalf("resync did not demonstrate re-dating: %+v", after)
	}
	if rows := recentSourceEvents([]Memory{after}, time.Now(), 24, 10, opts); len(rows) != 1 || rows[0].EventSource != "authored_write" {
		t.Fatalf("expected documented flood risk: %+v", rows)
	}
	if rows := recentSourceEvents([]Memory{after}, time.Now(), 24, 10); len(rows) != 0 {
		t.Fatal("default flood guard changed")
	}
}

func sameAuthoredInstant(t *testing.T, a, b string) bool {
	t.Helper()
	left, err := time.Parse(time.RFC3339Nano, a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := time.Parse(time.RFC3339Nano, b)
	if err != nil {
		t.Fatal(err)
	}
	return left.Equal(right)
}
