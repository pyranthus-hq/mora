package mora

// Verified no-op pull: a pull whose source revision matches the published head
// re-verifies that head instead of decrypting and building a new generation,
// and health accepts the verified no-op as a fresh confirmation.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	sharingpkg "github.com/pyranthus-hq/mora/internal/sharing"
)

// subscribeGitShare subscribes "neil" to a real git share remote holding mems.
func subscribeGitShare(t *testing.T, mems []Memory) (Config, *age.X25519Identity, string) {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	remote := realGitShareRemote(t, id.Recipient(), mems)
	var buf bytes.Buffer
	if err := shareSubscribe(context.Background(), cfg, []string{"neil", "--remote", remote}, &buf, realExec); err != nil {
		t.Fatalf("subscribe: %v\n%s", err, buf.String())
	}
	return cfg, id, remote
}

// commitShareRemote writes mems into the remote's memories/ (replacing what was
// there) and commits.
func commitShareRemote(t *testing.T, remote string, rec age.Recipient, mems []Memory) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(remote, "memories")); err != nil {
		t.Fatal(err)
	}
	buildShareRepoFixture(t, remote, rec, mems, false)
	mustGit(t, remote, "add", "-A")
	mustGit(t, remote, "commit", "-q", "-m", "update")
}

func pullNeil(t *testing.T, cfg Config) string {
	t.Helper()
	var buf bytes.Buffer
	if err := sharePull(context.Background(), cfg, []string{"neil"}, &buf, realExec); err != nil {
		t.Fatalf("sharePull: %v\n%s", err, buf.String())
	}
	return buf.String()
}

func genDirNames(t *testing.T, cfg Config, name string) []string {
	t.Helper()
	ents, err := os.ReadDir(shareGensDir(cfg, name))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func mustHead(t *testing.T, cfg Config, name string) shareCommit {
	t.Helper()
	c, ok, err := resolvePublishedCommit(cfg, name)
	if err != nil || !ok {
		t.Fatalf("resolve head: ok=%v err=%v", ok, err)
	}
	return c
}

func mustAttempt(t *testing.T, cfg Config, name string) shareAttempt {
	t.Helper()
	a, ok, err := loadShareAttempt(cfg, name)
	if err != nil || !ok {
		t.Fatalf("load attempt: ok=%v err=%v", ok, err)
	}
	return a
}

func TestSharePullNoopMintsNoGenerationAndStaysFresh(t *testing.T) {
	mems := []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "One", "first"),
		fixtureMemory("mem_20260601_000001_bbbbbbbb", "Two", "second"),
	}
	cfg, _, _ := subscribeGitShare(t, mems)
	head := mustHead(t, cfg, "neil")
	gens := genDirNames(t, cfg, "neil")

	for i := 0; i < 2; i++ {
		out := pullNeil(t, cfg)
		if !strings.Contains(out, "up to date, 2 total") {
			t.Fatalf("pull %d output = %q; want an up-to-date report", i, out)
		}
		if got := mustHead(t, cfg, "neil"); got.Seq != head.Seq || got.Gen != head.Gen || got.RunID != head.RunID {
			t.Fatalf("pull %d moved the head: %+v -> %+v", i, head, got)
		}
		if got := genDirNames(t, cfg, "neil"); strings.Join(got, ",") != strings.Join(gens, ",") {
			t.Fatalf("pull %d minted a generation: %v -> %v", i, gens, got)
		}
		a := mustAttempt(t, cfg, "neil")
		if a.State != "succeeded" || a.Seq != head.Seq || a.RunID == head.RunID || a.VerifiedAt == "" {
			t.Fatalf("pull %d attempt = %+v; want a verified no-op of seq %d", i, a, head.Seq)
		}
		if h := shareHealthOne(cfg, "neil", time.Now()); h.State != healthFresh {
			t.Fatalf("pull %d health = %+v; a verified no-op must stay fresh", i, h)
		}
	}
	commits, err := readAllCommits(cfg, "neil")
	if err != nil || len(commits) != 1 {
		t.Fatalf("commits = %d, %v; want exactly the subscribe commit", len(commits), err)
	}
}

