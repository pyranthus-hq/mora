package mora

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

var (
	grDriverOnce sync.Once
	grScriptsMu  sync.Mutex
	grScripts    = map[string]*grSQLScript{}
)

type grSQLScript struct {
	queries []grSQLQuery
}

type grSQLQuery struct {
	cols     []string
	rows     [][]driver.Value
	queryErr error
	rowsErr  error
}

type grDriver struct{}

type grConn struct {
	script *grSQLScript
	next   int
}

type grRows struct {
	cols []string
	rows [][]driver.Value
	err  error
	i    int
}

func (grDriver) Open(name string) (driver.Conn, error) {
	grScriptsMu.Lock()
	script := grScripts[name]
	grScriptsMu.Unlock()
	if script == nil {
		return nil, fmt.Errorf("missing gr sql script %q", name)
	}
	return &grConn{script: script}, nil
}

func (c *grConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("prepare unused") }
func (c *grConn) Close() error                        { return nil }
func (c *grConn) Begin() (driver.Tx, error)           { return nil, errors.New("tx unused") }

func (c *grConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	if c.next >= len(c.script.queries) {
		return nil, fmt.Errorf("unexpected query %d", c.next)
	}
	q := c.script.queries[c.next]
	c.next++
	if q.queryErr != nil {
		return nil, q.queryErr
	}
	return &grRows{cols: q.cols, rows: q.rows, err: q.rowsErr}, nil
}

func (r *grRows) Columns() []string { return r.cols }
func (r *grRows) Close() error      { return nil }

func (r *grRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		if r.err != nil {
			err := r.err
			r.err = nil
			return err
		}
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

func grOpenScriptDB(t *testing.T, queries ...grSQLQuery) *sql.DB {
	t.Helper()
	grDriverOnce.Do(func() { sql.Register("mora_gr_fake", grDriver{}) })
	name := strings.ReplaceAll(t.Name(), "/", "_")
	grScriptsMu.Lock()
	grScripts[name] = &grSQLScript{queries: queries}
	grScriptsMu.Unlock()
	t.Cleanup(func() {
		grScriptsMu.Lock()
		delete(grScripts, name)
		grScriptsMu.Unlock()
	})
	db, err := sql.Open("mora_gr_fake", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func grWriteMemory(t *testing.T, cfg Config, m Memory) {
	t.Helper()
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
}

func grTempConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		VaultDir:  filepath.Join(root, "vault"),
		ConfigDir: filepath.Join(root, "config"),
		DataDir:   filepath.Join(root, "data"),
		StateDir:  filepath.Join(root, "state"),
	}
}

func grOpenSQLiteIndex(t *testing.T, cfg Config) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath(cfg)), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	grExec(t, db, fmt.Sprintf(`PRAGMA user_version = %d`, indexSchemaVersion))
	return db
}

