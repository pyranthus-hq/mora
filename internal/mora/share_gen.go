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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	sharingpkg "github.com/pyranthus-hq/mora/internal/sharing"
)

const (
	shareGenSeqWidth = sharingpkg.GenSeqWidth
	shareGenRetain   = sharingpkg.GenRetain
)

// shareImportTTL is the import lease abandonment bound; orchestration owns it.
var shareImportTTL = 10 * time.Minute

type shareCommit = sharingpkg.Commit

func shareGenerationStore(cfg Config) sharingpkg.GenerationStore {
	return sharingpkg.GenerationStore{DataDir: cfg.DataDir}
}
func shareGensDir(cfg Config, name string) string { return shareGenerationStore(cfg).GensDir(name) }
func shareGenDir(cfg Config, name, gen string) string {
	return shareGenerationStore(cfg).GenDir(name, gen)
}
func shareGenCorpusDir(cfg Config, name, gen string) string {
	return shareGenerationStore(cfg).CorpusDir(name, gen)
}
func shareGenIndexPath(cfg Config, name, gen string) string {
	return shareGenerationStore(cfg).IndexPath(name, gen)
}
func shareCommitsDir(cfg Config, name string) string {
	return shareGenerationStore(cfg).CommitsDir(name)
}
func shareCommitPath(cfg Config, name string, seq int) string {
	return shareGenerationStore(cfg).CommitPath(name, seq)
}
func shareAttemptPath(cfg Config, name string) string {
	return shareGenerationStore(cfg).AttemptPath(name)
}
func shareImportLockPath(cfg Config, name string) string {
	return shareGenerationStore(cfg).ImportLockPath(name)
}
func shareMigratedLatchPath(cfg Config, name string) string {
	return shareGenerationStore(cfg).MigratedLatchPath(name)
}
func shareFetchDir(cfg Config, name, runID string) string {
	return shareGenerationStore(cfg).FetchDir(name, runID)
}
func genRunID(gen string) string { return sharingpkg.RunID(gen) }
func resolvePublishedCommit(cfg Config, name string) (shareCommit, bool, error) {
	return shareGenerationStore(cfg).Resolve(name)
}
func readAllCommits(cfg Config, name string) ([]shareCommit, error) {
	return shareGenerationStore(cfg).ReadAll(name)
}
func corpusDigestOf(corpusDir string) (string, error) { return sharingpkg.CorpusDigest(corpusDir) }
func fileDigestOf(path string) (string, error)        { return sharingpkg.FileDigest(path) }

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

// writeShareIndexRows runs the current share-search DDL + row inserts inside a
// caller's transaction (used by both the generation builder and heal's re-cut).
// Shares expose the memories/FTS subset and carry the common user_version stamp.
func writeShareIndexRows(ctx context.Context, tx *sql.Tx, mems []Memory) error {
	// provider/account/created_at_unix (v4, #241): same additive columns as
	// the main index (index.go), populated the SAME way (canonicalized
	// provider, Unix-seconds created_at) so searchShareIndex (share.go) can
	// apply the SAME SQL-predicate pre-rank filtering the local arms use — a
	// share index is a derived, rebuildable cache too, and every write path
	// (generation + heal) routes through this one function, so there is no
	// separate idempotent-ALTER migration to carry: a share index is always
	// fully regenerated (DELETE + reinsert), never patched.
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS memories (id TEXT PRIMARY KEY, scope TEXT, type TEXT, title TEXT, tags TEXT, source TEXT, created_at TEXT, path TEXT, text TEXT, provider TEXT, account TEXT, created_at_unix INTEGER)`,
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO memories VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.Scope, m.Type, m.Title, strings.Join(m.Tags, ","), m.Source, m.CreatedAt, m.Path, m.Text,
			providerToType(m.Provider), m.Account, createdAtUnix(m.CreatedAt)); err != nil {
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
var shareCommitSyncDirFn = atomicio.SyncDir

// shareAdmitFn is an optional per-write byte admitter: it rejects a corpus write
// that would push the whole-product footprint over the configured limit.
type shareAdmitFn func(nextBytes int64) error

// buildShareGenerationFromEntries writes a new immutable generation (durable
// corpus → index → atomicio.SyncDir(gen) → atomicio.SyncDir(gens)) from an already-decrypted,
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
		if werr := atomicio.WriteDurable(dst, entries[i].body, 0o644); werr != nil {
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
	if err := atomicio.SyncDir(shareGenDir(cfg, name, gen)); err != nil {
		return "", "", err
	}
	if err := atomicio.SyncDir(shareGensDir(cfg, name)); err != nil {
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
	return atomicio.ClaimExclusiveDurable(temp, dest, atomicio.ClaimOptions{
		Link: linkPublish, Unsupported: linkUnsupported,
	})
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
		// Ownership re-verify: a reaped holder must abort before linking. The
		// aggregate storage lease is a second fence: without it, a long build whose
		// reservation was reaped could publish after another subscription admitted
		// against the same headroom.
		if err := verifyStorageLeaseOwner(cfg, p.runID, time.Now()); err != nil {
			return 0, err
		}
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
		if atomicio.SharingViolationRetryable(claimErr) {
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
