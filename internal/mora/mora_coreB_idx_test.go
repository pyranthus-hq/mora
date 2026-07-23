package mora

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// coreBIdxsetUserVersion opens the on-disk index read-write and stamps
// PRAGMA user_version, simulating an index written by a different schema version.
func coreBIdxsetUserVersion(t *testing.T, cfg Config, v int) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatalf("open rw index: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA user_version = " + coreBIdxItoa(v)); err != nil {
		t.Fatalf("set user_version=%d: %v", v, err)
	}
}

// coreBIdxItoa is a tiny int->string used only for the PRAGMA literal above.
func coreBIdxItoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// coreBIdxpinAutoHeal overrides the indexAutoHeal package var and restores it.
func coreBIdxpinAutoHeal(t *testing.T, heal bool) {
	t.Helper()
	saved := indexAutoHeal
	indexAutoHeal = func(cfg Config) bool { return heal }
	t.Cleanup(func() { indexAutoHeal = saved })
}

// coreBIdxpopulatedVault builds a sandbox config with `mems` memories written and a
// bound vault marker + rebuilt index. Returns the config.
func coreBIdxpopulatedVault(t *testing.T, markerID string, mems []Memory) Config {
	t.Helper()
	t.Setenv("MORA_EMBEDDER", "") // force the deterministic static embedder
	cfg := sandboxCfg(t)
	for _, m := range mems {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("writeMemory: %v", err)
		}
	}
	if _, err := createVaultMarkerIfAbsent(cfg, markerID); err != nil {
		t.Fatalf("create marker: %v", err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("initial rebuild: %v", err)
	}
	return cfg
}

func coreBIdxmem(id, scope, typ, title, text string) Memory {
	return Memory{ID: id, Scope: scope, Type: typ, Title: title, Source: "manual", CreatedAt: nowRFC3339(), Text: text}
}

// TestCoreB_IdxCheckIndexSchemaMatchAndMismatch pins both branches of
// checkIndexSchema: the current version passes, a wrong version returns the
// actionable "different mora version" error naming both versions.
func TestCoreB_IdxCheckIndexSchemaMatchAndMismatch(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "schema.db")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec("PRAGMA user_version = " + coreBIdxItoa(indexSchemaVersion)); err != nil {
		t.Fatal(err)
	}
	if err := checkIndexSchema(db); err != nil {
		t.Fatalf("matching version must pass, got %v", err)
	}

	if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	err = checkIndexSchema(db)
	if err == nil {
		t.Fatal("wrong version must error")
	}
	for _, want := range []string{"different mora version", "index schema v99", "expects v" + coreBIdxItoa(indexSchemaVersion), "mora index rebuild"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("schema error %q missing %q", err.Error(), want)
		}
	}

	// Scan-error branch: a closed handle surfaces the query error, never masks it.
	closed, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if err := checkIndexSchema(closed); err == nil {
		t.Fatal("checkIndexSchema on a closed db must return the scan error")
	} else if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed-db error = %v, want a 'database is closed' error", err)
	}
}

// TestCoreB_IdxOpenIndexROValid: a freshly built index opens read-only and serves
// a working handle (Ping + a real row count through the returned db).
func TestCoreB_IdxOpenIndexROValid(t *testing.T) {
	cfg := coreBIdxpopulatedVault(t, "v_ro", []Memory{
		coreBIdxmem(newID(), "global", "insight", "one", "first body"),
		coreBIdxmem(newID(), "global", "insight", "two", "second body"),
	})
	db, err := openIndexRO(context.Background(), cfg)
	if err != nil {
		t.Fatalf("openIndexRO on a valid index: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if n != 2 {
		t.Fatalf("openIndexRO db memory count = %d, want 2", n)
	}
}

// TestCoreB_IdxOpenIndexROStaleNoHeal: a version-stale index with auto-heal pinned
// OFF returns the schema error (the semantic-embedder path that must not stall a
// read with a minutes-long re-embed).
func TestCoreB_IdxOpenIndexROStaleNoHeal(t *testing.T) {
	cfg := coreBIdxpopulatedVault(t, "v_stale", []Memory{
		coreBIdxmem(newID(), "global", "insight", "keep", "precious body"),
	})
	coreBIdxsetUserVersion(t, cfg, 1)
	coreBIdxpinAutoHeal(t, false)

	db, err := openIndexRO(context.Background(), cfg)
	if err == nil {
		db.Close()
		t.Fatal("stale index with no auto-heal must return the schema error")
	}
	if !strings.Contains(err.Error(), "different mora version") {
		t.Fatalf("want schema-mismatch error, got %v", err)
	}
	// The index must be left untouched (still stale) — no inline rebuild fired.
	verDB, _ := sql.Open("sqlite", dbPath(cfg))
	defer verDB.Close()
	var v int
	if err := verDB.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Fatalf("no-heal path must not rebuild; user_version = %d, want 1", v)
	}
}

