package mora

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hkBrokenDataCfg returns a Config whose DataDir sits UNDER a regular file, so
// any attempt to build/open the index (MkdirAll of the data dir) fails
// deterministically without relying on chmod. The vault dir is real so the
// rebuild reads a valid corpus and only the write side fails.
func hkBrokenDataCfg(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	vault := filepath.Join(root, "vault")
	if err := os.MkdirAll(filepath.Join(vault, "memories"), 0o700); err != nil {
		t.Fatal(err)
	}
	return Config{
		VaultDir:  vault,
		DataDir:   filepath.Join(blocker, "data"), // parent is a file -> MkdirAll fails
		StateDir:  filepath.Join(root, "state"),
		ConfigDir: filepath.Join(root, "config"),
	}
}

// hkSeedEntities builds a small real vault (under the temp HOME) with entity-rich
// memories and returns its Config. graphListEntities materializes on first read.
func hkSeedEntities(t *testing.T) {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "project:atlas", "--type", "decision",
		"--title", "Kickoff with [[Priya]]", "--text", "Talked to [[Priya]] about scope.\n- [Decision] adopt MCP")
	run(t, "write", "--scope", "project:atlas", "--type", "note",
		"--title", "Follow-up", "--text", "[[Priya]] confirmed the plan.")
	// `mora write` now reflects only the memory + FTS row into the index (O(1)
	// indexUpsert), not the whole-corpus entity graph — that reconciles on the next
	// FULL rebuild. Materialize the graph explicitly so these entity-behavior tests
	// exercise the graph rather than the write path's freshness.
	run(t, "index", "rebuild")
}

// ---------------------------------------------------------------------------
// connectors.go — ingestingConnectors error + skip branches
// ---------------------------------------------------------------------------

// TestHk_IngestingConnectorsLoadSourcesError asserts a malformed sources.json
// surfaces as an error rather than a silently-empty enumeration set.
func TestHk_IngestingConnectorsLoadSourcesError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "sources.json"), []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ingestingConnectors(cfg); err == nil {
		t.Fatal("ingestingConnectors must surface a malformed sources.json error")
	}
}

