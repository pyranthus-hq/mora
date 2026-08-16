package mora

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
)

// memStore is an in-memory objectStore for testing the bucket transport without a
// real S3 endpoint.
type memStore struct {
	mu   sync.Mutex
	objs map[string][]byte
}

type getObservingStore struct {
	objectStore
	once           sync.Once
	beforeFirstGet func()
}

func (s *getObservingStore) getObject(ctx context.Context, key string) ([]byte, error) {
	s.once.Do(s.beforeFirstGet)
	return s.objectStore.getObject(ctx, key)
}

// switchingGetStore serves one complete probe snapshot, then a different store
// starting at the second manifest read. It deterministically models a bucket
// changing between first-contact confirmation and the import refetch.
type switchingGetStore struct {
	first, second objectStore
	manifestKey   string
	mu            sync.Mutex
	manifestReads int
}

func (s *switchingGetStore) getObject(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	if key == s.manifestKey {
		s.manifestReads++
	}
	useSecond := s.manifestReads >= 2
	s.mu.Unlock()
	if useSecond {
		return s.second.getObject(ctx, key)
	}
	return s.first.getObject(ctx, key)
}

func (s *switchingGetStore) putObject(ctx context.Context, key string, data []byte) error {
	return s.first.putObject(ctx, key, data)
}

func (s *switchingGetStore) listKeys(ctx context.Context, prefix string) ([]string, error) {
	return s.first.listKeys(ctx, prefix)
}

func (s *switchingGetStore) deleteObject(ctx context.Context, key string) error {
	return s.first.deleteObject(ctx, key)
}

func newMemStore() *memStore { return &memStore{objs: map[string][]byte{}} }

func (m *memStore) putObject(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = append([]byte(nil), data...)
	return nil
}

func (m *memStore) getObject(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, errObjectNotFound
	}
	return append([]byte(nil), b...), nil
}