// TestCoreB_IdxOpenIndexROStaleAutoHeals: a version-stale index with auto-heal
// pinned ON is rebuilt inline and the returned handle serves the freshly stamped
// (v2) index with the full corpus.
func TestCoreB_IdxOpenIndexROStaleAutoHeals(t *testing.T) {
	cfg := coreBIdxpopulatedVault(t, "v_heal", []Memory{
		coreBIdxmem(newID(), "global", "insight", "a", "alpha body"),
		coreBIdxmem(newID(), "global", "insight", "b", "beta body"),
	})
	coreBIdxsetUserVersion(t, cfg, 1)
	coreBIdxpinAutoHeal(t, true)

	db, err := openIndexRO(context.Background(), cfg)
	if err != nil {
		t.Fatalf("auto-heal must succeed, got %v", err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != indexSchemaVersion {
		t.Fatalf("healed index user_version = %d, want %d", v, indexSchemaVersion)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("healed index memory count = %d, want 2", n)
	}
}

// TestCoreB_IdxRebuildBuildsCorpusGraphVectors is the happy path: a rebuild indexes
// every live memory, materializes the structural entity graph (a [[link]] → entity +
// MENTIONS edge), writes one static-embedding vector per memory, and binds
// index_meta (memory_count + a freshly adopted vault_id).
func TestCoreB_IdxRebuildBuildsCorpusGraphVectors(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	cfg := sandboxCfg(t)
	m1 := coreBIdxmem(newID(), "project:acme", "insight", "Sync notes", "we synced with [[Alice]] today")
	m2 := coreBIdxmem(newID(), "global", "insight", "Other", "unrelated body")
	for _, m := range []Memory{m1, m2} {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	count, err := rebuildIndex(context.Background(), cfg)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if count != 2 {
		t.Fatalf("rebuild count = %d, want 2", count)
	}

	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var mc int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&mc); err != nil {
		t.Fatal(err)
	}
	if mc != 2 {
		t.Fatalf("memories rows = %d, want 2", mc)
	}

	// writeGraph: the [[Alice]] link becomes a topic entity with mention_count 1 and
	// an empty (structural) aliases array.
	var display, aliases string
	var mention int
	if err := db.QueryRow(`SELECT display_name, aliases, mention_count FROM entities WHERE id='link:Alice'`).Scan(&display, &aliases, &mention); err != nil {
		t.Fatalf("link:Alice entity: %v", err)
	}
	if display != "Alice" || mention != 1 || aliases != "[]" {
		t.Fatalf("link:Alice = display %q mention %d aliases %q; want Alice/1/[]", display, mention, aliases)
	}
	var edgeCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM edges WHERE rel='MENTIONS' AND dst='link:Alice' AND evidence_id=?`, m1.ID).Scan(&edgeCount); err != nil {
		t.Fatal(err)
	}
	if edgeCount != 1 {
		t.Fatalf("MENTIONS link:Alice edge count = %d, want 1", edgeCount)
	}

	// writeVectors: one static-hash vector per live memory.
	var vecCount, dim int
	var model string
	if err := db.QueryRow(`SELECT COUNT(*) FROM mem_vectors`).Scan(&vecCount); err != nil {
		t.Fatal(err)
	}
	if vecCount != 2 {
		t.Fatalf("mem_vectors rows = %d, want 2", vecCount)
	}
	if err := db.QueryRow(`SELECT dim, model FROM mem_vectors WHERE memory_id=?`, m1.ID).Scan(&dim, &model); err != nil {
		t.Fatal(err)
	}
	if dim != 256 || model != "static-hash-v1" {
		t.Fatalf("vector dim/model = %d/%q, want 256/static-hash-v1", dim, model)
	}

	// index_meta bindings.
	var memCountMeta, vaultID string
	if err := db.QueryRow(`SELECT value FROM index_meta WHERE key='memory_count'`).Scan(&memCountMeta); err != nil {
		t.Fatal(err)
	}
	if memCountMeta != "2" {
		t.Fatalf("index_meta memory_count = %q, want 2", memCountMeta)
	}
	if err := db.QueryRow(`SELECT value FROM index_meta WHERE key='vault_id'`).Scan(&vaultID); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(vaultID, "v_") {
		t.Fatalf("adopted vault_id = %q, want a v_-prefixed id", vaultID)
	}
	// A marker was written binding that same id.
	m, present, err := readVaultMarker(cfg)
	if err != nil || !present {
		t.Fatalf("marker present=%v err=%v", present, err)
	}
	if m.VaultID != vaultID {
		t.Fatalf("marker id %q != index vault_id %q", m.VaultID, vaultID)
	}
}

// TestCoreB_IdxRebuildSkipsTombstoneAndUnparseable: a tombstoned memory and an
// unparseable .md file are both excluded from the searchable corpus, but the
// tombstone still emits its (invalidated) graph edge.
func TestCoreB_IdxRebuildSkipsTombstoneAndUnparseable(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	cfg := sandboxCfg(t)
	live := coreBIdxmem(newID(), "global", "insight", "live", "a live [[Alice]] memory")
	if err := writeMemory(cfg, live); err != nil {
		t.Fatal(err)
	}
	tomb := coreBIdxmem(newID(), "global", "insight", "gone", "a dead [[Bob]] memory")
	tomb.DeletedAt = "2026-01-02T00:00:00Z"
	if err := writeMemory(cfg, tomb); err != nil {
		t.Fatal(err)
	}
	// An unparseable .md file (no frontmatter) — parseMemory errors, rebuild skips it.
	if err := os.WriteFile(filepath.Join(memoriesRoot(cfg), "garbage.md"), []byte("no frontmatter here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	count, err := rebuildIndex(context.Background(), cfg)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if count != 1 {
		t.Fatalf("indexed count = %d, want 1 (tombstone + garbage skipped)", count)
	}

	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// The tombstoned memory must NOT be searchable.
	var tombRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id=?`, tomb.ID).Scan(&tombRows); err != nil {
		t.Fatal(err)
	}
	if tombRows != 0 {
		t.Fatalf("tombstone leaked into memories (%d rows)", tombRows)
	}
	// ...but its edge exists, stamped invalidated (NOT NULL).
	var inval sql.NullString
	if err := db.QueryRow(`SELECT invalidated_at FROM edges WHERE dst='link:Bob' AND evidence_id=?`, tomb.ID).Scan(&inval); err != nil {
		t.Fatalf("tombstone edge to link:Bob missing: %v", err)
	}
	if !inval.Valid || inval.String != "2026-01-02T00:00:00Z" {
		t.Fatalf("tombstone edge invalidated_at = %+v, want the deleted_at stamp", inval)
	}
}

// TestCoreB_IdxRebuildEnforceBlocksEmptyAllowCommits: an all-files-removed rebuild
// is BLOCKED under policyEnforce (index untouched, a block record written) and
// COMMITTED under policyAllow (index emptied, block record cleared).
func TestCoreB_IdxRebuildEnforceBlocksEmptyAllowCommits(t *testing.T) {
	cfg := coreBIdxpopulatedVault(t, "v_keep", []Memory{
		coreBIdxmem(newID(), "global", "insight", "one", "keep one"),
		coreBIdxmem(newID(), "global", "insight", "two", "keep two"),
	})
	if got := indexCount(t, cfg); got != 2 {
		t.Fatalf("primed index count = %d, want 2", got)
	}

	// Remove every memory file so the next rebuild would empty the index.
	if err := os.RemoveAll(memoriesRoot(cfg)); err != nil {
		t.Fatal(err)
	}

	// Enforce: blocked.
	c, err := rebuildIndexWithPolicy(context.Background(), cfg, policyEnforce)
	if !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("enforce empty-vault rebuild err = %v, want errRebuildBlocked", err)
	}
	if c != 0 {
		t.Fatalf("blocked rebuild new count = %d, want 0", c)
	}
	for _, want := range []string{"has no memory files", "index holds 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("block message %q missing %q", err.Error(), want)
		}
	}
	// The existing index is left intact.
	if got := indexCount(t, cfg); got != 2 {
		t.Fatalf("index count after block = %d, want 2 (untouched)", got)
	}
	// A diagnostic block record was written.
	rec, err := os.ReadFile(blockRecordPath(cfg))
	if err != nil {
		t.Fatalf("block record not written: %v", err)
	}
	if !strings.Contains(string(rec), "vault looked empty") {
		t.Fatalf("block record = %s, want reason 'vault looked empty'", rec)
	}

	// Allow: commits the empty index and clears the block record.
	c, err = rebuildIndexWithPolicy(context.Background(), cfg, policyAllow)
	if err != nil {
		t.Fatalf("policyAllow empty rebuild err = %v, want nil", err)
	}
	if c != 0 {
		t.Fatalf("allow rebuild count = %d, want 0", c)
	}
	if got := indexCount(t, cfg); got != 0 {
		t.Fatalf("index count after allow = %d, want 0 (committed empty)", got)
	}
	if _, err := os.Stat(blockRecordPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("block record should be cleared after a good rebuild, stat err = %v", err)
	}
}

