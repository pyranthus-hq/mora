package mora

// share_gen.go — Packet H (Gate 2, HEALTH-09/-10 for shares): the subscribed-
// share index is a SECOND derived index, redesigned to GENERATION-PUBLISH so a
// reaped-then-resumed "zombie" import holder can never serve stale/torn content.
//
// An import writes a NEW, immutable, per-run generation under
// gens/gen-<run_id>/ and never mutates a published one; it becomes visible only
// by claiming the next slot in a monotonic commit sequence via an atomic
// os.Link, fenced by the import lease. Because the served corpus and index are
// never mutated in place, a zombie's mid-flight writes land in an abandoned
// generation nobody can resolve, and the only thing left to fence is the single
// commit link. Every serve resolves the highest committed generation and
// verifies the artifact it reads against a per-generation integrity digest
// (read ↔ corpus_digest, search ↔ index_digest), recomputed on every serve.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// shareGenSeqWidth zero-pads commit sequence numbers so lexical max == numeric
// max in a plain directory listing (e.g. "0000000006").
const shareGenSeqWidth = 10

// shareGenRetain is how many superseded generations GC keeps for in-flight
// readers before reclaiming older ones (K).
const shareGenRetain = 3

// shareImportTTL is the import lease's abandonment bound — the fifth threshold
// family (Landmine 13). Generous (10 min vs sourcesLockTTL's 30s) because a real
// decrypt+rebuild of a large share can dominate, so a live-but-slow import must
// not be spuriously reaped. Overridable in tests to force reap windows.
var shareImportTTL = 10 * time.Minute

// shareCommit is one published generation's commit record, linked atomically
// into commits/<seq>. It binds BOTH served artifacts (the corpus the read path
// serves and the index.db the search path serves) to this generation.
type shareCommit struct {
	Seq          int    `json:"seq"`
	Gen          string `json:"gen"`                    // "gen-<run_id>" — the generation dir this seq publishes
	RunID        string `json:"run_id"`                 // == the import lease run_id that committed it
	SourceRev    string `json:"source_rev"`             // the immutable input this gen was built from (git tip sha or bucket version)
	BucketFloor  int    `json:"bucket_floor,omitempty"` // monotonic replay floor inherited by every derivative commit
	BuiltAt      string `json:"built_at"`               // RFC3339 — the sub's freshness clock
	CorpusDigest string `json:"corpus_digest"`          // sha256 over sorted "<hex>  <relpath>" lines of this gen's frozen corpus
	IndexDigest  string `json:"index_digest"`           // sha256 of the checkpointed+closed index.db file
	Count        int    `json:"count"`
}

// ---- layout paths (all under one subscription root, one filesystem) ----

func shareGensDir(cfg Config, name string) string {
	return filepath.Join(shareSubRoot(cfg, name), "gens")
}
func shareGenDir(cfg Config, name, gen string) string {
	return filepath.Join(shareGensDir(cfg, name), gen)
}
func shareGenCorpusDir(cfg Config, name, gen string) string {
	return filepath.Join(shareGenDir(cfg, name, gen), "corpus")
}
func shareGenIndexPath(cfg Config, name, gen string) string {
	return filepath.Join(shareGenDir(cfg, name, gen), "index.db")
}
func shareCommitsDir(cfg Config, name string) string {
	return filepath.Join(shareSubRoot(cfg, name), "commits")
}
func shareCommitPath(cfg Config, name string, seq int) string {
	return filepath.Join(shareCommitsDir(cfg, name), fmt.Sprintf("%0*d", shareGenSeqWidth, seq))
}
func shareAttemptPath(cfg Config, name string) string {
	return filepath.Join(shareSubRoot(cfg, name), "attempt.json")
}
func shareImportLockPath(cfg Config, name string) string {
	return filepath.Join(shareSubRoot(cfg, name), "import.lock")
}
func shareMigratedLatchPath(cfg Config, name string) string {
	return filepath.Join(shareSubRoot(cfg, name), "migrated")
}
func shareFetchDir(cfg Config, name, runID string) string {
	return filepath.Join(shareSubRoot(cfg, name), "fetch-"+runID)
}
func genRunID(gen string) string { return strings.TrimPrefix(gen, "gen-") }

// ---- resolver: serving resolves the highest committed generation ----