func (m *memStore) listKeys(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *memStore) deleteObject(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

func bucketTestCfg(t *testing.T) Config {
	t.Helper()
	return Config{ConfigDir: t.TempDir(), DataDir: t.TempDir(), VaultDir: t.TempDir(), StateDir: t.TempDir()}
}

func writeShareIdentity(t *testing.T, cfg Config, id *age.X25519Identity) {
	t.Helper()
	p := shareIdentityPath(cfg)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeBucketMemory(t *testing.T, dir, id, scope, body string) Memory {
	t.Helper()
	content := fmt.Sprintf("---\nid: %s\nscope: %s\ntype: note\ntitle: %s\ncreated_at: 2026-01-01T00:00:00Z\n---\n%s\n", id, scope, id, body)
	path := filepath.Join(dir, id+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := parseMemory(path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// bucketFixture stands up a publisher + subscriber sharing one age identity, two
// memories, and a fresh memStore. Returns everything a test needs.
type bucketFixture struct {
	ctx   context.Context
	cfg   Config
	id    *age.X25519Identity
	priv  ed25519.PrivateKey
	pub   sharePublish
	bc    bucketConfig
	store *memStore
	mems  []Memory
}

func newBucketFixture(t *testing.T) bucketFixture {
	t.Helper()
	cfg := bucketTestCfg(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	writeShareIdentity(t, cfg, id)
	priv, err := shareSigningKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	memRoot := t.TempDir()
	mems := []Memory{
		writeBucketMemory(t, memRoot, "mem_20260101_000000_aaaaaaaa", "project:acme", "alpha body"),
		writeBucketMemory(t, memRoot, "mem_20260101_000000_bbbbbbbb", "project:acme", "beta body"),
	}
	return bucketFixture{
		ctx:   context.Background(),
		cfg:   cfg,
		id:    id,
		priv:  priv,
		pub:   sharePublish{Name: "acme", Scope: "project:acme", Recipients: []string{id.Recipient().String()}, Owner: "adit"},
		bc:    bucketConfig{Bucket: "b", Prefix: "shares/acme"},
		store: newMemStore(),
		mems:  mems,
	}
}

func (f bucketFixture) recips() []age.Recipient { return []age.Recipient{f.id.Recipient()} }
func (f bucketFixture) sub() shareSubscription {
	return shareSubscription{Name: "acme", Transport: &transportRef{Kind: "bucket", Bucket: &f.bc}}
}

// TestBucketFetchRejectsDeclaredSizeMismatch covers the Phase-3-review fix binding
// the manifest's declared Size to the actual blob length — a malicious-but-signed
// publisher can't declare a bogus size (the hash check alone wouldn't catch it).

func TestShareInitBucketRecordsGrant(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	out := run(t, "share", "init", "acme", "--scope", "project:acme",
		"--recipient", id.Recipient().String(),
		"--via", "r2", "--bucket", "mybucket", "--endpoint", "https://acct.r2.cloudflarestorage.com", "--prefix", "shares/acme")
	if !strings.Contains(out, "bucket") {
		t.Fatalf("expected a bucket confirmation, got: %q", out)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	sf, err := loadShares(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.Publishes) != 1 {
		t.Fatalf("want 1 publish grant, got %d", len(sf.Publishes))
	}
	bc := bucketOf(sf.Publishes[0].Transport)
	if bc == nil {
		t.Fatal("recorded grant is not a bucket transport")
	}
	if bc.Bucket != "mybucket" || bc.Prefix != "shares/acme" {
		t.Fatalf("bucket config not persisted correctly: %+v", bc)
	}
	if sf.Publishes[0].Scope != "project:acme" {
		t.Fatalf("scope not recorded: %q", sf.Publishes[0].Scope)
	}
}

func TestShareInitBucketRequiresBucketName(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = Run(context.Background(), []string{"share", "init", "acme", "--scope", "project:acme",
		"--recipient", id.Recipient().String(), "--via", "r2"}, &out, &out, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "bucket") {
		t.Fatalf("expected a --bucket-required error, got: %v", err)
	}
}

func TestConcurrentFirstBucketSubscribersSerializeFetchAndRegistration(t *testing.T) {
	f := newBucketFixture(t)
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	pin := f.priv.Public().(ed25519.PublicKey)
	confirm := signPubFingerprint(pin)
	holdRelease, err := acquireStorageLease(f.cfg, "test-holder", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		err error
		out string
	}
	started := make(chan struct{}, 2)
	results := make(chan result, 2)
	for range 2 {
		go func() {
			started <- struct{}{}
			var out bytes.Buffer
			err := shareSubscribeBucketWithStore(f.ctx, f.cfg, "acme", f.bc, confirm, &out, f.store)
			results <- result{err: err, out: out.String()}
		}()
	}
	<-started
	<-started
	time.Sleep(100 * time.Millisecond)
	holdRelease()
	r1, r2 := <-results, <-results

	successes, conflicts := 0, 0
	for _, got := range []result{r1, r2} {
		if got.err == nil {
			successes++
		} else if strings.Contains(got.err.Error(), "already exists") {
			conflicts++
		} else {
			t.Fatalf("concurrent bucket subscribe failed unexpectedly: %v\n%s", got.err, got.out)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent outcomes: successes=%d conflicts=%d; want one each", successes, conflicts)
	}
	sf, err := loadShares(f.cfg)
	if err != nil || len(sf.Subscriptions) != 1 || sf.Subscriptions[0].Name != "acme" {
		t.Fatalf("serialized bucket registration = %+v, %v; want one acme", sf, err)
	}
	if _, ok := findSharedMemory(f.cfg, f.mems[0].ID); !ok {
		t.Fatal("losing bucket subscriber removed the winner's generation")
	}
	if h := shareHealthOne(f.cfg, "acme", time.Now()); h.State != healthFresh {
		t.Fatalf("losing bucket subscriber poisoned winner health: %+v", h)
	}
}

func TestFirstBucketSubscribeBindsProbeSignerAndVersion(t *testing.T) {
	t.Run("signer swap", func(t *testing.T) {
		f := newBucketFixture(t)
		if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
			t.Fatal(err)
		}
		other := newMemStore()
		_, otherPriv, err := ed25519.GenerateKey(cryptorand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := bucketPublish(f.ctx, other, f.bc, f.pub, f.mems, otherPriv, f.recips()); err != nil {
			t.Fatal(err)
		}
		switcher := &switchingGetStore{first: f.store, second: other, manifestKey: f.bc.ObjectPrefix() + shareManifestObject}
		confirm := signPubFingerprint(f.priv.Public().(ed25519.PublicKey))
		var out bytes.Buffer
		err = shareSubscribeBucketWithStore(f.ctx, f.cfg, "acme", f.bc, confirm, &out, switcher)
		if !errors.Is(err, errShareKeyRotated) {
			t.Fatalf("signer changed after confirmed probe = %v; want key-rotation refusal", err)
		}
	})

	t.Run("version rollback", func(t *testing.T) {
		f := newBucketFixture(t)
		if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
			t.Fatal(err)
		}
		if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
			t.Fatal(err)
		}
		older := newMemStore()
		if err := bucketPublish(f.ctx, older, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
			t.Fatal(err)
		}
		switcher := &switchingGetStore{first: f.store, second: older, manifestKey: f.bc.ObjectPrefix() + shareManifestObject}
		confirm := signPubFingerprint(f.priv.Public().(ed25519.PublicKey))
		var out bytes.Buffer
		err := shareSubscribeBucketWithStore(f.ctx, f.cfg, "acme", f.bc, confirm, &out, switcher)
		if err == nil || !strings.Contains(err.Error(), "rollback") {
			t.Fatalf("version rolled back after confirmed probe = %v; want refusal", err)
		}
	})
}

func TestBucketStorageLimitRetryUsesPrintedExactDecision(t *testing.T) {
	f := newBucketFixture(t)
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdShareStorageLimit(f.cfg, []string{"16"}, &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	confirm := signPubFingerprint(f.priv.Public().(ed25519.PublicKey))
	err := shareSubscribeBucketWithStore(f.ctx, f.cfg, "acme", f.bc, confirm, &out, f.store)
	if err == nil || !strings.Contains(err.Error(), "storage-limit") {
		t.Fatalf("tiny-limit bucket subscribe = %v; want explicit decision", err)
	}
	const marker = "storage-limit "
	i := strings.Index(err.Error(), marker)
	if i < 0 {
		t.Fatalf("refusal omitted required limit: %v", err)
	}
	fields := strings.Fields(err.Error()[i+len(marker):])
	if len(fields) == 0 {
		t.Fatalf("refusal omitted required limit: %v", err)
	}
	required := strings.Trim(fields[0], "'\"")
	entries, readErr := os.ReadDir(shareSubRoot(f.cfg, "acme"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "fetch-") {
			t.Fatalf("normal admission refusal retained bucket staging %q", entry.Name())
		}
	}
	if err := cmdShareStorageLimit(f.cfg, []string{required}, &out, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := shareSubscribeBucketWithStore(f.ctx, f.cfg, "acme", f.bc, confirm, &out, f.store); err != nil {
		t.Fatalf("printed storage-limit %s did not admit immediate bucket retry: %v", required, err)
	}
}

func TestBucketFetchMaterializationImports(t *testing.T) {
	f := newBucketFixture(t)
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	sub := f.sub()
	pin, version, err := bucketFetch(f.ctx, f.store, f.bc, sub, []age.Identity{f.id}, dest)
	if err != nil {
		t.Fatal(err)
	}
	sub.PinnedPubkey, sub.LastVersion = pin, version
	stats, err := importFixtureGeneration(f.ctx, f.cfg, sub, dest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 2 {
		t.Fatalf("imported %d memories, want 2", stats.Total)
	}
}
