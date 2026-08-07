package mora

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func cmdIndex(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) (err error) {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "rebuild even if the vault looks empty or unfamiliar")
	if perr := fs.Parse(flagsFirst(args)); perr != nil {
		return perr
	}
	if fs.NArg() != 1 || fs.Arg(0) != "rebuild" {
		return errors.New("usage: mora index rebuild [--force]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// index-hourly producer chokepoint (HEALTH-11): stamp the rebuild's own outcome.
	defer stampChokepoint(cfg, stdout, args, "index-hourly", producerClock(), &err)
	policy := policyEnforce
	if *force {
		policy = policyAllow
	}
	count, err := rebuildIndexWithPolicy(ctx, cfg, policy)
	if err != nil {
		return err
	}
	// vault/index.md is refreshed inside rebuildIndexWithPolicy's commit path now
	// (B5), so every rebuild caller keeps it honest — not just this CLI one.
	fmt.Fprintf(stdout, "indexed %d memories\n", count)
	return nil
}
func dbPath(cfg Config) string { return filepath.Join(cfg.DataDir, "index.db") }

// roIndexDSN is the DSN every read-only index open uses. Writers persist WAL mode;
// readers only need to use that committed mode, not set it again. A hierarchical
// file URI makes modernc enforce mode=ro instead of silently opening the bare-path
// spelling read-write. When no live WAL exists, immutable=1 is safe for this
// per-request snapshot and lets sandboxed agents read index.db without permission
// to create WAL/SHM sidecars. A live WAL keeps ordinary mode=ro so its committed
// rows remain visible. query_only is a second fail-closed guard; busy_timeout still
// covers a live writer's short lock window.
func roIndexDSN(cfg Config) string {
	path := filepath.ToSlash(dbPath(cfg))
	if filepath.VolumeName(path) != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := &url.URL{Scheme: "file", Path: path}
	dsn := u.String() + "?mode=ro&_pragma=busy_timeout(15000)&_pragma=query_only(1)"
	if info, err := os.Stat(dbPath(cfg) + "-wal"); errors.Is(err, os.ErrNotExist) || (err == nil && info.Size() == 0) {
		dsn += "&immutable=1"
	}
	return dsn
}

// rwIndexDSN is the DSN every WRITER of the live index (full rebuild + incremental
// upsert) uses. Two pragmas carry the concurrency contract:
//   - _txlock=immediate grabs the writer lock at BeginTx (not lazily mid-tx), so
//     two concurrent rebuilds serialize instead of both starting and one hitting an
//     un-retryable SQLITE_BUSY inside an open transaction.
//   - journal_mode(WAL) is what lets N long-lived `mora mcp serve` READER processes
//     coexist with a writer. In the default rollback journal a writer's EXCLUSIVE
//     lock is incompatible with every reader's SHARED lock, so under real
//     multi-process load (each agent session holds one mcp serve) a write waits for
//     ALL readers, blows past busy_timeout, and surfaces "database is locked". In
//     WAL readers and the single writer never block each other. WAL persists in the
//     db header, so the first open of THIS or the RO DSN converts a legacy
//     delete-mode index in place; thereafter the pragma is a no-op. modernc opens
//     even mode=ro connections read-write, so a reader can create the -wal/-shm
//     sidecars — there is no read-only-WAL breakage here.
func rwIndexDSN(cfg Config) string {
	return dbPath(cfg) + "?_txlock=immediate&_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)"
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

// indexUpsertSchemaComplete is the physical readiness probe for the
// incremental-write boundary. It deliberately verifies the union of D and E's
// schema changes: the legacy readiness contract already required memories,
// memories_fts, and index_meta; v5 adds D's three memories columns and E's
// three segment tables. Version fencing still protects ordinary read opens.
func indexUpsertSchemaComplete(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN
		 ('memories','memories_fts','index_meta','gmail_segments','gmail_segments_fts','gmail_segment_diagnostics')`).Scan(&n); err != nil {
		return false, err
	}
	if n != 6 {
		return false, nil
	}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(memories)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return columns["provider"] && columns["account"] && columns["created_at_unix"], nil
}

func rebuildIndex(ctx context.Context, cfg Config) (int, error) {
	return rebuildIndexWithPolicy(ctx, cfg, policyEnforce)
}
func rebuildIndexWithPolicy(ctx context.Context, cfg Config, policy rebuildPolicy) (count int, err error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return 0, err
	}
	// A4 — the rebuild marks ITSELF before opening the writer tx. A rebuild that
	// fails, is killed, or is blocked leaves this op, so the index reads dirty and
	// every surface reddens, while the prior committed index is preserved (the
	// deferred tx.Rollback below). Its own op is cleared by the covering commit
	// (rule a: marked_at <= listing_started_at). A no-op on a cold-start index
	// (indexReadyForUpsert==false): the state is already `never`, worse than dirty.
	selfOp, _ := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindRebuild})
	_ = selfOp
	// On ANY failed return, best-effort stamp the reason in a separate tx (after the
	// writer lock below is released — this defer is registered first, so LIFO runs
	// it last). The pending op is deliberately NOT cleared on failure.
	defer func() {
		if err != nil {
			stampIndexAttemptFailure(cfg, err)
		}
	}()
	// HEALTH-12 / Packet D2 — the ONE fail-closed embedder gate, for ALL 18 rebuild
	// triggers (they every one funnel through here, index.go's rebuildIndexWithPolicy;
	// a gate in cmdIndex alone would close none of them). Resolve BEFORE BeginTx so a
	// doomed rebuild never takes the writer lock — a latency choice, not a second
	// correctness gate. An unreachable configured `ollama` embedder returns
	// errEmbedderUnavailable and this HARD-FAILS: the self-op above stays (index reads
	// dirty), the previous vectors are untouched (no tx was ever opened), and NOTHING
	// is silently re-embedded with the static fallback (the recorded incident).
	emb, err := chooseEmbedderFor(cfg)
	if err != nil {
		return 0, err
	}
	// _txlock=immediate acquires the write lock at BeginTx instead of lazily
	// upgrading a deferred read lock mid-transaction — two concurrent rebuilds
	// would otherwise both start, then one hits SQLITE_BUSY on the first write and
	// cannot retry inside an open tx. The 15s busy_timeout matches the RO DSN so a
	// rebuild waits out a contending writer rather than failing fast.
	db, err := sql.Open("sqlite", rwIndexDSN(cfg))
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
	// Snapshot the wall clock the instant BEFORE listing (A3): a pending op whose
	// marked_at is at or before this instant is demonstrably covered by this
	// rebuild's listing; one marked AFTER it raced in and is NOT cleared here.
	listingStartedAt := indexClock()
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
		// provider/account/created_at_unix (v4, #241): indexed so search.go/
		// hybrid.go/share.go's retrieval arms can filter by source/time
		// INSIDE SQL, before ORDER BY/LIMIT — a TRUE pre-rank predicate against
		// the SAME row the arm is already ranking, never a live vault-file
		// reopen or a lexical CreatedAt string compare mid-ranking. provider is
		// stored CANONICALIZED (providerToType(m.Provider), the same alias
		// sourceInstanceKey applies) so a query-time family filter is an exact
		// `=` match with no per-row alias resolution; created_at_unix is
		// CreatedAt parsed as an RFC3339 instant at write time (createdAtUnix,
		// search_filters.go), so a since_hours cutoff is a plain integer `>=`
		// compare, never string comparison. A FRESH db gets all three from this
		// CREATE; an EXISTING pre-column db gets them from the additive ALTERs
		// below (CREATE TABLE IF NOT EXISTS is a no-op once the table exists).
		`CREATE TABLE IF NOT EXISTS memories (id TEXT PRIMARY KEY, scope TEXT, type TEXT, title TEXT, tags TEXT, source TEXT, created_at TEXT, path TEXT, text TEXT, provider TEXT, account TEXT, created_at_unix INTEGER)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(id, scope, title, tags, source, text)`,
		// Entity graph (I1): rebuildable from the vault Markdown, never the only home of state.
		// salience_micros (Phase 14): the frozen person-ranking sort key. A FRESH db gets it
		// from this CREATE; an EXISTING pre-column db gets it from the additive ALTER below
		// (CREATE TABLE IF NOT EXISTS is a no-op once the table exists, so it can't add it).
		`CREATE TABLE IF NOT EXISTS entities (id TEXT PRIMARY KEY, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER, first_seen TEXT, last_seen TEXT, salience_micros INTEGER)`,
		`CREATE TABLE IF NOT EXISTS edges (src TEXT, rel TEXT, dst TEXT, evidence_id TEXT, valid_from TEXT, valid_to TEXT, observed_at TEXT, invalidated_at TEXT, PRIMARY KEY (src, rel, dst, evidence_id))`,
		// person_merges (P13): the provenance of every applied identity fusion — which
		// two SOURCE person ids were merged, and by which signal (same-mailbox /
		// name-echo / confirmed). A dedicated table (not an edge) so merged-away member
		// ids can never leak into get_entity's evidence/neighbor derivation. Rebuildable.
		`CREATE TABLE IF NOT EXISTS person_merges (member_a TEXT, member_b TEXT, signal TEXT, detail TEXT, PRIMARY KEY (member_a, member_b, signal))`,
		`CREATE INDEX IF NOT EXISTS idx_edges_src ON edges(src)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_dst ON edges(dst)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_rel ON edges(rel)`,
		`CREATE INDEX IF NOT EXISTS idx_entities_kind ON entities(kind)`,
		// Hybrid retrieval (I2): one static-embedding vector per memory, rebuildable.
		`CREATE TABLE IF NOT EXISTS mem_vectors (memory_id TEXT PRIMARY KEY, dim INT, model TEXT, vec BLOB)`,
		// Typed commitments (Gate 3): a whole-vault, generation-stamped projection.
		// row_key is the CommitmentID whenever immutable opening refs exist. Legacy
		// pre-PR1 memories retain an internal row key but never receive a fabricated
		// commitment_id.
		`CREATE TABLE IF NOT EXISTS commitments (
			generation TEXT NOT NULL,
			row_key TEXT PRIMARY KEY,
			commitment_id TEXT UNIQUE,
			memory_id TEXT NOT NULL,
			payload TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_commitments_generation_memory ON commitments(generation, memory_id)`,
		// index_meta: vault-binding key/value store. Rows are rewritten by upsert on
		// each rebuild; deliberately NOT in the DELETE list so vault_id persists
		// across rebuilds and the guard can compare it.
		`CREATE TABLE IF NOT EXISTS index_meta (key TEXT PRIMARY KEY, value TEXT)`,
		`DELETE FROM memories`,
		`DELETE FROM memories_fts`,
		`DELETE FROM entities`,
		`DELETE FROM edges`,
		`DELETE FROM person_merges`,
		`DELETE FROM mem_vectors`,
		`DELETE FROM commitments`,
		// Stamp the schema this binary writes (read paths refuse a mismatch).
		// Inside the same tx as everything else: a rolled-back rebuild must not
		// leave a fresh stamp on a stale index.
		fmt.Sprintf(`PRAGMA user_version = %d`, indexSchemaVersion),
	}
	// Issue #243 — the disposable Gmail evidence-segment projection: created
	// and cleared inside this SAME transaction as every other derived table
	// (frozen interface #1).
	stmts = append(stmts, gmailSegSchemaStmts...)
	stmts = append(stmts, gmailSegDeleteStmts...)
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
	// v4 (#241): same idempotent-ALTER migration for memories.provider/account/
	// created_at_unix — tolerated as a no-op ("duplicate column name") on a
	// fresh db whose CREATE above already has them.
	for _, col := range []string{
		`ALTER TABLE memories ADD COLUMN provider TEXT`,
		`ALTER TABLE memories ADD COLUMN account TEXT`,
		`ALTER TABLE memories ADD COLUMN created_at_unix INTEGER`,
	} {
		if _, err := tx.ExecContext(ctx, col); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return 0, err
		}
	}
	memStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO memories VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer memStmt.Close()
	ftsStmt, err := tx.PrepareContext(ctx, `INSERT INTO memories_fts VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer ftsStmt.Close()
	// Issue #243 — prepared once, reused for every live memory below, mirroring
	// memStmt/ftsStmt's own prepare-once discipline.
	gsegStmts, err := prepareGmailSegStmts(ctx, tx)
	if err != nil {
		return 0, err
	}
	defer gsegStmts.Close()
	count = 0
	var parsed []Memory // ALL memories (incl. tombstones) — feeds the graph
	var live []Memory   // non-tombstoned only — the searchable corpus + vectors
	parsedCount := 0    // includes governed historical revisions; only parse failures count as unparseable
	governance, err := loadGovernance(cfg)
	if err != nil {
		return 0, err
	}
	// Content manifest (B1a): one sha256 line per readable file, from the SAME bytes
	// the parse uses (parseMemoryBytes) — zero extra I/O. An unreadable file is
	// skipped here and counts toward `unparseable` (listed − parsed) below.
	var manifestLines []string
	for _, path := range files {
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		manifestLines = append(manifestLines, manifestLine(cfg, path, sha256.Sum256(b)))
		m, err := parseMemoryBytes(path, b)
		if err != nil {
			continue
		}
		parsedCount++
		// Teach keeps superseded/retracted authored Markdown as auditable evidence
		// while excluding it from the current index, graph, vectors, and
		// commitments. Undo changes only the ledger; the next rebuild restores it.
		if !governance.memoryVisible(m.ID) {
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
		// #241: provider canonicalized (providerToType) and CreatedAt parsed to
		// Unix seconds ONCE here, at write time — the arms compare against
		// these precomputed columns, never a live file or a lexical string.
		if _, err := memStmt.ExecContext(ctx,
			m.ID, m.Scope, m.Type, m.Title, strings.Join(m.Tags, ","), m.Source, m.CreatedAt, path, m.Text,
			providerToType(m.Provider), m.Account, createdAtUnix(m.CreatedAt)); err != nil {
			return count, err
		}
		if _, err := ftsStmt.ExecContext(ctx,
			m.ID, m.Scope, m.Title, strings.Join(m.Tags, ","), m.Source, m.Text); err != nil {
			return count, err
		}
		// Issue #243 — derive this memory's evidence-segment projection (or
		// its fail-closed diagnostic) in the SAME transaction, over the SAME
		// live corpus memories/memories_fts just indexed.
		if err := writeGmailSegments(ctx, gsegStmts, m); err != nil {
			return count, err
		}
		live = append(live, m)
		count++
	}

	// Materialize the entity graph from the just-indexed memories, in the SAME
	// transaction — a graph failure rolls back the whole index too (atomic). cfg
	// carries the vault dir so the graph can apply the governance ledger's confirmed
	// cross-channel merges (a corrupt ledger fails the rebuild loud, never silently).
	if err := writeGraph(ctx, tx, cfg, parsed); err != nil {
		return count, err
	}

	// Materialize per-memory embedding vectors (I2 hybrid retrieval), same tx. `emb`
	// was resolved (and fail-closed-checked) BEFORE BeginTx above, so a config.toml
	// `embedder = "ollama"` opt-in indexes semantic vectors the query path will match,
	// and a mid-rebuild daemon death now surfaces as a real error from Embed (rolling
	// this whole tx back) instead of committing zero vectors.
	if err := writeVectors(ctx, tx, emb, live); err != nil {
		return count, err
	}

	// Commitments are derived over the exact same whole-vault snapshot as the graph
	// and vectors. Their generation also binds the injected rebuild instant and
	// source-health snapshot because state_uncertain is a material input: two
	// different health snapshots must never share one generation id.
	stampNow := indexClock().UTC()
	commitmentGeneration := commitmentGenerationOf(manifestLines, cfg, stampNow)
	if err := writeCommitments(ctx, tx, commitmentGeneration, parsed, cfg, stampNow); err != nil {
		return count, err
	}

	// Gate 2 stamps, all INSIDE this committing tx so they advance ONLY on a
	// committed rebuild (never on a rolled-back one):
	//   - the three projection timestamps (Finding 2): a full rebuild advances all
	//     three; indexUpsert advances only fts_indexed_at, so their relation is the
	//     honest graph-lag signal (B1 rule 6).
	//   - MINIMAL embedder provenance (B1 rule 5 / HEALTH-12 mismatch arm): what
	//     RAN, so doctor can flag it against what the config now ASKS for.
	//   - the content manifest (B1a): the committed vault content identity an
	//     out-of-band edit is detected against.
	//   - the SEMANTIC embedder_digest (D3): the resolved Ollama model digest, "" for
	//     the static floor. embedder_model/embedder_dim are the MINIMAL provenance PR 1
	//     already writes; this adds the digest column so a later `ollama pull` that
	//     re-resolves the same model NAME to new weights is a detectable mismatch.
	stampNowText := stampNow.Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO index_meta(key,value) VALUES
		 ('indexed_at',?),('fts_indexed_at',?),('graph_indexed_at',?),('vectors_indexed_at',?),
		 ('commitments_generation',?),
		 ('embedder_model',?),('embedder_dim',?),('embedder_digest',?),
		 ('vault_manifest_algo',?),('vault_manifest_digest',?),('vault_manifest_listed',?),('vault_manifest_unparseable',?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		stampNowText, stampNowText, stampNowText, stampNowText, commitmentGeneration,
		emb.ModelID(), fmt.Sprintf("%d", emb.Dim()), embedderDigestOf(emb),
		indexManifestAlgo, manifestDigestOf(manifestLines),
		fmt.Sprintf("%d", len(files)), fmt.Sprintf("%d", len(files)-parsedCount)); err != nil {
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
	// A full rebuild reinserts the entire index into the WAL in one transaction, so
	// the -wal file is now ~db-sized. Fold it back into index.db and reset the -wal
	// to keep it from staying huge between the periodic auto-checkpoints. Best-effort:
	// a busy checkpoint (a reader still attached) is harmless — auto-checkpoint will
	// catch up — and must never fail a committed rebuild.
	_, _ = db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	_ = clearBlockRecord(cfg) // best-effort: a stale block record must not fail a good rebuild

	// A3 — retire the pending ops this committed rebuild demonstrably covered, and
	// truncate the ingest journal lines whose file it listed. Best-effort: a failed
	// removal only leaves a false-dirty the next rebuild clears (never a false-clean).
	clearCoveredPendingOps(cfg, listingStartedAt, files, memoryPaths(parsed))
	recoverIngestJournals(cfg, files)

	// B5 — refresh vault/index.md from the SAME stamp written into index_meta, so
	// the page buildContext injects into every context payload cannot disagree with
	// the index it describes. Best-effort: a cosmetic derived file must not undo a
	// committed rebuild (and returning here would spuriously fire the failure stamp).
	if werr := writeWikiIndex(cfg, count, stampNowText); werr != nil {
		fmt.Fprintf(os.Stderr, "warn: could not refresh vault/index.md: %v\n", werr)
	}
	return count, nil
}

// writeGraph derives the structural entity graph from the indexed memories and
// inserts the entity + edge rows on the given transaction. Empty bi-temporal
// timestamps persist as SQL NULL. Statements are prepared once and reused — a
// real vault is ~10^5 edges, so per-row SQL re-parsing dominated the rebuild.
func writeGraph(ctx context.Context, tx *sql.Tx, cfg Config, mems []Memory) error {
	// Resolve the confirm-queue's confirmed cross-channel merges (RULE 3). An absent
	// ledger is the common case (no confirms); a corrupt one fails loud so a rebuild
	// can never silently drop a user's confirmed unification.
	g, err := loadGovernance(cfg)
	if err != nil {
		return err
	}
	confirmed, _ := g.mergeDecisions()
	res := buildGraphResult(mems, confirmed)
	ents, edges, warnings := res.entities, res.edges, res.warnings
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
	// Merge provenance (P13): one row per applied fusion, so "why is X the same as Y"
	// is durable and auditable (feeds the trust model). Deterministic, so it keeps the
	// rebuild byte-identical for a fixed vault + ledger.
	mergeStmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO person_merges (member_a, member_b, signal, detail) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer mergeStmt.Close()
	for _, m := range res.merges {
		if _, err := mergeStmt.ExecContext(ctx, m.A, m.B, m.Signal, m.Detail); err != nil {
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
		// A failed Embed aborts the rebuild (rolled back by the caller's deferred
		// tx.Rollback). It NEVER falls through to persist a zero/substitute vector —
		// HEALTH-12: a daemon that dies mid-rebuild must not commit degraded vectors
		// stamped with the real model id while the rebuild exits 0.
		vec, err := emb.Embed(m.Title + "\n" + m.Text)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, m.ID, emb.Dim(), emb.ModelID(), encodeVec(vec)); err != nil {
			return err
		}
	}
	return nil
}

// embedderDigestOf extracts the semantic model digest an embedder carries (D3),
// via an optional Digest() method so the Embedder interface stays minimal. The
// static floor has no digest and returns "".
func embedderDigestOf(emb Embedder) string {
	if d, ok := emb.(interface{ Digest() string }); ok {
		return d.Digest()
	}
	return ""
}