// TestHk_IngestingConnectorsSkipsUnknownEnabled asserts an ENABLED source whose
// type is not an ingesting catalog entry is skipped (the !ok/!Ingesting arm),
// so it never enters the three-state enumeration set.
func TestHk_IngestingConnectorsSkipsUnknownEnabled(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	src := []Source{
		{Name: "gmail", Type: "gmail", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
		{Name: "mystery", Type: "mystery-connector", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
	}
	if err := saveSources(cfg, src); err != nil {
		t.Fatal(err)
	}
	got, err := ingestingConnectors(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "gmail" {
		t.Fatalf("enabled-but-unknown connector must be excluded, got %v", got)
	}
}

// TestHk_IngestingConnectorsDedupesInstanceKey asserts two enabled sources that
// resolve to the SAME instance key are collapsed to a single entry (the seen[key]
// skip), so the enumeration set never double-counts one connector instance.
func TestHk_IngestingConnectorsDedupesInstanceKey(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	src := []Source{
		{Name: "gmail-a", Type: "gmail", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
		{Name: "gmail-b", Type: "gmail", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: "2026-01-02T00:00:00Z"},
	}
	if err := saveSources(cfg, src); err != nil {
		t.Fatal(err)
	}
	got, err := ingestingConnectors(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "gmail" {
		t.Fatalf("duplicate instance keys must collapse to one, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// entities.go — cmdEntities
// ---------------------------------------------------------------------------

// TestHk_CmdEntitiesListText asserts the default (no-arg) view prints the grouped
// entity overview with People/Scopes/Links sections and the discovery tip.
func TestHk_CmdEntitiesListText(t *testing.T) {
	hkSeedEntities(t)
	var out bytes.Buffer
	if err := cmdEntities(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "project:atlas") || !strings.Contains(s, "Priya") {
		t.Fatalf("entity overview missing seeded entities:\n%s", s)
	}
	if !strings.Contains(s, "mora entities") {
		t.Fatalf("entity overview missing the discovery tip:\n%s", s)
	}
}

// TestHk_CmdEntitiesListJSON asserts --json emits the full entity list as JSON.
func TestHk_CmdEntitiesListJSON(t *testing.T) {
	hkSeedEntities(t)
	var out bytes.Buffer
	if err := cmdEntities(context.Background(), []string{"--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var ents []Entity
	if err := json.Unmarshal(out.Bytes(), &ents); err != nil {
		t.Fatalf("--json output is not a JSON entity list: %v\n%s", err, out.String())
	}
	found := false
	for _, e := range ents {
		if e.Kind == "link" && e.Name == "Priya" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a [[Priya]] link entity in JSON, got %+v", ents)
	}
}

// TestHk_CmdEntitiesEmptyVault asserts the empty-graph guidance message.
func TestHk_CmdEntitiesEmptyVault(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	var out bytes.Buffer
	if err := cmdEntities(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No entities found yet") {
		t.Fatalf("empty vault must print the ingest hint, got:\n%s", out.String())
	}
}

// TestHk_CmdEntitiesDetailText asserts the name-filtered detail view lists the
// referencing memories for a matched entity.
func TestHk_CmdEntitiesDetailText(t *testing.T) {
	hkSeedEntities(t)
	var out bytes.Buffer
	if err := cmdEntities(context.Background(), []string{"Priya"}, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "Priya (link)") {
		t.Fatalf("detail view header missing, got:\n%s", s)
	}
	if !strings.Contains(s, "memories") {
		t.Fatalf("detail view should report a memory count, got:\n%s", s)
	}
}

// TestHk_CmdEntitiesDetailJSON asserts the JSON detail view carries found=true and
// the matched entity's memories.
func TestHk_CmdEntitiesDetailJSON(t *testing.T) {
	hkSeedEntities(t)
	var out bytes.Buffer
	if err := cmdEntities(context.Background(), []string{"Priya", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("detail --json invalid: %v\n%s", err, out.String())
	}
	if res["found"] != true {
		t.Fatalf("detail JSON found flag wrong: %v", res["found"])
	}
	if res["name"] != "Priya" {
		t.Fatalf("detail JSON name = %v, want Priya", res["name"])
	}
}

// TestHk_CmdEntitiesLoadConfigError asserts an unreadable config surfaces as an
// error from the entities command.
func TestHk_CmdEntitiesLoadConfigError(t *testing.T) {
	hkBreakLoadConfig(t)
	var out bytes.Buffer
	if err := cmdEntities(context.Background(), nil, &out); err == nil {
		t.Fatal("cmdEntities must surface an unreadable config error")
	}
}

// TestHk_CmdEntitiesGraphError asserts a broken index location surfaces the
// graphListEntities error instead of an empty overview.
func TestHk_CmdEntitiesGraphError(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A config.toml that points data_dir under a file makes the graph build fail.
	cfgDir := filepath.Join(root, "cfg")
	vault := filepath.Join(root, "vault")
	if err := os.MkdirAll(filepath.Join(vault, "memories"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	toml := "vault_dir = \"" + vault + "\"\ndata_dir = \"" + filepath.Join(blocker, "data") + "\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORA_CONFIG_DIR", cfgDir)
	var out bytes.Buffer
	if err := cmdEntities(context.Background(), nil, &out); err == nil {
		t.Fatal("cmdEntities must surface a graph-build error when the index cannot be written")
	}
}

// ---------------------------------------------------------------------------
// entities.go — entityDetailGraph direct
// ---------------------------------------------------------------------------

// TestHk_EntityDetailGraphNoMatchText asserts the not-found text message.
func TestHk_EntityDetailGraphNoMatchText(t *testing.T) {
	var out bytes.Buffer
	err := entityDetailGraph(context.Background(), Config{}, &out,
		[]Entity{{Name: "Priya", Kind: "link"}}, "Nonexistent", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `No entity named "Nonexistent"`) {
		t.Fatalf("no-match text wrong, got:\n%s", out.String())
	}
}

// TestHk_EntityDetailGraphNoMatchJSON asserts the not-found JSON payload carries
// found=false and an empty memories array.
func TestHk_EntityDetailGraphNoMatchJSON(t *testing.T) {
	var out bytes.Buffer
	err := entityDetailGraph(context.Background(), Config{}, &out,
		[]Entity{{Name: "Priya", Kind: "link"}}, "Ghost", true)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("no-match --json invalid: %v\n%s", err, out.String())
	}
	if res["found"] != false || res["name"] != "Ghost" {
		t.Fatalf("no-match JSON wrong: %+v", res)
	}
}

// TestHk_EntityDetailGraphIndexError asserts a broken index location surfaces the
// ensureIndexDB error on the matched path (past the name-match, into the DB open).
func TestHk_EntityDetailGraphIndexError(t *testing.T) {
	cfg := hkBrokenDataCfg(t)
	var out bytes.Buffer
	err := entityDetailGraph(context.Background(), cfg, &out,
		[]Entity{{Name: "Priya", Kind: "link", MemoryIDs: []string{"m1"}}}, "Priya", false)
	if err == nil {
		t.Fatal("entityDetailGraph must surface an index-open error for a matched entity")
	}
}

// TestHk_EntityDetailGraphLoadMemoriesError asserts a matched entity whose
// backing `memories` table has gone missing surfaces the load error rather than
// printing a half-rendered detail view. The graph tables (entities/edges) remain
// intact so ensureIndexDB succeeds and the failure is isolated to the memory read.
func TestHk_EntityDetailGraphLoadMemoriesError(t *testing.T) {
	hkSeedEntities(t)
	cfg := mustConfig(t)
	// Materialize the graph and grab a real matched entity.
	ents, err := graphListEntities(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) == 0 {
		t.Fatal("expected seeded entities")
	}
	// Drop the memories table (leaving entities/edges + user_version intact) so
	// loadMemoriesByID's query fails while the schema check still passes.
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE memories`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := entityDetailGraph(context.Background(), cfg, &out, ents, ents[0].Name, false); err == nil {
		t.Fatal("a missing memories table must surface a load error")
	}
}

// ---------------------------------------------------------------------------
// entities.go — entitiesForMCP / entityMemoriesForMCP
// ---------------------------------------------------------------------------

// TestHk_EntitiesForMCPKindFilter asserts the kind filter drops non-matching
// entities and that memory_ids are stripped for the agent surface.
func TestHk_EntitiesForMCPKindFilter(t *testing.T) {
	hkSeedEntities(t)
	cfg := mustConfig(t)
	ents, err := entitiesForMCP(context.Background(), cfg, "scope", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) == 0 {
		t.Fatal("expected at least one scope entity")
	}
	for _, e := range ents {
		if e.Kind != "scope" {
			t.Fatalf("kind filter leaked a %q entity: %+v", e.Kind, e)
		}
		if e.MemoryIDs != nil {
			t.Fatalf("entitiesForMCP must strip memory_ids, got %+v", e.MemoryIDs)
		}
	}
}

// TestHk_EntitiesForMCPGraphError asserts a broken index location surfaces the
// error rather than an empty list.
func TestHk_EntitiesForMCPGraphError(t *testing.T) {
	cfg := hkBrokenDataCfg(t)
	if _, err := entitiesForMCP(context.Background(), cfg, "", 0); err == nil {
		t.Fatal("entitiesForMCP must surface a graph-build error")
	}
}

// TestHk_EntityMemoriesForMCPGraphError asserts a broken index location surfaces
// the graphGetEntity error.
func TestHk_EntityMemoriesForMCPGraphError(t *testing.T) {
	cfg := hkBrokenDataCfg(t)
	if _, err := entityMemoriesForMCP(context.Background(), cfg, "Priya"); err == nil {
		t.Fatal("entityMemoriesForMCP must surface a graph-build error")
	}
}