// resolvePublishedCommit lists commits/, picks the maximum seq, and returns its
// shareCommit. ok=false when commits/ is empty or absent (fail-closed: nothing
// resolves). An unreadable commits/ dir or an unreadable/corrupt max-seq record
// is an ERROR (fail closed — the sourceHealthUnreadable pattern), never a
// silent healthy-empty. This reads a directory listing, never a mutable
// pointer, so a reader can never observe an "absent pointer" served as healthy.
func resolvePublishedCommit(cfg Config, name string) (shareCommit, bool, error) {
	dir := shareCommitsDir(cfg, name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return shareCommit{}, false, nil
		}
		return shareCommit{}, false, err
	}
	maxSeq := -1
	var maxName string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n, perr := strconv.Atoi(strings.TrimSpace(e.Name()))
		if perr != nil {
			continue // ignore non-numeric debris (e.g. an O_EXCL placeholder mid-claim)
		}
		if n > maxSeq {
			maxSeq, maxName = n, e.Name()
		}
	}
	if maxSeq < 0 {
		return shareCommit{}, false, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, maxName))
	if err != nil {
		return shareCommit{}, false, err
	}
	var c shareCommit
	if err := json.Unmarshal(b, &c); err != nil {
		return shareCommit{}, false, fmt.Errorf("share %q: commit record %s is corrupt: %w", name, maxName, err)
	}
	return c, true, nil
}

// readAllCommits reads every parseable commit record (used by the claim loop and
// GC). Non-numeric or unparseable entries are skipped for the seq scan; a truly
// unreadable directory is an error.
func readAllCommits(cfg Config, name string) ([]shareCommit, error) {
	dir := shareCommitsDir(cfg, name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []shareCommit
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, perr := strconv.Atoi(strings.TrimSpace(e.Name())); perr != nil {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			return nil, rerr
		}
		var c shareCommit
		if json.Unmarshal(b, &c) != nil {
			// A briefly-unreadable placeholder (O_EXCL claim mid-fallback) — skip
			// it for the seq scan; the resolver treats it as not-yet-committed.
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// ---- digests ----

// corpusDigestOf hashes a generation's frozen corpus into its committed content
// identity: sha256 over the sorted "<hex>  <relpath>" lines, relpath being the
// bare "<id>.md" file name (the corpus dir is flat). Mirrors manifestDigestOf.
func corpusDigestOf(corpusDir string) (string, error) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(corpusDir, e.Name()))
		if rerr != nil {
			return "", rerr
		}
		sum := sha256.Sum256(b)
		lines = append(lines, hex.EncodeToString(sum[:])+"  "+e.Name())
	}
	return manifestDigestOf(lines), nil
}