// TestCoreB_IdxRebuildCorruptMarkerErrors: an unreadable vault marker fails the
// rebuild loud (identity is in question) rather than clobbering the index.
func TestCoreB_IdxRebuildCorruptMarkerErrors(t *testing.T) {
	cfg := coreBIdxpopulatedVault(t, "v_x", []Memory{
		coreBIdxmem(newID(), "global", "insight", "keep", "precious"),
	})
	if err := os.WriteFile(markerPath(cfg), []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := rebuildIndex(context.Background(), cfg)
	if err == nil {
		t.Fatal("corrupt marker must fail the rebuild")
	}
	if !strings.Contains(err.Error(), "unreadable (corrupt JSON)") {
		t.Fatalf("want corrupt-marker error, got %v", err)
	}
}

// TestCoreB_IdxRebuildReStampsLostMarker: when the marker is LOST but the index
// still knows its id, a policyAllow rebuild self-heals identity by re-stamping the
// marker bound to the index's own id (so a later Enforce rebuild no longer blocks).
func TestCoreB_IdxRebuildReStampsLostMarker(t *testing.T) {
	cfg := coreBIdxpopulatedVault(t, "v_lost", []Memory{
		coreBIdxmem(newID(), "global", "insight", "one", "body one"),
		coreBIdxmem(newID(), "global", "insight", "two", "body two"),
	})
	boundID, err := readIndexVaultID(context.Background(), cfg)
	if err != nil || boundID != "v_lost" {
		t.Fatalf("index vault_id = %q err=%v, want v_lost", boundID, err)
	}
	// Delete the marker (memories remain) → next rebuild sees indexID set, marker absent.
	if err := os.Remove(markerPath(cfg)); err != nil {
		t.Fatal(err)
	}

	c, err := rebuildIndexWithPolicy(context.Background(), cfg, policyAllow)
	if err != nil {
		t.Fatalf("policyAllow re-stamp rebuild err = %v", err)
	}
	if c != 2 {
		t.Fatalf("re-stamp rebuild count = %d, want 2", c)
	}
	m, present, err := readVaultMarker(cfg)
	if err != nil || !present {
		t.Fatalf("marker not re-stamped: present=%v err=%v", present, err)
	}
	if m.VaultID != "v_lost" {
		t.Fatalf("re-stamped marker id = %q, want v_lost (the index's own id)", m.VaultID)
	}
	// Identity self-healed: a subsequent Enforce rebuild now proceeds.
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("enforce rebuild after self-heal must proceed, got %v", err)
	}
}

