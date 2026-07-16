package mora

// share_gen_test.go — Packet H incident/acceptance witnesses (in-process). The
// two-process zombie/interrupt/storm replays live in share_subprocess_test.go.
// Each mutation named in the comments reddens exactly its test.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

// publishGen rewrites the subscription's fixture repo to `mems` and commits a new
// immutable generation for it (the serving-layer analogue of a pull).
func publishGen(t *testing.T, cfg Config, name string, id *age.X25519Identity, mems []Memory) shareImportStats {
	t.Helper()
	// A real pull rebuilds from the immutable current input; clear the fixture's
	// memories dir so a revoked memory does not linger from a prior generation.
	_ = os.RemoveAll(filepath.Join(shareRepoDir(cfg, name), "memories"))
	buildShareRepoFixture(t, shareRepoDir(cfg, name), id.Recipient(), mems, true)
	st, err := importFixtureGeneration(context.Background(), cfg, shareSubscription{Name: name, Remote: "r"}, shareRepoDir(cfg, name))
	if err != nil {
		t.Fatalf("publishGen(%q): %v", name, err)
	}
	return st
}

// acquirePublishLeases mirrors shareBuildAndPublish's one legal lock order for
// focused tests that drive publishShareGeneration directly.
func acquirePublishLeases(t *testing.T, cfg Config, name, runID string) func() {
	t.Helper()
	storageRelease, err := acquireStorageLease(cfg, runID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	importRelease, err := acquireImportLease(cfg, name, runID, time.Now())
	if err != nil {
		storageRelease()
		t.Fatal(err)
	}
	return func() {
		importRelease()
		storageRelease()
	}
}

// registerSub adds a subscription row so the serving surfaces see it.
func registerSub(t *testing.T, cfg Config, name string) {
	t.Helper()
	sf, err := loadShares(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sf.Subscriptions {
		if s.Name == name {
			return
		}
	}
	sf.Subscriptions = append(sf.Subscriptions, shareSubscription{Name: name, Remote: "r", CreatedAt: "2026-07-01T00:00:00Z"})
	if err := saveShares(cfg, sf); err != nil {
		t.Fatal(err)
	}
}

// T5 (row 39): serving resolves the HIGHEST committed generation.
func TestOnlyHighestCommittedGenerationIsServed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")

	keep := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Kept", "still valid")
	revoked := fixtureMemory("mem_20260601_000001_bbbbbbbb", "Revoked", "secret to revoke")
	publishGen(t, cfg, "neil", id, []Memory{keep, revoked}) // seq 1
	publishGen(t, cfg, "neil", id, []Memory{keep})          // seq 2 revokes bbbb

	// read + search both resolve the newest generation (revoked memory absent).
	if _, ok := findSharedMemory(cfg, revoked.ID); ok {
		t.Fatal("read served the revoked memory from an old generation")
	}
	if _, ok := findSharedMemory(cfg, keep.ID); !ok {
		t.Fatal("read did not serve the kept memory from the newest generation")
	}
	res, err := searchSharedCorpora(context.Background(), cfg, "revoke", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, corpus := range res {
		for _, m := range corpus {
			if m.ID == revoked.ID {
				t.Fatal("search served the revoked memory from an old generation")
			}
		}
	}
	// Resolver returns the max seq.
	c, ok, _ := resolvePublishedCommit(cfg, "neil")
	if !ok || c.Seq != 2 {
		t.Fatalf("resolved seq %d (ok=%v); want 2", c.Seq, ok)
	}
}

// T1 (composite): a failed refresh (rebuild errors after the private corpus is
// complete, before any commit) serves the PREVIOUS complete committed generation
// and links no new commit. Mutation (rebuildShareIndexFn): if the failed rebuild
// still committed, the revoked/torn state would serve.
func TestFailedShareRefreshServesUntornLastGoodGeneration(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")

	a := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Alpha", "one")
	b := fixtureMemory("mem_20260601_000001_bbbbbbbb", "Beta", "two")
	publishGen(t, cfg, "neil", id, []Memory{a, b}) // last-good seq 1
	firstCommit, _, _ := resolvePublishedCommit(cfg, "neil")

	// Publisher drops b; the refresh's index build fails after the private corpus
	// is written but before any commit.
	buildShareRepoFixture(t, shareRepoDir(cfg, "neil"), id.Recipient(), []Memory{a}, true)
	orig := rebuildShareIndexFn
	rebuildShareIndexFn = func(ctx context.Context, path string, mems []Memory) error {
		return errInjectedRebuild
	}
	_, err := importFixtureGeneration(context.Background(), cfg, shareSubscription{Name: "neil", Remote: "r"}, shareRepoDir(cfg, "neil"))
	rebuildShareIndexFn = orig
	if err == nil {
		t.Fatal("failed refresh did not error")
	}
	// No new commit linked; the previous generation still resolves and still
	// serves BOTH memories (explicit surfaced safe-staleness).
	c, ok, _ := resolvePublishedCommit(cfg, "neil")
	if !ok || c.Seq != firstCommit.Seq {
		t.Fatalf("a failed refresh advanced the commit seq to %d; want %d", c.Seq, firstCommit.Seq)
	}
	if _, ok := findSharedMemory(cfg, b.ID); !ok {
		t.Fatal("last-good generation stopped serving after a failed refresh (torn state)")
	}
}

var errInjectedRebuild = injectedErr("injected rebuild failure")

type injectedErr string

func (e injectedErr) Error() string { return string(e) }

// T3 (TestNeverPulledShareIsNever): no commits, no legacy, no latch → never.
func TestNeverPulledShareIsNever(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	registerSub(t, cfg, "neil")
	if err := os.MkdirAll(shareSubRoot(cfg, "neil"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := shareHealthOne(cfg, "neil", time.Now())
	if h.State != healthNever {
		t.Fatalf("never-pulled share state = %q; want never", h.State)
	}
}

// T7 (rows 41a/b): an uncommitted / half-built generation is never served.
func TestUncommittedGenerationNeverServed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")
	publishGen(t, cfg, "neil", id, []Memory{fixtureMemory("mem_20260601_000000_aaaaaaaa", "A", "one")})

	// Hand-craft an uncommitted generation dir holding a would-be revoked memory.
	ghost := "gen-ghostrun"
	gc := shareGenCorpusDir(cfg, "neil", ghost)
	if err := os.MkdirAll(gc, 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := renderMemory(fixtureMemory("mem_20260601_000009_cccccccc", "Ghost", "never committed"))
	if err := os.WriteFile(filepath.Join(gc, "mem_20260601_000009_cccccccc.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	// The resolver enumerates commits/, not gens/, so the ghost is invisible.
	if _, ok := findSharedMemory(cfg, "mem_20260601_000009_cccccccc"); ok {
		t.Fatal("an uncommitted generation was served on read")
	}
	c, ok, _ := resolvePublishedCommit(cfg, "neil")
	if !ok || c.Gen == ghost {
		t.Fatalf("resolver picked the uncommitted gen %q", c.Gen)
	}
}

// T13 (row 47a): corpus corruption fails read closed (never altered bytes) while
// search keeps serving its intact index; a positive digest is never cached.
func TestCorruptedPublishedCorpusFailsClosedOnRead(t *testing.T) {
	t.Run("no_check", testCorruptedPublishedCorpusFailsClosedOnRead)
}

func testCorruptedPublishedCorpusFailsClosedOnRead(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")
	m := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Decision", "we chose age encryption")
	publishGen(t, cfg, "neil", id, []Memory{m})

	// Warm any cache with a clean read first.
	if _, ok := findSharedMemory(cfg, m.ID); !ok {
		t.Fatal("clean read failed")
	}
	// Flip a byte in the frozen corpus file (on-disk corruption).
	c, _, _ := resolvePublishedCommit(cfg, "neil")
	corpusFile := filepath.Join(shareGenCorpusDir(cfg, "neil", c.Gen), m.ID+".md")
	raw, _ := os.ReadFile(corpusFile)
	raw[len(raw)/2] ^= 0xff
	if err := os.WriteFile(corpusFile, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// Read fails closed: not-found, never the altered bytes.
	if got, ok := findSharedMemory(cfg, m.ID); ok {
		t.Fatalf("read served corrupted corpus bytes: %+v", got)
	}
	// Search keeps serving — the index.db it reads is intact and digest-matched.
	res, err := searchSharedCorpora(context.Background(), cfg, "age", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	served := false
	for _, corpus := range res {
		for _, r := range corpus {
			if r.ID == m.ID {
				served = true
			}
		}
	}
	if !served {
		t.Fatal("corpus-only corruption wrongly excluded search (should serve the intact index)")
	}
	// Doctor surfaces the corruption (health hashes the corpus digest).
	if h := shareHealthOne(cfg, "neil", time.Now()); h.State == healthFresh {
		t.Fatal("doctor read fresh despite corpus corruption")
	}
}

// T14 (row 48): a substituted (valid v2, wrong-gen) index.db fails search closed
// via the index_digest binding, then heals from the frozen corpus.
func TestSubstitutedShareIndexFailsClosedOnSearch(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")
	x := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Xray", "revoke me")
	y := fixtureMemory("mem_20260601_000001_bbbbbbbb", "Yankee", "kept")
	publishGen(t, cfg, "neil", id, []Memory{x, y}) // gen-1 with X
	publishGen(t, cfg, "neil", id, []Memory{y})    // gen-2 revokes X (published)

	c1, c2 := commitAtSeq(t, cfg, "neil", 1), commitAtSeq(t, cfg, "neil", 2)
	// Warm cache with a clean search.
	if _, err := searchSharedCorpora(context.Background(), cfg, "kept", "", 10); err != nil {
		t.Fatal(err)
	}
	// Substitute gen-2's index.db with gen-1's structurally-valid v2 database.
	src, _ := os.ReadFile(shareGenIndexPath(cfg, "neil", c1.Gen))
	if err := os.WriteFile(shareGenIndexPath(cfg, "neil", c2.Gen), src, 0o644); err != nil {
		t.Fatal(err)
	}
	// Search must NOT serve X — the index_digest catches the substitution; heal
	// re-cuts from gen-2's frozen corpus (X absent).
	res, err := searchSharedCorpora(context.Background(), cfg, "revoke", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, corpus := range res {
		for _, m := range corpus {
			if m.ID == x.ID {
				t.Fatal("search served revoked X from a substituted index")
			}
		}
	}
}

func commitAtSeq(t *testing.T, cfg Config, name string, seq int) shareCommit {
	t.Helper()
	b, err := os.ReadFile(shareCommitPath(cfg, name, seq))
	if err != nil {
		t.Fatalf("read commit %d: %v", seq, err)
	}
	var c shareCommit
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// T8 (row 42): heal re-cuts ONLY from the published gen's frozen corpus and can
// never launder a stray/out-of-band file.
func TestHealRebuildsOnlyFromFrozenPublishedCorpus(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")
	m := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Real", "genuine content")
	publishGen(t, cfg, "neil", id, []Memory{m})
	c, _, _ := resolvePublishedCommit(cfg, "neil")

	// Corrupt gen-N's index and plant a stray file in a DIFFERENT gen dir.
	if err := os.WriteFile(shareGenIndexPath(cfg, "neil", c.Gen), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	strayGenCorpus := shareGenCorpusDir(cfg, "neil", "gen-strayrun")
	if err := os.MkdirAll(strayGenCorpus, 0o700); err != nil {
		t.Fatal(err)
	}
	strayBody, _ := renderMemory(fixtureMemory("mem_20260601_000009_dddddddd", "Stray", "out of band"))
	if err := os.WriteFile(filepath.Join(strayGenCorpus, "mem_20260601_000009_dddddddd.md"), strayBody, 0o644); err != nil {
		t.Fatal(err)
	}
	// A search heals from gen-N's own frozen corpus; the stray id is never served.
	res, err := searchSharedCorpora(context.Background(), cfg, "genuine", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := false
	for _, corpus := range res {
		for _, r := range corpus {
			if r.ID == "mem_20260601_000009_dddddddd" {
				t.Fatal("heal laundered a stray out-of-band file")
			}
			if r.ID == m.ID {
				got = true
			}
		}
	}
	if !got {
		t.Fatal("heal did not re-cut the genuine memory from the frozen corpus")
	}
}

// T9 (row 43a): GC never retires the published generation.
func TestGCNeverRetiresPublishedGen(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")
	m := fixtureMemory("mem_20260601_000000_aaaaaaaa", "A", "one")
	for i := 0; i < 6; i++ { // several generations
		publishGen(t, cfg, "neil", id, []Memory{m})
	}
	if err := shareGCSweep(cfg, "neil", time.Now()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	c, ok, _ := resolvePublishedCommit(cfg, "neil")
	if !ok {
		t.Fatal("GC deleted the published commit")
	}
	if _, err := os.Stat(shareGenDir(cfg, "neil", c.Gen)); err != nil {
		t.Fatalf("GC retired the published generation: %v", err)
	}
	if _, ok := findSharedMemory(cfg, m.ID); !ok {
		t.Fatal("published generation no longer serves after GC")
	}
}

// T10 (row 44a/b): GC reclaims committed losers and crash orphans, keeping K.
func TestGCReclaimsLosersAndOrphans(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")
	m := fixtureMemory("mem_20260601_000000_aaaaaaaa", "A", "one")
	for i := 0; i < 6; i++ {
		publishGen(t, cfg, "neil", id, []Memory{m})
	}
	// A stale uncommitted crash orphan (old mtime, no live lease).
	orphan := shareGenCorpusDir(cfg, "neil", "gen-orphanrun")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * shareImportTTL)
	_ = os.Chtimes(shareGenDir(cfg, "neil", "gen-orphanrun"), old, old)

	if err := shareGCSweep(cfg, "neil", time.Now()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// The crash orphan is gone.
	if _, err := os.Stat(shareGenDir(cfg, "neil", "gen-orphanrun")); !os.IsNotExist(err) {
		t.Fatal("GC did not reclaim the stale uncommitted crash orphan")
	}
	// Only K superseded generations (plus the published one) survive.
	gens, _ := os.ReadDir(shareGensDir(cfg, "neil"))
	if len(gens) > shareGenRetain+1 {
		t.Fatalf("GC kept %d generations; want <= %d", len(gens), shareGenRetain+1)
	}
}

// T4 (row 55b): the published reader is read-only (mode=ro).
func TestPublishedShareIndexReaderIsReadOnly(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "neil")
	publishGen(t, cfg, "neil", id, []Memory{fixtureMemory("mem_20260601_000000_aaaaaaaa", "A", "one")})
	c, _, _ := resolvePublishedCommit(cfg, "neil")
	before, _ := fileDigestOf(shareGenIndexPath(cfg, "neil", c.Gen))
	db, err := openShareIndexRO(context.Background(), shareGenIndexPath(cfg, "neil", c.Gen), c.IndexDigest)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE hax (x)`); err == nil {
		t.Fatal("DDL succeeded through a read-only share index handle")
	}
	after, _ := fileDigestOf(shareGenIndexPath(cfg, "neil", c.Gen))
	if before != after {
		t.Fatal("read-only handle mutated the index.db")
	}
}

// shares_unhealthy is surfaced on search_memory (H5).
func TestSearchMemorySurfacesUnhealthyShares(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "Local note", "local content")
	registerSub(t, cfg, "bad")
	if err := os.MkdirAll(shareSubRoot(cfg, "bad"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shareIndexPath(cfg, "bad"), []byte("legacy garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := callMCPTool(context.Background(), "search_memory", map[string]any{"query": "content"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), "shares_unhealthy") || !strings.Contains(string(b), `"bad"`) {
		t.Fatalf("search_memory did not surface the unhealthy share:\n%s", b)
	}
}