func TestSharePullRealChangeImportsWithHonestCounts(t *testing.T) {
	a := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Keep", "kept")
	b := fixtureMemory("mem_20260601_000001_bbbbbbbb", "Edit", "before")
	c := fixtureMemory("mem_20260601_000002_cccccccc", "Drop", "dropped")
	cfg, id, remote := subscribeGitShare(t, []Memory{a, b, c})
	head := mustHead(t, cfg, "neil")

	b.Text = "after"
	d := fixtureMemory("mem_20260601_000003_dddddddd", "New", "added")
	commitShareRemote(t, remote, id.Recipient(), []Memory{a, b, d})

	out := pullNeil(t, cfg)
	if !strings.Contains(out, "2 new/updated, 1 removed, 3 total") {
		t.Fatalf("pull output = %q; want 2 new/updated, 1 removed, 3 total", out)
	}
	next := mustHead(t, cfg, "neil")
	if next.Seq != head.Seq+1 || next.Gen == head.Gen {
		t.Fatalf("a real change did not commit a new generation: %+v -> %+v", head, next)
	}
	if at := mustAttempt(t, cfg, "neil"); at.RunID != next.RunID || at.VerifiedAt != "" {
		t.Fatalf("attempt = %+v; want the importing run with no verified_at", at)
	}
	if _, ok := findSharedMemory(cfg, c.ID); ok {
		t.Fatal("removed memory still served")
	}
	if m, ok := findSharedMemory(cfg, b.ID); !ok || !strings.Contains(m.Text, "after") {
		t.Fatalf("edited memory not updated: ok=%v text=%q", ok, m.Text)
	}
	if h := shareHealthOne(cfg, "neil", time.Now()); h.State != healthFresh {
		t.Fatalf("health after real import = %+v", h)
	}

	// The next pull of the same publisher commit is a no-op again.
	if out := pullNeil(t, cfg); !strings.Contains(out, "up to date, 3 total") {
		t.Fatalf("re-pull output = %q; want up to date", out)
	}
}

