package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func cmdIndex(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "rebuild even if the vault looks empty or unfamiliar")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || fs.Arg(0) != "rebuild" {
		return errors.New("usage: mora index rebuild [--force]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	policy := policyEnforce
	if *force {
		policy = policyAllow
	}
	count, err := rebuildIndexWithPolicy(ctx, cfg, policy)
	if err != nil {
		return err
	}
	if err := writeWikiIndex(cfg, count); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "indexed %d memories\n", count)
	return nil
}
func dbPath(cfg Config) string { return filepath.Join(cfg.DataDir, "index.db") }

// roIndexDSN is the DSN every read-only index open uses. busy_timeout matters
// for READERS too: the hourly rebuild does a whole-index DELETE-then-reinsert
// inside ONE transaction, and its commit flush of a large rollback journal can
// hold the writer lock for longer than a few seconds. A short reader timeout
// surfaces a raw "database is locked" mid-rebuild (and openIndexRO can misread
// that SQLITE_BUSY as a stale schema and launch a spurious rebuild). 15s lets an
// interactive read (brief --entity, prep, think, get_entity) and an MCP tool call
// ride out the rebuild's commit window instead of erroring; humanizeIndexBusy
// gives an actionable message if a read still outlasts it.
// (TestReadOnlyIndexWaitsOnWriteLock pins the wait behavior.)
//
// Note: with modernc.org/sqlite, mode=ro on a non-"file:" DSN is parsed out
// but NOT enforced (connections open read-write); it is kept as
// documentation-of-intent until the read paths adopt a stricter pragma.
func roIndexDSN(cfg Config) string {
	return dbPath(cfg) + "?mode=ro&_pragma=busy_timeout(15000)"
}

// openIndexRO opens the index read-only, refusing to serve a schema this
// binary doesn't understand (a swapped binary otherwise reads missing columns
// or zeroed salience silently). A stale index self-heals inline when
// indexAutoHeal allows; otherwise the error names the exact fix, and
// `mora upgrade` runs the rebuild at the moment the user consented to a slow
// step.
func openIndexRO(ctx context.Context, cfg Config) (*sql.DB, error) {
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		return nil, err
	}
	verr := checkIndexSchema(db)
	if verr == nil {
		return db, nil
	}
	_ = db.Close()
	if !indexAutoHeal(cfg) {
		return nil, verr
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return nil, fmt.Errorf("rebuilding a stale index (%v) failed: %w", verr, err)
	}
	db, err = sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		return nil, err
	}
	if err := checkIndexSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