func grExec(t *testing.T, db *sql.DB, stmt string, args ...any) {
	t.Helper()
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

func grSeedGraphVault(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	grWriteMemory(t, cfg, Memory{
		ID:        "mail-1",
		Scope:     "project:gr",
		Type:      "email",
		Title:     "Launch thread with [[Roadmap]]",
		Tags:      []string{"planning"},
		Text:      "Alice and Bob discussed [[Roadmap]].\n- [Decision] ship graph view",
		CreatedAt: "2026-06-01T09:00:00Z",
		Meta: map[string]any{
			"from": []string{"alice@example.com"},
			"to":   []string{"bob@example.com"},
			"names": map[string]any{
				"alice@example.com": "Alice Example",
				"bob@example.com":   "Bob Builder",
			},
			"occurred_at": "2026-06-01T08:00:00Z",
		},
	})
	grWriteMemory(t, cfg, Memory{
		ID:        "mail-2",
		Scope:     "project:gr",
		Type:      "email",
		Title:     "Follow up",
		Text:      "Bob replied to Alice Example about the roadmap.",
		CreatedAt: "2026-06-02T09:00:00Z",
		Meta: map[string]any{
			"from": []string{"bob@example.com"},
			"to":   []string{"alice@example.com"},
			"names": map[string]string{
				"bob@example.com":   "Bob Builder",
				"alice@example.com": "Alice Example",
			},
		},
	})
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestGr_CmdGraphOverviewJSONAndDetail(t *testing.T) {
	grSeedGraphVault(t)
	ctx := context.Background()

	var overview bytes.Buffer
	if err := cmdGraph(ctx, []string{"--top", "1"}, &overview); err != nil {
		t.Fatal(err)
	}
	out := overview.String()
	for _, want := range []string{"Entity graph", "People (top 1 of 2)", "Topics & links", "Expand one:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("overview missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Bob Builder") && strings.Contains(out, "Alice Example") {
		t.Fatalf("--top 1 should render only one person row:\n%s", out)
	}

	var listJSON bytes.Buffer
	if err := cmdGraph(ctx, []string{"--json", "--top=2"}, &listJSON); err != nil {
		t.Fatal(err)
	}
	if got := listJSON.String(); !strings.Contains(got, `"name": "Alice Example"`) || !strings.Contains(got, `"kind": "person"`) {
		t.Fatalf("graph --json should emit graph entities, got:\n%s", got)
	}

	var detail bytes.Buffer
	if err := cmdGraph(ctx, []string{"Alice", "Example"}, &detail); err != nil {
		t.Fatal(err)
	}
	detailOut := detail.String()
	for _, want := range []string{"Alice Example", "Connected people", "Relationships", "Evidence", "[mail-1]"} {
		if !strings.Contains(detailOut, want) {
			t.Fatalf("detail missing %q:\n%s", want, detailOut)
		}
	}

	var detailJSON bytes.Buffer
	if err := cmdGraph(ctx, []string{"--json", "alice@example.com"}, &detailJSON); err != nil {
		t.Fatal(err)
	}
	if got := detailJSON.String(); !strings.Contains(got, `"found": true`) || !strings.Contains(got, `"aliases"`) {
		t.Fatalf("graph detail --json should emit entity record, got:\n%s", got)
	}

	var missing bytes.Buffer
	if err := cmdGraph(ctx, []string{"Nobody"}, &missing); err != nil {
		t.Fatal(err)
	}
	if got := missing.String(); !strings.Contains(got, `No entity named "Nobody"`) {
		t.Fatalf("missing entity message = %q", got)
	}
}

func TestGr_CmdGraphEmptyAndConfigError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	var empty bytes.Buffer
	if err := cmdGraph(context.Background(), []string{"--top", "bad", "--top"}, &empty); err != nil {
		t.Fatal(err)
	}
	if got := empty.String(); !strings.Contains(got, "No entity graph yet") {
		t.Fatalf("empty graph message = %q", got)
	}

	blockingFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blockingFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORA_CONFIG_DIR", blockingFile)
	var out bytes.Buffer
	if err := cmdGraph(context.Background(), nil, &out); err == nil {
		t.Fatal("cmdGraph should return loadConfig error when MORA_CONFIG_DIR is a file")
	}

	cfgRoot := t.TempDir()
	badVault := filepath.Join(cfgRoot, "vault-file")
	if err := os.WriteFile(badVault, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgRoot, "config.toml"), []byte(fmt.Sprintf("vault_dir = %q\n", badVault)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORA_CONFIG_DIR", cfgRoot)
	out.Reset()
	if err := cmdGraph(context.Background(), nil, &out); err == nil {
		t.Fatal("cmdGraph should return graphListEntities error for an invalid configured vault")
	}
}

func TestGr_GraphRenderHelpers(t *testing.T) {
	if got := graphBar(1, 100, 10); got != "█" {
		t.Fatalf("small positive bar = %q, want one cell", got)
	}
	if got := graphBar(0, 10, 10); got != "" {
		t.Fatalf("zero count bar = %q, want empty", got)
	}
	if got := graphBar(5, 0, 10); got != "" {
		t.Fatalf("zero max bar = %q, want empty", got)
	}
	if got := graphBar(5, 10, 0); got != "" {
		t.Fatalf("zero width bar = %q, want empty", got)
	}
	if got := graphTrunc("abcdef", 1); got != "a" {
		t.Fatalf("n=1 trunc = %q, want a", got)
	}
	if got := graphTrunc("abc", 3); got != "abc" {
		t.Fatalf("short trunc = %q, want original", got)
	}
	if got := graphTrunc("abcdef", 4); got != "abc…" {
		t.Fatalf("ellipsis trunc = %q", got)
	}

	var buf bytes.Buffer
	renderGraphOverview(&buf, []Entity{
		{Name: "service bot", Kind: "service", Count: 99, Salience: 99},
		{Name: "topic-a", Kind: "tag", Count: 2},
		{Name: "topic-b", Kind: "tag", Count: 5},
	}, 10)
	got := buf.String()
	if strings.Contains(got, "service bot") {
		t.Fatalf("service identities should be hidden from overview:\n%s", got)
	}
	if !strings.Contains(got, "topic-b") || strings.Index(got, "topic-b") > strings.Index(got, "topic-a") {
		t.Fatalf("non-person sections should sort by count desc:\n%s", got)
	}

	buf.Reset()
	renderPersonSection(&buf, "People", []Entity{
		{Name: "Same", Kind: "person", Count: 1, Salience: 10, MemoryIDs: []string{"b"}},
		{Name: "Same", Kind: "person", Count: 1, Salience: 10, MemoryIDs: []string{"a"}},
	}, 5)
	got = buf.String()
	if strings.Index(got, "Same") < 0 || strings.Count(got, "Same") != 2 {
		t.Fatalf("person tie render missing rows:\n%s", got)
	}
}

func TestGr_PureGraphHelperEdges(t *testing.T) {
	if got := metaStrings([]any{"a", 7, "", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("metaStrings []any = %#v", got)
	}
	if got := metaStrings("solo"); !reflect.DeepEqual(got, []string{"solo"}) {
		t.Fatalf("metaStrings string = %#v", got)
	}
	if got := metaStrings(42); got != nil {
		t.Fatalf("metaStrings unknown = %#v, want nil", got)
	}

	pairs := metaPairs([]any{map[string]any{"handle": "+1555", "name": "Texter"}, map[string]string{"handle": "h2", "name": "Name2"}})
	if len(pairs) != 2 || pairs[0].handle != "+1555" || pairs[1].name != "Name2" {
		t.Fatalf("metaPairs mixed maps = %#v", pairs)
	}
	pairs = metaPairs([]map[string]string{{"handle": "h3", "name": "Name3"}})
	if len(pairs) != 1 || pairs[0].handle != "h3" {
		t.Fatalf("metaPairs typed maps = %#v", pairs)
	}

	parts, senders, recipients, rel := personRefs(Memory{
		Type: "event",
		Meta: map[string]any{
			"organizer":    "host@example.com",
			"attendees":    []any{"guest@example.com", ""},
			"participants": []map[string]string{{"handle": "+15551212", "name": "Phone Friend"}},
			"names":        map[string]any{"host@example.com": "Host Human", "guest@example.com": "Guest Human"},
		},
	})
	if rel != "ATTENDED" || !reflect.DeepEqual(senders, []string{"person:host@example.com"}) || len(recipients) != 0 {
		t.Fatalf("event refs rel/senders/recipients = %s %#v %#v", rel, senders, recipients)
	}
	if len(parts) != 3 || parts[0].id != "person:+15551212" || parts[2].name != "Host Human" {
		t.Fatalf("event participants sorted/resolved = %#v", parts)
	}

	capped := capParticipants([]personRef{
		{id: "person:a"}, {id: "person:b"}, {id: "person:c"},
	}, map[string]bool{"person:a": true, "person:b": true, "person:c": true}, 2)
	if !reflect.DeepEqual(capped, []personRef{{id: "person:a"}, {id: "person:b"}}) {
		t.Fatalf("pathological sender cap = %#v", capped)
	}

	uf := newUnionFind([]string{"a", "b"})
	uf.union("a", "b")
	uf.union("a", "b")
	if uf.find("a") != uf.find("b") {
		t.Fatalf("union should leave a and b in the same set: %#v", uf.parent)
	}

	if got := legacyKindFromID("plain"); got != "plain" {
		t.Fatalf("legacyKindFromID(no colon) = %q", got)
	}

	parts, senders, _, _ = personRefs(Memory{
		Type: "email",
		Meta: map[string]any{
			"from":         []any{"dup@example.com", ""},
			"participants": []map[string]string{{"handle": "", "name": "Ignored"}, {"handle": "dup@example.com", "name": "Duplicate Name"}},
		},
	})
	if !reflect.DeepEqual(senders, []string{"person:dup@example.com"}) || len(parts) != 1 || parts[0].name != "Duplicate Name" {
		t.Fatalf("duplicate participant should backfill missing name: parts=%#v senders=%#v", parts, senders)
	}
}

func TestGr_BuildGraphGazetteerMentionsAndRewriteEdges(t *testing.T) {
	entities, edges, warnings := buildGraph([]Memory{
		{
			ID:        "m0",
			Type:      "note",
			Title:     "Older mention",
			Text:      "Alice Example was mentioned before metadata arrived.",
			CreatedAt: "2025-12-31T00:00:00Z",
		},
		{
			ID:        "m1",
			Type:      "email",
			Title:     "Intro",
			Text:      "Alice introduced herself.",
			CreatedAt: "2026-01-01T00:00:00Z",
			Meta: map[string]any{
				"from":  []string{"alice@example.com"},
				"names": map[string]string{"alice@example.com": "Alice Example"},
			},
		},
		{
			ID:        "m2",
			Type:      "note",
			Title:     "Mention",
			Text:      "Follow up with Alice Example next week.",
			CreatedAt: "2026-01-02T00:00:00Z",
		},
	})
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
	var alice graphEntity
	for _, e := range entities {
		if e.ID == "person:alice@example.com" {
			alice = e
		}
	}
	if alice.MentionCount != 3 || alice.FirstSeen != "2025-12-31T00:00:00Z" || alice.LastSeen != "2026-01-02T00:00:00Z" {
		t.Fatalf("gazetteer mention should extend Alice evidence bounds: %+v", alice)
	}
	grHasEdge := false
	for _, e := range edges {
		if e.Src == "memory:m2" && e.Rel == "MENTIONS" && e.Dst == "person:alice@example.com" && e.EvidenceID == "m2" {
			grHasEdge = true
		}
	}
	if !grHasEdge {
		t.Fatalf("body mention edge missing from edges: %#v", edges)
	}

	rewritten := rewritePersonEdges([]graphEdge{
		{Src: "person:a", Rel: "EMAILED", Dst: "person:b", EvidenceID: "m"},
		{Src: "person:a", Rel: "EMAILED", Dst: "person:b", EvidenceID: "m"},
		{Src: "person:b", Rel: "MENTIONS", Dst: "person:c", EvidenceID: "m2"},
		{Src: "person:b", Rel: "MENTIONS", Dst: "person:c", EvidenceID: "m2"},
	}, map[string]string{"person:b": "person:a", "person:c": "person:z"})
	if len(rewritten) != 1 {
		t.Fatalf("rewrite should drop self-loop and dedupe, got %#v", rewritten)
	}
	if rewritten[0].Src != "person:a" || rewritten[0].Dst != "person:z" {
		t.Fatalf("rewrite should map source and destination endpoints: %#v", rewritten)
	}
}

func TestGr_BuildGraphWarnsAndSkipsCappedSenders(t *testing.T) {
	from := make([]string, 0, maxParticipantFanout+1)
	for i := 0; i < maxParticipantFanout+1; i++ {
		from = append(from, fmt.Sprintf("sender%02d@example.com", i))
	}
	_, edges, warnings := buildGraph([]Memory{{
		ID:        "blast",
		Type:      "email",
		Title:     "Sender-heavy blast",
		CreatedAt: "2026-01-01T00:00:00Z",
		Meta: map[string]any{
			"from": from,
			"to":   []string{"recipient@example.com"},
		},
	}})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "capping person fan-out") {
		t.Fatalf("expected cap warning, got %#v", warnings)
	}
	for _, e := range edges {
		if e.Rel == "EMAILED" && (e.Src == "person:sender64@example.com" || e.Dst == "person:recipient@example.com") {
			t.Fatalf("capped sender/recipient should not emit EMAILED edge: %+v", e)
		}
	}
}

func TestGr_SortAndAliasReadHelpers(t *testing.T) {
	ents := []Entity{
		{Name: "Same", Kind: "person", Count: 1, MemoryIDs: []string{"b"}},
		{Name: "Same", Kind: "person", Count: 1, MemoryIDs: []string{"a"}},
		{Name: "Alpha", Kind: "tag", Count: 2, MemoryIDs: []string{"c"}},
	}
	sortEntitiesLegacy(ents)
	if ents[0].Name != "Alpha" || ents[1].MemoryIDs[0] != "a" || ents[2].MemoryIDs[0] != "b" {
		t.Fatalf("legacy sort tie-break failed: %#v", ents)
	}
	if !aliasMatches(`["Alice","alice@example.com"]`, "ALICE") {
		t.Fatal("aliasMatches should compare case-insensitively")
	}
	if aliasMatches(`not json`, "Alice") {
		t.Fatal("aliasMatches should reject malformed alias JSON")
	}
	if aliasMatches("", "Alice") {
		t.Fatal("aliasMatches should reject empty alias JSON")
	}

	names := trustedPersonNames(&personAgg{aliases: map[string]bool{
		"alice@example.com": true,
		"+15551212":         true,
		"Alice":             true,
		"Alice Example":     true,
	}})
	if !reflect.DeepEqual(names, []string{"alice example"}) {
		t.Fatalf("trustedPersonNames = %#v", names)
	}

	uf := newUnionFind([]string{"person:a", "person:z"})
	uf.union("person:z", "person:a")
	if uf.find("person:a") != uf.find("person:z") {
		t.Fatalf("reverse union should merge roots: %#v", uf.parent)
	}
}

func TestGr_GraphReadSQLiteErrorAndFallbackPaths(t *testing.T) {
	ctx := context.Background()
	cfg := grTempConfig(t)
	if graphReady(cfg) {
		t.Fatal("missing db should not be graph-ready")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath(cfg)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath(cfg), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if graphReady(cfg) {
		t.Fatal("corrupt db should not be graph-ready")
	}

	cfg = grTempConfig(t)
	if err := os.WriteFile(cfg.VaultDir, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := graphListEntities(ctx, cfg); err == nil {
		t.Fatal("graphListEntities should surface rebuild errors")
	}

	cfg = grTempConfig(t)
	db := grOpenSQLiteIndex(t, cfg)
	grExec(t, db, `CREATE TABLE entities(id TEXT, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER, salience_micros INTEGER)`)
	grExec(t, db, `CREATE TABLE edges(src TEXT, rel TEXT, dst TEXT)`)
	if _, err := graphListEntities(ctx, cfg); err == nil || !strings.Contains(err.Error(), "evidence_id") {
		t.Fatalf("graphListEntities should surface live-evidence query error, got %v", err)
	}

	cfg = grTempConfig(t)
	db = grOpenSQLiteIndex(t, cfg)
	grExec(t, db, `CREATE TABLE entities(id TEXT, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER)`)
	grExec(t, db, `CREATE TABLE edges(dst TEXT, evidence_id TEXT, invalidated_at TEXT)`)
	if _, err := graphListEntities(ctx, cfg); err == nil || !strings.Contains(err.Error(), "salience_micros") {
		t.Fatalf("graphListEntities should surface entity query error, got %v", err)
	}

	cfg = grTempConfig(t)
	db = grOpenSQLiteIndex(t, cfg)
	grExec(t, db, `CREATE TABLE entities(id TEXT, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER, salience_micros TEXT)`)
	grExec(t, db, `CREATE TABLE edges(src TEXT, rel TEXT, dst TEXT, evidence_id TEXT, observed_at TEXT, invalidated_at TEXT)`)
	grExec(t, db, `INSERT INTO entities(id, kind, display_name, aliases, mention_count, salience_micros) VALUES('tag:bad','topic','bad','[]',1,'not-int')`)
	grExec(t, db, `INSERT INTO edges(src, rel, dst, evidence_id, observed_at, invalidated_at) VALUES('memory:m','ABOUT','tag:bad','m',NULL,NULL)`)
	if _, err := graphListEntities(ctx, cfg); err == nil || !strings.Contains(err.Error(), "converting driver.Value") {
		t.Fatalf("graphListEntities should surface entity scan error, got %v", err)
	}

	cfg = grTempConfig(t)
	db = grOpenSQLiteIndex(t, cfg)
	grExec(t, db, `CREATE TABLE entities(id TEXT, kind TEXT, display_name TEXT)`)
	grExec(t, db, `CREATE TABLE edges(src TEXT, rel TEXT, dst TEXT, evidence_id TEXT, observed_at TEXT, invalidated_at TEXT)`)
	if _, err := graphGetEntity(ctx, cfg, "x"); err == nil || !strings.Contains(err.Error(), "aliases") {
		t.Fatalf("graphGetEntity should surface malformed entity query, got %v", err)
	}

	cfg = grTempConfig(t)
	db = grOpenSQLiteIndex(t, cfg)
	grExec(t, db, `CREATE TABLE entities(id TEXT, kind TEXT, display_name TEXT, aliases TEXT, mention_count TEXT, salience_micros INTEGER)`)
	grExec(t, db, `CREATE TABLE edges(src TEXT, rel TEXT, dst TEXT, evidence_id TEXT, observed_at TEXT, invalidated_at TEXT)`)
	grExec(t, db, `INSERT INTO entities(id, kind, display_name, aliases, mention_count, salience_micros) VALUES('tag:bad','topic','bad','[]','not-int',0)`)
	if _, err := graphGetEntity(ctx, cfg, "bad"); err == nil || !strings.Contains(err.Error(), "converting driver.Value") {
		t.Fatalf("graphGetEntity should surface entity scan error, got %v", err)
	}

	cfg = grTempConfig(t)
	db = grOpenSQLiteIndex(t, cfg)
	grExec(t, db, `CREATE TABLE entities(id TEXT, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER, salience_micros INTEGER)`)
	grExec(t, db, `CREATE TABLE edges(src TEXT, rel TEXT, dst TEXT, evidence_id TEXT, invalidated_at TEXT)`)
	grExec(t, db, `INSERT INTO entities(id, kind, display_name, aliases, mention_count, salience_micros) VALUES('tag:x','topic','x','[]',1,0)`)
	if _, err := graphGetEntity(ctx, cfg, "x"); err == nil || !strings.Contains(err.Error(), "observed_at") {
		t.Fatalf("graphGetEntity should surface edge query error, got %v", err)
	}

	cfg = grTempConfig(t)
	db = grOpenSQLiteIndex(t, cfg)
	grExec(t, db, `CREATE TABLE entities(id TEXT, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER, salience_micros INTEGER)`)
	grExec(t, db, `CREATE TABLE edges(src TEXT, rel TEXT, dst TEXT, evidence_id TEXT, observed_at TEXT, invalidated_at TEXT)`)
	grExec(t, db, `INSERT INTO entities(id, kind, display_name, aliases, mention_count, salience_micros) VALUES('tag:orphan','topic','orphan','[]',1,0)`)
	res, err := graphGetEntity(ctx, cfg, "orphan")
	if err != nil {
		t.Fatal(err)
	}
	if res["found"] != false {
		t.Fatalf("entity with no live evidence should be not found: %#v", res)
	}

	cfg = grTempConfig(t)
	db = grOpenSQLiteIndex(t, cfg)
	grExec(t, db, `CREATE TABLE entities(id TEXT, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER, salience_micros INTEGER)`)
	grExec(t, db, `CREATE TABLE edges(src TEXT, rel TEXT, dst TEXT, evidence_id TEXT, observed_at TEXT, invalidated_at TEXT)`)
	grExec(t, db, `INSERT INTO entities(id, kind, display_name, aliases, mention_count, salience_micros) VALUES('person:a@example.com','person','A','',1,0)`)
	grExec(t, db, `INSERT INTO edges(src, rel, dst, evidence_id, observed_at, invalidated_at) VALUES('memory:m','PARTICIPATED_IN','person:a@example.com','missing',NULL,NULL)`)
	if _, err := graphGetEntity(ctx, cfg, "A"); err == nil || !strings.Contains(err.Error(), "no such table: memories") {
		t.Fatalf("graphGetEntity should surface memory load error, got %v", err)
	}
}

func TestGr_GraphDetailFallbacksAndEvidenceLimit(t *testing.T) {
	ctx := context.Background()
	cfg := grTempConfig(t)
	db := grOpenSQLiteIndex(t, cfg)
	grExec(t, db, `CREATE TABLE entities(id TEXT, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER, salience_micros INTEGER)`)
	grExec(t, db, `CREATE TABLE edges(src TEXT, rel TEXT, dst TEXT, evidence_id TEXT, observed_at TEXT, invalidated_at TEXT)`)
	grExec(t, db, `CREATE TABLE memories(id TEXT, scope TEXT, type TEXT, title TEXT, tags TEXT, source TEXT, created_at TEXT, path TEXT, text TEXT)`)
	grExec(t, db, `INSERT INTO entities(id, kind, display_name, aliases, mention_count, salience_micros) VALUES('person:a@example.com','person','A','',9,0)`)
	for i := 0; i < 9; i++ {
		id := fmt.Sprintf("m%d", i)
		rel := "MENTIONS"
		if i%2 == 0 {
			rel = "ABOUT"
		}
		grExec(t, db, `INSERT INTO edges(src, rel, dst, evidence_id, observed_at, invalidated_at) VALUES(?,?,?,?,NULL,NULL)`, "memory:"+id, rel, "person:a@example.com", id)
		grExec(t, db, `INSERT INTO edges(src, rel, dst, evidence_id, observed_at, invalidated_at) VALUES(?,?,?,?,NULL,NULL)`, "memory:"+id, "PARTICIPATED_IN", "person:a@example.com", id)
		grExec(t, db, `INSERT INTO edges(src, rel, dst, evidence_id, observed_at, invalidated_at) VALUES(?,?,?,?,NULL,NULL)`, "memory:"+id, "PARTICIPATED_IN", "person:b@example.com", id)
		grExec(t, db, `INSERT INTO memories(id, scope, type, title, tags, source, created_at, path, text) VALUES(?,?,?,?,?,?,?,?,?)`,
			id, "s", "note", "Title "+id, "", "src", fmt.Sprintf("2026-01-%02dT00:00:00Z", i+1), "/missing-"+id+".md", "text")
	}
	grExec(t, db, `INSERT INTO edges(src, rel, dst, evidence_id, observed_at, invalidated_at) VALUES('memory:mx','MENTIONS','person:a@example.com','mx',NULL,NULL)`)
	grExec(t, db, `INSERT INTO memories(id, scope, type, title, tags, source, created_at, path, text) VALUES('mx','s','note','Title mx','','src','2026-01-10T00:00:00Z','/missing-mx.md','text')`)
	var out bytes.Buffer
	if err := graphDetailView(ctx, cfg, &out, "A", false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"A  (person)", "Connected people", "Relationships", "Evidence", "and 2 more", "b@example.com"} {
		if !strings.Contains(got, want) {
			t.Fatalf("detail output missing %q:\n%s", want, got)
		}
	}

	badCfg := grTempConfig(t)
	if err := os.WriteFile(badCfg.VaultDir, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := graphDetailView(ctx, badCfg, &bytes.Buffer{}, "A", false); err == nil {
		t.Fatal("graphDetailView should return graphGetEntity errors")
	}
}

func TestGr_SQLReadHelpersSuccessAndErrors(t *testing.T) {
	ctx := context.Background()

	db := grOpenScriptDB(t, grSQLQuery{
		cols: []string{"dst", "evidence_id"},
		rows: [][]driver.Value{{"person:a", "m1"}, {"person:a", "m1"}, {"person:a", "m2"}},
	})
	evidence, err := liveEvidenceByEntity(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence["person:a"], []string{"m1", "m2"}) {
		t.Fatalf("deduped evidence = %#v", evidence)
	}

	db = grOpenScriptDB(t, grSQLQuery{queryErr: errors.New("edge query failed")})
	if _, err := liveEvidenceByEntity(ctx, db); err == nil || !strings.Contains(err.Error(), "edge query failed") {
		t.Fatalf("liveEvidenceByEntity query err = %v", err)
	}

	db = grOpenScriptDB(t, grSQLQuery{cols: []string{"dst"}, rows: [][]driver.Value{{"person:a"}}})
	if _, err := liveEvidenceByEntity(ctx, db); err == nil || !strings.Contains(err.Error(), "expected 1 destination") {
		t.Fatalf("liveEvidenceByEntity scan err = %v", err)
	}

	db = grOpenScriptDB(t, grSQLQuery{cols: []string{"dst", "evidence_id"}, rowsErr: errors.New("edge rows failed")})
	if _, err := liveEvidenceByEntity(ctx, db); err == nil || !strings.Contains(err.Error(), "edge rows failed") {
		t.Fatalf("liveEvidenceByEntity rows err = %v", err)
	}

	if mems, err := loadMemoriesByID(ctx, db, nil); err != nil || mems != nil {
		t.Fatalf("empty loadMemoriesByID = %#v, %v", mems, err)
	}

	db = grOpenScriptDB(t, grSQLQuery{
		cols: []string{"id", "scope", "type", "title", "tags", "source", "created_at", "path", "text"},
		rows: [][]driver.Value{
			{"m-a", "s", "note", "Tie A", "", "src", "2026-01-02T00:00:00Z", "/missing-a.md", "a text"},
			{"m-old", "s", "note", "Old", "b,a", "src", "2026-01-01T00:00:00Z", "/missing-old.md", "old text"},
			{"m-new", "s", "note", "New", "", "src", "2026-01-02T00:00:00Z", "/missing-new.md", "new text"},
		},
	})
	mems, err := loadMemoriesByID(ctx, db, []string{"m-old", "m-new", "m-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 3 || mems[0].ID != "m-a" || mems[1].ID != "m-new" || !reflect.DeepEqual(mems[2].Tags, []string{"b", "a"}) {
		t.Fatalf("loadMemoriesByID fallback/sort/tags = %#v", mems)
	}

	db = grOpenScriptDB(t, grSQLQuery{queryErr: errors.New("memory query failed")})
	if _, err := loadMemoriesByID(ctx, db, []string{"m"}); err == nil || !strings.Contains(err.Error(), "memory query failed") {
		t.Fatalf("loadMemoriesByID query err = %v", err)
	}

	db = grOpenScriptDB(t, grSQLQuery{cols: []string{"id"}, rows: [][]driver.Value{{"m"}}})
	if _, err := loadMemoriesByID(ctx, db, []string{"m"}); err == nil || !strings.Contains(err.Error(), "expected 1 destination") {
		t.Fatalf("loadMemoriesByID scan err = %v", err)
	}

	db = grOpenScriptDB(t, grSQLQuery{
		cols:    []string{"id", "scope", "type", "title", "tags", "source", "created_at", "path", "text"},
		rowsErr: errors.New("memory rows failed"),
	})
	if _, err := loadMemoriesByID(ctx, db, []string{"m"}); err == nil || !strings.Contains(err.Error(), "memory rows failed") {
		t.Fatalf("loadMemoriesByID rows err = %v", err)
	}

	db = grOpenScriptDB(t, grSQLQuery{cols: []string{"dst"}, rows: [][]driver.Value{{"person:b"}, {"person:c"}}})
	co, err := coOccurringPeople(ctx, db, "person:a")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(co, []string{"person:b", "person:c"}) {
		t.Fatalf("coOccurringPeople = %#v", co)
	}

	db = grOpenScriptDB(t, grSQLQuery{queryErr: errors.New("co query failed")})
	if _, err := coOccurringPeople(ctx, db, "person:a"); err == nil || !strings.Contains(err.Error(), "co query failed") {
		t.Fatalf("coOccurringPeople query err = %v", err)
	}

	db = grOpenScriptDB(t, grSQLQuery{cols: []string{"dst", "extra"}, rows: [][]driver.Value{{"person:b", "x"}}})
	if _, err := coOccurringPeople(ctx, db, "person:a"); err == nil || !strings.Contains(err.Error(), "expected 2 destination") {
		t.Fatalf("coOccurringPeople scan err = %v", err)
	}

	db = grOpenScriptDB(t, grSQLQuery{cols: []string{"dst"}, rowsErr: errors.New("co rows failed")})
	if _, err := coOccurringPeople(ctx, db, "person:a"); err == nil || !strings.Contains(err.Error(), "co rows failed") {
		t.Fatalf("coOccurringPeople rows err = %v", err)
	}
}