// TestCoreB_IdxRebuildAdoptsExistingMarkerSeed: a fresh index (no prior vault_id)
// adopts the id of a marker that already exists, rather than minting a new one.
func TestCoreB_IdxRebuildAdoptsExistingMarkerSeed(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	cfg := sandboxCfg(t)
	if _, err := createVaultMarkerIfAbsent(cfg, "v_seed"); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, coreBIdxmem(newID(), "global", "insight", "m", "seed body")); err != nil {
		t.Fatal(err)
	}
	count, err := rebuildIndex(context.Background(), cfg)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	id, err := readIndexVaultID(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if id != "v_seed" {
		t.Fatalf("index adopted vault_id %q, want v_seed", id)
	}
}

// TestCoreB_IdxWriteGraphDirect drives writeGraph on a bare transaction to pin the
// branches a default rebuild doesn't reach: person entities (non-empty aliases
// array), the fan-out warning loop (>maxParticipantFanout participants), and the
// nullStr NULL persistence for an empty timestamp.
func TestCoreB_IdxWriteGraphDirect(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "graph.db")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE entities (id TEXT PRIMARY KEY, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER, first_seen TEXT, last_seen TEXT, salience_micros INTEGER)`,
		`CREATE TABLE edges (src TEXT, rel TEXT, dst TEXT, evidence_id TEXT, valid_from TEXT, valid_to TEXT, observed_at TEXT, invalidated_at TEXT, PRIMARY KEY (src, rel, dst, evidence_id))`,
		`CREATE TABLE person_merges (member_a TEXT, member_b TEXT, signal TEXT, detail TEXT, PRIMARY KEY (member_a, member_b, signal))`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}

	structural := Memory{ID: "m_struct", Scope: "project:acme", Type: "insight", Title: "Sync", Text: "[[Alice]] and [[Bob]]", CreatedAt: "2026-01-01T00:00:00Z"}
	nullTs := Memory{ID: "m_null", Scope: "global", Type: "insight", Title: "NoTime", Text: "[[Carol]]"} // empty CreatedAt → NULL edge stamps

	// A blast with more than maxParticipantFanout participants trips the warning loop.
	to := make([]string, 0, maxParticipantFanout+1)
	for i := 0; i <= maxParticipantFanout; i++ {
		to = append(to, "r"+coreBIdxItoa(i)+"@example.com")
	}
	blast := Memory{
		ID: "m_fan", Scope: "global", Type: "email", Title: "Blast",
		Text: "wide email", CreatedAt: "2026-01-03T00:00:00Z",
		Meta: map[string]any{"from": []string{"sender@example.com"}, "to": to},
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeGraph(context.Background(), tx, Config{VaultDir: t.TempDir()}, []Memory{structural, nullTs, blast}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("writeGraph: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Structural entity: empty aliases array; project kind for a project scope.
	var aliases, kind string
	if err := db.QueryRow(`SELECT aliases FROM entities WHERE id='link:Alice'`).Scan(&aliases); err != nil {
		t.Fatalf("link:Alice: %v", err)
	}
	if aliases != "[]" {
		t.Fatalf("structural aliases = %q, want []", aliases)
	}
	if err := db.QueryRow(`SELECT kind FROM entities WHERE id='scope:project:acme'`).Scan(&kind); err != nil {
		t.Fatalf("scope entity: %v", err)
	}
	if kind != "project" {
		t.Fatalf("scope:project:acme kind = %q, want project", kind)
	}

	// Person entity carries a non-empty aliases array (the sender's own address).
	if err := db.QueryRow(`SELECT aliases FROM entities WHERE id='person:sender@example.com'`).Scan(&aliases); err != nil {
		t.Fatalf("sender entity: %v", err)
	}
	if !strings.Contains(aliases, "sender@example.com") || aliases == "[]" {
		t.Fatalf("sender aliases = %q, want a non-empty array containing the address", aliases)
	}

	// nullStr: the timestamp-less memory's edge persists valid_from / observed_at as SQL NULL.
	var vf, obs sql.NullString
	if err := db.QueryRow(`SELECT valid_from, observed_at FROM edges WHERE dst='link:Carol'`).Scan(&vf, &obs); err != nil {
		t.Fatalf("link:Carol edge: %v", err)
	}
	if vf.Valid || obs.Valid {
		t.Fatalf("empty-timestamp edge should be NULL, got valid_from=%+v observed_at=%+v", vf, obs)
	}
	// A stamped edge keeps its value (nullStr non-empty branch).
	var vf2 sql.NullString
	if err := db.QueryRow(`SELECT valid_from FROM edges WHERE dst='link:Alice' AND evidence_id='m_struct'`).Scan(&vf2); err != nil {
		t.Fatal(err)
	}
	if !vf2.Valid || vf2.String != "2026-01-01T00:00:00Z" {
		t.Fatalf("stamped edge valid_from = %+v, want 2026-01-01T00:00:00Z", vf2)
	}

	// The fan-out capped the person entities: fewer than the 66 references, but the
	// sender survived (senders are never capped away).
	var personCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM entities WHERE id LIKE 'person:%'`).Scan(&personCount); err != nil {
		t.Fatal(err)
	}
	if personCount == 0 || personCount > maxParticipantFanout {
		t.Fatalf("person entity count = %d, want in (0, %d]", personCount, maxParticipantFanout)
	}
}