func checkIndexSchema(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v != indexSchemaVersion {
		return fmt.Errorf("the search index was built by a different mora version (index schema v%d, this binary expects v%d) — run `mora index rebuild`", v, indexSchemaVersion)
	}
	return nil
}
func rebuildIndex(ctx context.Context, cfg Config) (int, error) {
	return rebuildIndexWithPolicy(ctx, cfg, policyEnforce)
}
func rebuildIndexWithPolicy(ctx context.Context, cfg Config, policy rebuildPolicy) (int, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return 0, err
	}
	// _txlock=immediate acquires the write lock at BeginTx instead of lazily
	// upgrading a deferred read lock mid-transaction — two concurrent rebuilds
	// would otherwise both start, then one hits SQLITE_BUSY on the first write and
	// cannot retry inside an open tx. The 15s busy_timeout matches the RO DSN so a
	// rebuild waits out a contending writer rather than failing fast.
	db, err := sql.Open("sqlite", dbPath(cfg)+"?_txlock=immediate&_pragma=busy_timeout(15000)")
	if err != nil {
		return 0, err
	}
	defer db.Close()

	// Rebuild the whole index inside ONE transaction so a mid-rebuild failure
	// rolls back to the prior committed index instead of leaving a half-empty
	// one (the DELETE-then-reinsert is destructive). Every write — schema,
	// DELETEs, memories, and FTS — must go through this same tx.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds; the safety net on any early return

	// List the vault only AFTER BeginTx has taken the immediate write lock, so the
	// snapshot is serialized against every other rebuild. Listing BEFORE the lock
	// let two concurrent rebuilds interleave: rebuild A lists, rebuild B (fired by
	// a newer write) lists+commits, then A commits LAST carrying its OLDER list,
	// silently dropping the just-written memory until a later rebuild. Because the
	// lock serializes rebuilds, whichever commits later necessarily listed later,
	// so a committed index can no longer be clobbered by an older rebuild's stale
	// snapshot (a memory written AFTER the surviving rebuild's listing is ordinary
	// until-next-rebuild staleness, not this race). Routed through listRebuildFiles
	// so a test can assert the lock is held at listing time (allMemoryFiles is a
	// pure filesystem walk — it takes no DB lock, so it cannot deadlock this tx).
	files, err := listRebuildFiles(cfg)
	if err != nil {
		return 0, err
	}

	// Capture old state BEFORE the destructive DELETEs run — on a fresh db the
	// tables do not exist yet, so ignore errors and keep the zero values.
	oldCount := 0
	_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories`).Scan(&oldCount)
	indexID := ""
	_ = tx.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key='vault_id'`).Scan(&indexID)

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memories (id TEXT PRIMARY KEY, scope TEXT, type TEXT, title TEXT, tags TEXT, source TEXT, created_at TEXT, path TEXT, text TEXT)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(id, scope, title, tags, source, text)`,
		// Entity graph (I1): rebuildable from the vault Markdown, never the only home of state.
		// salience_micros (Phase 14): the frozen person-ranking sort key. A FRESH db gets it
		// from this CREATE; an EXISTING pre-column db gets it from the additive ALTER below
		// (CREATE TABLE IF NOT EXISTS is a no-op once the table exists, so it can't add it).
		`CREATE TABLE IF NOT EXISTS entities (id TEXT PRIMARY KEY, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER, first_seen TEXT, last_seen TEXT, salience_micros INTEGER)`,
		`CREATE TABLE IF NOT EXISTS edges (src TEXT, rel TEXT, dst TEXT, evidence_id TEXT, valid_from TEXT, valid_to TEXT, observed_at TEXT, invalidated_at TEXT, PRIMARY KEY (src, rel, dst, evidence_id))`,
		`CREATE INDEX IF NOT EXISTS idx_edges_src ON edges(src)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_dst ON edges(dst)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_rel ON edges(rel)`,
		`CREATE INDEX IF NOT EXISTS idx_entities_kind ON entities(kind)`,
		// Hybrid retrieval (I2): one static-embedding vector per memory, rebuildable.
		`CREATE TABLE IF NOT EXISTS mem_vectors (memory_id TEXT PRIMARY KEY, dim INT, model TEXT, vec BLOB)`,
		// index_meta: vault-binding key/value store. Rows are rewritten by upsert on
		// each rebuild; deliberately NOT in the DELETE list so vault_id persists
		// across rebuilds and the guard can compare it.
		`CREATE TABLE IF NOT EXISTS index_meta (key TEXT PRIMARY KEY, value TEXT)`,
		`DELETE FROM memories`,
		`DELETE FROM memories_fts`,
		`DELETE FROM entities`,
		`DELETE FROM edges`,
		`DELETE FROM mem_vectors`,
		// Stamp the schema this binary writes (read paths refuse a mismatch).
		// Inside the same tx as everything else: a rolled-back rebuild must not
		// leave a fresh stamp on a stale index.
		fmt.Sprintf(`PRAGMA user_version = %d`, indexSchemaVersion),
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return 0, err
		}
	}
	// Additive-by-rebuild migration (Phase 14): an EXISTING entities table predating
	// salience_micros is NOT touched by the CREATE TABLE IF NOT EXISTS above (no-op once
	// the table exists), so add the column here. On a FRESH db the column already exists
	// and SQLite errors "duplicate column name" — tolerated so the atomic rebuild tx is
	// never aborted by this idempotent ALTER. Any OTHER error is fatal.
	if _, err := tx.ExecContext(ctx, `ALTER TABLE entities ADD COLUMN salience_micros INTEGER`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return 0, err
	}
	memStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO memories VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer memStmt.Close()
	ftsStmt, err := tx.PrepareContext(ctx, `INSERT INTO memories_fts VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer ftsStmt.Close()
	count := 0
	var parsed []Memory // ALL memories (incl. tombstones) — feeds the graph
	var live []Memory   // non-tombstoned only — the searchable corpus + vectors
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil {
			continue
		}
		parsed = append(parsed, m)
		// Tombstones (connector-produced deleted_at, e.g. a cancelled calendar
		// event — `mora delete` hard-removes the file instead) must NOT be
		// retrievable. Keep them out of memories / memories_fts (and mem_vectors
		// below), the single chokepoint every search arm JOINs through, so they
		// can't leak into search_memory, think, or the recall hook. Mirrors the
		// DeletedAt skip in graph/digest/salience. The graph still receives them
		// via `parsed` so it can emit their (invalidated) edges.
		if m.DeletedAt != "" {
			continue
		}
		if _, err := memStmt.ExecContext(ctx,
			m.ID, m.Scope, m.Type, m.Title, strings.Join(m.Tags, ","), m.Source, m.CreatedAt, path, m.Text); err != nil {
			return count, err
		}
		if _, err := ftsStmt.ExecContext(ctx,
			m.ID, m.Scope, m.Title, strings.Join(m.Tags, ","), m.Source, m.Text); err != nil {
			return count, err
		}
		live = append(live, m)
		count++
	}

	// Materialize the entity graph from the just-indexed memories, in the SAME
	// transaction — a graph failure rolls back the whole index too (atomic).
	if err := writeGraph(ctx, tx, parsed); err != nil {
		return count, err
	}

	// Materialize per-memory embedding vectors (I2 hybrid retrieval), same tx. Use
	// the cfg-aware resolver so a config.toml `embedder = "ollama"` opt-in indexes
	// semantic vectors that the query path (also cfg-aware) will match.
	if err := writeVectors(ctx, tx, chooseEmbedderFor(cfg), live); err != nil {
		return count, err
	}

	// Bind index metadata: memory_count + vault_dir are always written; vault_id
	// only when a marker is present (so legacy vaults without a marker get no row).
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO index_meta(key,value) VALUES('memory_count',?),('vault_dir',?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		fmt.Sprintf("%d", count), cfg.VaultDir); err != nil {
		return count, err
	}
	// Validate-before-commit guard: read the marker, assess the rebuild, and
	// either block (rolling back via the deferred tx.Rollback) or bind the id.
	marker, markerPresent, err := readVaultMarker(cfg)
	if err != nil {
		return count, err
	}
	decision := assessRebuild(oldCount, count, marker.VaultID, markerPresent, indexID)
	if policy == policyEnforce && (decision == decBlockEmpty || decision == decBlockIdentity) {
		if werr := writeBlockRecord(cfg, decision, cfg.VaultDir, oldCount, count); werr != nil {
			_ = werr // best-effort; do not mask the block error
		}
		return count, fmt.Errorf("%w: %s", errRebuildBlocked, rebuildBlockMessage(decision, cfg.VaultDir, oldCount))
	}
	// Bind the vault id (adopt a fresh one if neither marker nor index had it).
	effID := indexID
	if effID == "" {
		seed := marker.VaultID
		if seed == "" {
			seed = "v_" + newID()
		}
		effID, err = createVaultMarkerIfAbsent(cfg, seed)
		if err != nil {
			return count, err
		}
	} else if !markerPresent {
		// The marker was LOST but the index still knows its id (effID==indexID).
		// Without recreating it, the next Enforce rebuild sees markerPresent=false
		// and blocks forever (decBlockIdentity), making --force non-idempotent.
		// Re-stamp the marker bound to the index's own id so identity self-heals.
		if _, err = createVaultMarkerIfAbsent(cfg, effID); err != nil {
			return count, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO index_meta(key,value) VALUES('vault_id',?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, effID); err != nil {
		return count, err
	}
	if err := tx.Commit(); err != nil {
		return count, err
	}
	_ = clearBlockRecord(cfg) // best-effort: a stale block record must not fail a good rebuild
	return count, nil
}

// writeGraph derives the structural entity graph from the indexed memories and
// inserts the entity + edge rows on the given transaction. Empty bi-temporal
// timestamps persist as SQL NULL. Statements are prepared once and reused — a
// real vault is ~10^5 edges, so per-row SQL re-parsing dominated the rebuild.
func writeGraph(ctx context.Context, tx *sql.Tx, mems []Memory) error {
	ents, edges, warnings := buildGraph(mems)
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, w)
	}
	entStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO entities (id, kind, display_name, aliases, mention_count, first_seen, last_seen, salience_micros) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer entStmt.Close()
	for _, e := range ents {
		aliases := e.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		aj, err := json.Marshal(aliases)
		if err != nil {
			return err
		}
		if _, err := entStmt.ExecContext(ctx, e.ID, e.Kind, e.DisplayName, string(aj), e.MentionCount, nullStr(e.FirstSeen), nullStr(e.LastSeen), e.Salience); err != nil {
			return err
		}
	}
	edgeStmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO edges (src, rel, dst, evidence_id, valid_from, valid_to, observed_at, invalidated_at) VALUES (?, ?, ?, ?, ?, NULL, ?, ?)`)
	if err != nil {
		return err
	}
	defer edgeStmt.Close()
	for _, ed := range edges {
		if _, err := edgeStmt.ExecContext(ctx, ed.Src, ed.Rel, ed.Dst, ed.EvidenceID, nullStr(ed.ValidFrom), nullStr(ed.ObservedAt), nullStr(ed.InvalidatedAt)); err != nil {
			return err
		}
	}
	return nil
}

// writeVectors embeds each memory (title + body) and inserts its vector on the
// given transaction. The embedder is deterministic, so the same vault produces
// byte-identical vectors across rebuilds. Statement prepared once (a real vault is
// ~10^4 memories).
func writeVectors(ctx context.Context, tx *sql.Tx, emb Embedder, mems []Memory) error {
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO mem_vectors (memory_id, dim, model, vec) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range mems {
		vec := emb.Embed(m.Title + "\n" + m.Text)
		if _, err := stmt.ExecContext(ctx, m.ID, emb.Dim(), emb.ModelID(), encodeVec(vec)); err != nil {
			return err
		}
	}
	return nil
}