func TestSharePullRebuildsWhenHeadNoLongerVerifies(t *testing.T) {
	m := fixtureMemory("mem_20260601_000000_aaaaaaaa", "One", "first")
	cases := map[string]func(t *testing.T, cfg Config, head shareCommit){
		"index_digest": func(t *testing.T, cfg Config, head shareCommit) {
			p := shareGenIndexPath(cfg, "neil", head.Gen)
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			b[len(b)-1] ^= 0xff
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"corpus_digest": func(t *testing.T, cfg Config, head shareCommit) {
			p := filepath.Join(shareGenCorpusDir(cfg, "neil", head.Gen), m.ID+".md")
			if err := os.WriteFile(p, []byte("corrupt"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		// The digests still match, but the index was built for another schema
		// (as after a mora upgrade). Serving would refuse it, so a pull must
		// rebuild rather than re-verify it.
		"index_schema": func(t *testing.T, cfg Config, head shareCommit) {
			p := shareGenIndexPath(cfg, "neil", head.Gen)
			db, err := sql.Open("sqlite", p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			d, err := fileDigestOf(p)
			if err != nil {
				t.Fatal(err)
			}
			head.IndexDigest = d
			body, err := json.MarshalIndent(head, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			cp := shareCommitPath(cfg, "neil", head.Seq)
			_ = os.Chmod(cp, 0o644)
			if err := os.WriteFile(cp, append(body, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			if msg := shareHeadIntegrityError(cfg, "neil", mustHead(t, cfg, "neil")); msg != "" {
				t.Fatalf("schema fixture should pass the digest checks: %s", msg)
			}
		},
	}
	for name, damage := range cases {
		subRun(t, name, func(t *testing.T) {
			cfg, _, _ := subscribeGitShare(t, []Memory{m})
			head := mustHead(t, cfg, "neil")
			damage(t, cfg, head)

			out := pullNeil(t, cfg)
			if strings.Contains(out, "up to date") {
				t.Fatalf("pull re-verified a head that does not verify: %q", out)
			}
			next := mustHead(t, cfg, "neil")
			if next.Seq != head.Seq+1 {
				t.Fatalf("pull did not rebuild: head %+v -> %+v", head, next)
			}
			if h := shareHealthOne(cfg, "neil", time.Now()); h.State != healthFresh {
				t.Fatalf("health after rebuild = %+v", h)
			}
			if _, ok := findSharedMemory(cfg, m.ID); !ok {
				t.Fatal("rebuilt generation does not serve the memory")
			}
		})
	}
}

func TestShareHealthFreshnessFollowsVerifiedNoop(t *testing.T) {
	cfg, _, _ := subscribeGitShare(t, []Memory{fixtureMemory("mem_20260601_000000_aaaaaaaa", "One", "first")})
	head := mustHead(t, cfg, "neil")
	built, err := time.Parse(time.RFC3339, head.BuiltAt)
	if err != nil {
		t.Fatal(err)
	}
	importRun := mustAttempt(t, cfg, "neil")
	writeAttempt := func(a shareAttempt) {
		t.Helper()
		body, err := json.MarshalIndent(a, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(shareAttemptPath(cfg, "neil"), append(body, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	late := built.Add(shareStaleThreshold + 12*time.Hour)

	// The importing run alone: freshness ages from built_at.
	if h := shareHealthOne(cfg, "neil", late); h.State != healthStale {
		t.Fatalf("import-only health at built+60h = %+v; want stale", h)
	}

	// A verified no-op 40h after the build confirms the head again.
	verified := built.Add(40 * time.Hour).UTC().Format(time.RFC3339)
	noop := shareAttempt{RunID: "noop-run", State: "succeeded", StartedAt: verified, CompletedAt: verified, Seq: head.Seq, VerifiedAt: verified}
	writeAttempt(noop)
	if h := shareHealthOne(cfg, "neil", late); h.State != healthFresh {
		t.Fatalf("health 20h after a verified no-op = %+v; want fresh", h)
	}
	if h := shareHealthOne(cfg, "neil", built.Add(40*time.Hour+shareStaleThreshold+time.Hour)); h.State != healthStale {
		t.Fatalf("health 49h after the verified no-op = %+v; want stale", h)
	}

	// verified_at binds to the head seq it checked; another head is unconfirmed.
	noop.Seq = head.Seq + 1
	writeAttempt(noop)
	if h := shareHealthOne(cfg, "neil", built.Add(time.Hour)); h.State != healthStale || !strings.Contains(h.LastError, "no matching completed import") {
		t.Fatalf("verified no-op of another seq = %+v; want stale", h)
	}

	// A different run without verified_at never confirms a head it did not build.
	writeAttempt(shareAttempt{RunID: "other-run", State: "succeeded", StartedAt: importRun.StartedAt, Seq: head.Seq})
	if h := shareHealthOne(cfg, "neil", built.Add(time.Hour)); h.State != healthStale {
		t.Fatalf("unverified foreign success = %+v; want stale", h)
	}
}

// countingStore records every object key read through it.
type countingStore struct {
	objectStore
	mu   sync.Mutex
	gets []string
}

func (s *countingStore) getObject(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	s.gets = append(s.gets, key)
	s.mu.Unlock()
	return s.objectStore.getObject(ctx, key)
}

func (s *countingStore) blobGets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, k := range s.gets {
		if sharingpkg.BlobKeyRE.MatchString(filepath.Base(k)) {
			n++
		}
	}
	return n
}

func TestBucketPullNoopSkipsBlobsAndStaysFresh(t *testing.T) {
	f := newBucketFixture(t)
	cfg := f.cfg
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	fp := signPubFingerprint(f.priv.Public().(ed25519.PublicKey))
	if err := shareSubscribeBucketWithStore(f.ctx, cfg, "acme", f.bc, fp, &buf, f.store); err != nil {
		t.Fatalf("subscribe: %v\n%s", err, buf.String())
	}
	pull := func(store objectStore) (shareRunOutcome, shareImportStats) {
		t.Helper()
		sf, err := loadShares(cfg)
		if err != nil || len(sf.Subscriptions) != 1 {
			t.Fatalf("registry: %+v %v", sf, err)
		}
		sub := sf.Subscriptions[0]
		var out shareRunOutcome
		var stats shareImportStats
		var pin ed25519.PublicKey
		var ver int
		if err := shareImportAndPublish(f.ctx, cfg, sub.Name, func(runID string) (shareRunOutcome, error) {
			o, st, p, v, ierr := bucketShareImportWithStore(f.ctx, cfg, sub, f.bc, runID, store, true)
			out, stats, pin, ver = o, st, p, v
			return o, ierr
		}); err != nil {
			t.Fatalf("bucket pull: %v", err)
		}
		if err := updateSubscriptionState(cfg, sub.Name, pin, ver); err != nil {
			t.Fatal(err)
		}
		return out, stats
	}
	head := mustHead(t, cfg, "acme")

	cs := &countingStore{objectStore: f.store}
	out, stats := pull(cs)
	if !out.Verified || out.Seq != head.Seq || stats.Total != 2 || stats.Imported != 0 {
		t.Fatalf("unchanged bucket pull = %+v %+v; want a verified no-op of seq %d", out, stats, head.Seq)
	}
	if n := cs.blobGets(); n != 0 {
		t.Fatalf("no-op pull downloaded %d blobs; gets=%v", n, cs.gets)
	}
	if got := mustHead(t, cfg, "acme"); got.Seq != head.Seq {
		t.Fatalf("no-op pull moved the head: %+v -> %+v", head, got)
	}
	if h := shareHealthOne(cfg, "acme", time.Now()); h.State != healthFresh {
		t.Fatalf("health after bucket no-op = %+v", h)
	}

	// The publisher drops one memory, edits the other, and adds a third.
	memRoot := t.TempDir()
	next := []Memory{
		writeBucketMemory(t, memRoot, "mem_20260101_000000_bbbbbbbb", "project:acme", "beta body edited"),
		writeBucketMemory(t, memRoot, "mem_20260101_000000_cccccccc", "project:acme", "gamma body"),
	}
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, next, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	cs = &countingStore{objectStore: f.store}
	out, stats = pull(cs)
	if out.Verified || stats.Imported != 2 || stats.Removed != 1 || stats.Total != 2 {
		t.Fatalf("changed bucket pull = %+v %+v; want 2 changed, 1 removed, 2 total", out, stats)
	}
	if n := cs.blobGets(); n != 2 {
		t.Fatalf("changed pull fetched %d blobs; want 2", n)
	}
	if got := mustHead(t, cfg, "acme"); got.Seq != head.Seq+1 || !strings.HasPrefix(got.SourceRev, "bucket-v2-") {
		t.Fatalf("changed pull head = %+v; want seq %d at bucket-v2", got, head.Seq+1)
	}
}

// Two publishers racing from one version can each sign a different manifest
// with the next version number. The envelope the subscriber sees later must
// be imported, not reported up to date because the version number matches.
func TestBucketPullImportsSameVersionDifferentManifest(t *testing.T) {
	f := newBucketFixture(t)
	cfg := f.cfg
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	fp := signPubFingerprint(f.priv.Public().(ed25519.PublicKey))
	var buf bytes.Buffer
	if err := shareSubscribeBucketWithStore(f.ctx, cfg, "acme", f.bc, fp, &buf, f.store); err != nil {
		t.Fatalf("subscribe: %v\n%s", err, buf.String())
	}
	// The second publisher starts from the same version-1 bucket state.
	racer := newMemStore()
	for k, v := range f.store.objs {
		racer.objs[k] = append([]byte(nil), v...)
	}
	pull := func(store objectStore) shareRunOutcome {
		t.Helper()
		sf, err := loadShares(cfg)
		if err != nil {
			t.Fatal(err)
		}
		sub := sf.Subscriptions[0]
		var out shareRunOutcome
		var pin ed25519.PublicKey
		var ver int
		if err := shareImportAndPublish(f.ctx, cfg, sub.Name, func(runID string) (shareRunOutcome, error) {
			o, _, p, v, ierr := bucketShareImportWithStore(f.ctx, cfg, sub, f.bc, runID, store, true)
			out, pin, ver = o, p, v
			return o, ierr
		}); err != nil {
			t.Fatalf("pull: %v", err)
		}
		if err := updateSubscriptionState(cfg, sub.Name, pin, ver); err != nil {
			t.Fatal(err)
		}
		return out
	}
	memRoot := t.TempDir()
	first := []Memory{writeBucketMemory(t, memRoot, "mem_20260101_000000_eeeeeeee", "project:acme", "first v2")}
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, first, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	if out := pull(f.store); out.Verified {
		t.Fatal("first version-2 envelope was not imported")
	}
	second := []Memory{writeBucketMemory(t, memRoot, "mem_20260101_000000_ffffffff", "project:acme", "second v2")}
	if err := bucketPublish(f.ctx, racer, f.bc, f.pub, second, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	if out := pull(racer); out.Verified {
		t.Fatal("a different version-2 manifest was reported up to date")
	}
	if _, ok := findSharedMemory(cfg, second[0].ID); !ok {
		t.Fatal("the later version-2 manifest was not imported")
	}
	if out := pull(racer); !out.Verified {
		t.Fatal("re-pulling the same envelope should be a verified no-op")
	}
}

func TestBucketPullNoopStillEnforcesReplayFloor(t *testing.T) {
	f := newBucketFixture(t)
	cfg := f.cfg
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	fp := signPubFingerprint(f.priv.Public().(ed25519.PublicKey))
	if err := shareSubscribeBucketWithStore(f.ctx, cfg, "acme", f.bc, fp, &buf, f.store); err != nil {
		t.Fatalf("subscribe: %v\n%s", err, buf.String())
	}
	sf, err := loadShares(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sub := sf.Subscriptions[0]
	// A committed floor above the served version (as after a lost registry
	// update) must refuse the replayed head version, not re-verify it.
	head := mustHead(t, cfg, "acme")
	head.Seq, head.RunID, head.BucketFloor = head.Seq+1, "floor", 9
	body, _ := json.Marshal(head)
	if err := os.WriteFile(shareCommitPath(cfg, "acme", head.Seq), body, 0o644); err != nil {
		t.Fatal(err)
	}
	err = shareImportAndPublish(f.ctx, cfg, sub.Name, func(runID string) (shareRunOutcome, error) {
		o, _, _, _, ierr := bucketShareImportWithStore(f.ctx, cfg, sub, f.bc, runID, f.store, true)
		return o, ierr
	})
	if err == nil || !strings.Contains(err.Error(), "anti-rollback") {
		t.Fatalf("no-op below the committed floor = %v; want the rollback refusal", err)
	}
}

// A bucket version is not unique across buckets. Subscribe must build even when
// files from an earlier subscription under the same name hold a verifying head
// at the same version, or it would serve the old bucket's content.
func TestBucketSubscribeNeverAdoptsLeftoverHead(t *testing.T) {
	f := newBucketFixture(t)
	cfg := f.cfg
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	fp := signPubFingerprint(f.priv.Public().(ed25519.PublicKey))
	var buf bytes.Buffer
	if err := shareSubscribeBucketWithStore(f.ctx, cfg, "acme", f.bc, fp, &buf, f.store); err != nil {
		t.Fatalf("subscribe: %v\n%s", err, buf.String())
	}
	old := mustHead(t, cfg, "acme")
	// A partial remove: the registry entry is gone, the generation files remain.
	sf, err := loadShares(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sf.Subscriptions = nil
	if err := saveShares(cfg, sf); err != nil {
		t.Fatal(err)
	}

	other := bucketConfig{Bucket: "b2", Prefix: "shares/other"}
	otherStore := newMemStore()
	memRoot := t.TempDir()
	mems := []Memory{writeBucketMemory(t, memRoot, "mem_20260101_000000_dddddddd", "project:acme", "other bucket")}
	if err := bucketPublish(f.ctx, otherStore, other, f.pub, mems, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := shareSubscribeBucketWithStore(f.ctx, cfg, "acme", other, fp, &buf, otherStore); err != nil {
		t.Fatalf("re-subscribe: %v\n%s", err, buf.String())
	}
	head := mustHead(t, cfg, "acme")
	if head.Seq == old.Seq || head.BucketFloor != old.BucketFloor {
		t.Fatalf("fixture must reuse bucket version %d under a new head: %+v -> %+v", old.BucketFloor, old, head)
	}
	if _, ok := findSharedMemory(cfg, f.mems[0].ID); ok {
		t.Fatal("re-subscribe served the earlier bucket's memory")
	}
	if _, ok := findSharedMemory(cfg, mems[0].ID); !ok {
		t.Fatal("re-subscribe did not import the new bucket")
	}
}