// TestCoreB_IdxWriteVectorsEmbedderEncoding drives writeVectors with a (fake)
// Ollama embedder to pin the model id / dim stamped per row and the exact encoded
// vector bytes (encodeVec round-trips through decodeVec to the normalized embedding).
func TestCoreB_IdxWriteVectorsEmbedderEncoding(t *testing.T) {
	srv := fakeOllama(t, []float64{3, 4}) // |v| = 5 → normalizes to 0.6, 0.8
	defer srv.Close()
	emb := ollamaEmbedder{baseURL: srv.URL, model: "nomic-embed-text", dim: 2, client: &http.Client{Timeout: 5 * time.Second}}

	dbFile := filepath.Join(t.TempDir(), "vec.db")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE mem_vectors (memory_id TEXT PRIMARY KEY, dim INT, model TEXT, vec BLOB)`); err != nil {
		t.Fatal(err)
	}

	mems := []Memory{
		{ID: "v1", Title: "t1", Text: "b1"},
		{ID: "v2", Title: "t2", Text: "b2"},
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeVectors(context.Background(), tx, emb, mems); err != nil {
		_ = tx.Rollback()
		t.Fatalf("writeVectors: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM mem_vectors`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("mem_vectors rows = %d, want 2", n)
	}

	var dim int
	var model string
	var blob []byte
	if err := db.QueryRow(`SELECT dim, model, vec FROM mem_vectors WHERE memory_id='v1'`).Scan(&dim, &model, &blob); err != nil {
		t.Fatal(err)
	}
	if dim != 2 || model != "ollama:nomic-embed-text" {
		t.Fatalf("stamped dim/model = %d/%q, want 2/ollama:nomic-embed-text", dim, model)
	}
	vec := decodeVec(blob)
	if len(vec) != 2 {
		t.Fatalf("decoded vec len = %d, want 2", len(vec))
	}
	if math.Abs(float64(vec[0])-0.6) > 1e-5 || math.Abs(float64(vec[1])-0.8) > 1e-5 {
		t.Fatalf("decoded vec = %v, want [0.6 0.8]", vec)
	}
}