// fileDigestOf returns the sha256 of a whole file (the index.db integrity stamp).
func fileDigestOf(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ---- generation build (crash-durable ordering) ----

// rebuildShareIndexFn is the injectable index-build seam. Production builds the
// per-generation index; tests (T1) override it to fail AFTER the private corpus
// is complete but BEFORE any commit, proving a failed rebuild never links a
// commit and the previous generation keeps serving.
var rebuildShareIndexFn = buildShareGenIndex

type shareIndexBudgetContextKey struct{}

func withShareIndexBudget(ctx context.Context, maxBytes int64) context.Context {
	return context.WithValue(ctx, shareIndexBudgetContextKey{}, maxBytes)
}

func shareIndexBudget(ctx context.Context) (int64, bool) {
	maxBytes, ok := ctx.Value(shareIndexBudgetContextKey{}).(int64)
	return maxBytes, ok && maxBytes >= 0
}

// buildShareGenIndex materializes one generation's immutable index.db in WAL
// mode, checkpoints (TRUNCATE) so no -wal/-shm sidecar holds uncommitted rows,
// closes, and fsyncs the file — the build-before-publish durability the commit
// link depends on.
func buildShareGenIndex(ctx context.Context, indexPath string, mems []Memory) error {
	db, err := sql.Open("sqlite", indexPath+"?_txlock=immediate&_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return err
	}
	if maxBytes, bounded := shareIndexBudget(ctx); bounded {
		var pageSize, currentMax int64
		if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
			_ = db.Close()
			return err
		}
		if err := db.QueryRowContext(ctx, `PRAGMA max_page_count`).Scan(&currentMax); err != nil {
			_ = db.Close()
			return err
		}
		if pageSize <= 0 {
			_ = db.Close()
			return errors.New("share index: SQLite reported an invalid page size")
		}
		maxPages := maxBytes / pageSize
		if maxPages <= 0 {
			_ = db.Close()
			return fmt.Errorf("share index needs at least one %d-byte SQLite page; only %d storage bytes remain", pageSize, maxBytes)
		}
		if maxPages < currentMax {
			var applied int64
			q := fmt.Sprintf(`PRAGMA max_page_count = %d`, maxPages)
			if err := db.QueryRowContext(ctx, q).Scan(&applied); err != nil {
				_ = db.Close()
				return err
			}
			if applied > maxPages {
				_ = db.Close()
				return fmt.Errorf("share index already needs %d SQLite pages; storage budget permits %d", applied, maxPages)
			}
		}
	}
	if err := func() error {
		tx, terr := db.BeginTx(ctx, nil)
		if terr != nil {
			return terr
		}
		defer func() { _ = tx.Rollback() }()
		if err := writeShareIndexRows(ctx, tx, mems); err != nil {
			return err
		}
		return tx.Commit()
	}(); err != nil {
		_ = db.Close()
		return err
	}
	// Fold the WAL back into the main db file and drop the sidecars so the closed
	// file is the whole, self-contained index the digest binds.
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		_ = db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	// fsync the closed file so a committed generation survives a crash.
	f, err := os.OpenFile(indexPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// writeShareIndexRows runs the shared v2 DDL + row inserts inside a caller's
// transaction (used by both the generation builder and heal's re-cut). Same
// memories/memories_fts shape + user_version stamp as the personal index.
func writeShareIndexRows(ctx context.Context, tx *sql.Tx, mems []Memory) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS memories (id TEXT PRIMARY KEY, scope TEXT, type TEXT, title TEXT, tags TEXT, source TEXT, created_at TEXT, path TEXT, text TEXT)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(id, scope, title, tags, source, text)`,
		`DELETE FROM memories`,
		`DELETE FROM memories_fts`,
		fmt.Sprintf(`PRAGMA user_version = %d`, indexSchemaVersion),
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	for _, m := range mems {
		if _, err := tx.ExecContext(ctx, `INSERT INTO memories VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.Scope, m.Type, m.Title, strings.Join(m.Tags, ","), m.Source, m.CreatedAt, m.Path, m.Text); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memories_fts VALUES (?, ?, ?, ?, ?, ?)`,
			m.ID, m.Scope, m.Title, strings.Join(m.Tags, ","), m.Source, m.Text); err != nil {
			return err
		}
	}
	return nil
}

// testHookPostFirstGenCorpusWrite fires (tests only) right after the FIRST
// corpus file of a new generation lands durably — the deterministic seam T2 uses
// to SIGKILL a subprocess after attempt.json is written and the first corpus
// file exists but before the commit link.
var testHookPostFirstGenCorpusWrite func()

// testHookPreCommitClaim fires once before publishShareGeneration's claim loop.
// Nil in production.
var testHookPreCommitClaim func()

// shareCommitSyncDirFn is the durability barrier for the winning commit link.
// Tests wrap it to prove that attempt.json cannot transition to succeeded until
// the commits directory entry is durable. Production always uses syncDir; the
// row-51d mutation is deleting the call at the publish site, not replacing this
// seam.
var shareCommitSyncDirFn = syncDir

// shareAdmitFn is an optional per-write byte admitter: it rejects a corpus write
// that would push the whole-product footprint over the configured limit.
type shareAdmitFn func(nextBytes int64) error

// buildShareGenerationFromEntries writes a new immutable generation (durable
// corpus → index → syncDir(gen) → syncDir(gens)) from an already-decrypted,
// validated entry set and returns the frozen digests. It never touches a
// published generation; nothing observes the generation until the commit link.
func buildShareGenerationFromEntries(ctx context.Context, cfg Config, name, gen string, entries []shareBlobEntry) (corpusDigest, indexDigest string, err error) {
	storage, err := newShareStorageAdmission(cfg, name)
	if err != nil {
		return "", "", err
	}
	return buildShareGenerationBounded(ctx, cfg, name, gen, entries, storage.checkAdditional, storage)
}

func buildShareGenerationBounded(ctx context.Context, cfg Config, name, gen string, entries []shareBlobEntry, admit shareAdmitFn, storage *shareStorageAdmission) (corpusDigest, indexDigest string, err error) {
	corpusDir := shareGenCorpusDir(cfg, name, gen)
	if err := os.MkdirAll(corpusDir, 0o700); err != nil {
		return "", "", err
	}
	// Deterministic order so the "first corpus file" seam (T2) is stable.
	sort.Slice(entries, func(i, j int) bool { return entries[i].mem.ID < entries[j].mem.ID })
	mems := make([]Memory, len(entries))
	for i := range entries {
		dst := filepath.Join(corpusDir, entries[i].mem.ID+".md")
		if admit != nil {
			if aerr := admit(int64(len(entries[i].body))); aerr != nil {
				return "", "", aerr
			}
		}
		if werr := atomicWriteDurable(dst, entries[i].body, 0o644); werr != nil {
			return "", "", werr
		}
		mems[i] = entries[i].mem
		mems[i].Path = dst // the generation's frozen corpus is the served source
		if i == 0 && testHookPostFirstGenCorpusWrite != nil {
			testHookPostFirstGenCorpusWrite()
		}
	}
	indexPath := shareGenIndexPath(cfg, name, gen)
	indexCtx := ctx
	if storage != nil {
		remaining, rerr := storage.remaining()
		if rerr != nil {
			return "", "", rerr
		}
		indexCtx = withShareIndexBudget(ctx, remaining)
	}
	if err := rebuildShareIndexFn(indexCtx, indexPath, mems); err != nil {
		return "", "", err
	}
	// SQLite is closed/checkpointed at this point. Re-count its actual bytes (and
	// every other configured root) before any commit link can make the generation
	// visible. The PRAGMA cap bounds normal growth; this final check also catches
	// WAL/temp overhead and an injected or filesystem-level overshoot.
	if storage != nil {
		if aerr := storage.checkCurrent(); aerr != nil {
			return "", "", aerr
		}
	}
	if indexDigest, err = fileDigestOf(indexPath); err != nil {
		return "", "", err
	}
	if corpusDigest, err = corpusDigestOf(corpusDir); err != nil {
		return "", "", err
	}
	// Both the generation's own entries and its dir entry in the parent gens/
	// must be durable before the gen can be referenced by a commit record.
	if err := syncDir(shareGenDir(cfg, name, gen)); err != nil {
		return "", "", err
	}
	if err := syncDir(shareGensDir(cfg, name)); err != nil {
		return "", "", err
	}
	return corpusDigest, indexDigest, nil
}

// errRollback is returned when a bucket fetch presents a version below the
// durable monotonic replay floor — a replayed older-but-signed envelope.
var errRollback = errors.New("bucket share: replayed version is below the committed anti-rollback floor — refusing")

// claimExclusiveDurable publishes an already-fsynced temp at dest with a
// create-exclusive guarantee (os.Link primary; O_CREATE|O_EXCL placeholder +
// replace-rename fallback on hardlink-unsupported volumes). Returns os.ErrExist
// when someone already claimed dest. Exactly one claimant wins.
func claimExclusiveDurable(temp, dest string) error {
	err := linkPublish(temp, dest)
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrExist) {
		return err
	}
	if !linkUnsupported(err) {
		return err
	}
	// Fallback: claim dest with an O_CREATE|O_EXCL placeholder, then rename our
	// own already-fsynced temp over that placeholder. The placeholder is briefly
	// unreadable JSON, which the resolver classifies not-committed and skips.
	claim, cerr := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if cerr != nil {
		return cerr // EEXIST wraps os.ErrExist here too
	}
	if closeErr := claim.Close(); closeErr != nil {
		return errors.Join(closeErr, os.Remove(dest))
	}
	return renameReplaceWithRetry(temp, dest)
}

// shareCommitParams carries everything the claim loop needs to construct and
// fence a commit record.
type shareCommitParams struct {
	name         string
	runID        string
	gen          string
	sourceRev    string
	corpusDigest string
	indexDigest  string
	count        int
	builtAt      time.Time
	isBucket     bool
	fetched      int // bucket fetched version (0 for git)
	subVersion   int // sub.LastVersion fast-path floor
	// parentFloor is set (>=0) by heal/repair to force floor inheritance; -1
	// means "derive from records" (the normal fetch/subscribe path).
	parentFloor int
}

// shareCommitMaxAttempts bounds the claim retry so a losing peer does not force
// a stale publish.
const shareCommitMaxAttempts = 8

// publishShareGeneration is the ONE fence: a monotonic, lease-fenced atomic
// os.Link seq-claim on commits/<seq>, with bucket anti-rollback binding. It runs
// while holding the import lease; its first act each attempt is the ownership
// re-verify, so a reaped holder aborts rather than publishing over its
// successor. Returns the committed seq on success.
func publishShareGeneration(cfg Config, p shareCommitParams) (int, error) {
	commitsDir := shareCommitsDir(cfg, p.name)
	if err := os.MkdirAll(commitsDir, 0o700); err != nil {
		return 0, err
	}
	// testHookPreCommitClaim (tests only) fires once BEFORE the claim loop — the
	// deterministic seam the zombie replay uses to park a fully-built import right
	// before its ownership re-verify.
	if testHookPreCommitClaim != nil {
		testHookPreCommitClaim()
	}
	for attempt := 0; attempt < shareCommitMaxAttempts; attempt++ {
		// Ownership re-verify: a reaped holder must abort before linking.
		if err := verifyImportLeaseOwner(cfg, p.name, p.runID, time.Now()); err != nil {
			return 0, err
		}
		records, err := readAllCommits(cfg, p.name)
		if err != nil {
			return 0, err
		}
		s := 0
		priorFloor := p.subVersion
		for _, r := range records {
			if r.Seq > s {
				s = r.Seq
			}
			if r.BucketFloor > priorFloor {
				priorFloor = r.BucketFloor
			}
		}
		// Bucket anti-rollback: reject a replayed older version BEFORE building/
		// claiming over a newer committed version.
		if p.isBucket && p.fetched < priorFloor {
			return 0, errRollback
		}
		nextFloor := priorFloor
		if p.parentFloor >= 0 {
			// A repair must name the exact floor of the published parent it read.
			// Do not silently recover a missing/zeroed call-site value from older
			// records: that would make heal's inheritance contract redundant and a
			// later GC could erase the only durable replay floor. Fail closed if the
			// caller tries to publish below any floor already on record.
			if p.parentFloor < priorFloor {
				return 0, fmt.Errorf("share %q: repair floor %d is below committed floor %d", p.name, p.parentFloor, priorFloor)
			}
			nextFloor = p.parentFloor
		}
		if p.isBucket && p.fetched > nextFloor {
			nextFloor = p.fetched
		}
		seq := s + 1
		rec := shareCommit{
			Seq: seq, Gen: p.gen, RunID: p.runID, SourceRev: p.sourceRev,
			BucketFloor: nextFloor, BuiltAt: p.builtAt.UTC().Format(time.RFC3339),
			CorpusDigest: p.corpusDigest, IndexDigest: p.indexDigest, Count: p.count,
		}
		body, merr := json.MarshalIndent(rec, "", "  ")
		if merr != nil {
			return 0, merr
		}
		temp, terr := writeCommitTemp(commitsDir, append(body, '\n'))
		if terr != nil {
			return 0, terr
		}
		claimErr := claimExclusiveDurable(temp, shareCommitPath(cfg, p.name, seq))
		if claimErr == nil {
			_ = os.Remove(temp)
			if err := shareCommitSyncDirFn(commitsDir); err != nil {
				return 0, err
			}
			return seq, nil // WON
		}
		_ = os.Remove(temp)
		if errors.Is(claimErr, os.ErrExist) {
			continue // someone claimed this seq; re-verify ownership at the top
		}
		if sharingViolationRetryable(claimErr) {
			continue
		}
		return 0, claimErr
	}
	return 0, fmt.Errorf("share %q: commit sequence contended — a peer import is winning; not forcing a stale publish", p.name)
}

// writeCommitTemp writes body to a unique same-dir temp, fsyncs, and closes it
// before it is linked (preserving the lock publisher's close-before-link
// discipline so no open handle races os.Link on Windows).
func writeCommitTemp(dir string, body []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".commit-*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if _, err := f.Write(body); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}
