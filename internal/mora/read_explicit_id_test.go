package mora

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadResolvesReturnedExplicitIDIndependentOfFilename(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := "calendar/038-ridgeline-partner-call-halcyon"
	want := Memory{
		ID: id, Scope: "project:halcyon", Type: "event", Title: "Partner call",
		Source: "fixture", CreatedAt: "2026-07-16T09:00:00Z", Text: "cited source text",
	}
	body, err := renderMemory(want)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(memoriesRoot(cfg), "calendar", "human-readable-unrelated-name.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := findMemory(cfg, id)
	if err != nil || got.ID != id {
		t.Fatalf("findMemory(%q) = id %q, err %v", id, got.ID, err)
	}
	var cli Memory
	if err := json.Unmarshal([]byte(run(t, "read", id, "--json")), &cli); err != nil {
		t.Fatal(err)
	}
	if cli.ID != id || cli.Text != want.Text {
		t.Fatalf("read returned %+v; want explicit id %q", cli, id)
	}
}
