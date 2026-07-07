package mora

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
)

// memStore is an in-memory objectStore for testing the bucket transport without a
// real S3 endpoint.
type memStore struct {
	mu   sync.Mutex
	objs map[string][]byte
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

func TestBucketPublishFetchImportRoundtrip(t *testing.T) {
	f := newBucketFixture(t)
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
		t.Fatalf("bucketPublish: %v", err)
	}

	// A URL holder who is not a recipient sees only opaque ciphertext + a signed
	// manifest envelope — no plaintext, ids, or scope.
	for k, v := range f.store.objs {
		if bytes.Contains(v, []byte("alpha body")) || bytes.Contains(v, []byte("project:acme")) {
			t.Fatalf("plaintext/scope leaked in object %s", k)
		}
	}

	dest := t.TempDir()
	sub := f.sub()
	pin, ver, err := bucketFetch(f.ctx, f.store, f.bc, sub, []age.Identity{f.id}, dest)
	if err != nil {
		t.Fatalf("bucketFetch: %v", err)
	}
	if ver != 1 {
		t.Fatalf("version = %d, want 1", ver)
	}
	if !bytes.Equal(pin, f.priv.Public().(ed25519.PublicKey)) {
		t.Fatal("returned pin != publisher signing key")
	}
	if _, err := os.Stat(filepath.Join(dest, "share.json")); err != nil {
		t.Fatalf("materialized dir missing share.json: %v", err)
	}
	for _, m := range f.mems {
		if _, err := os.Stat(filepath.Join(dest, "memories", m.ID+".md.age")); err != nil {
			t.Fatalf("materialized dir missing blob for %s: %v", m.ID, err)
		}
	}

	// End-to-end: the existing shareImport validates + indexes the materialized dir.
	sub.PinnedPubkey, sub.LastVersion = pin, ver
	stats, err := shareImport(f.ctx, f.cfg, sub, dest)
	if err != nil {
		t.Fatalf("shareImport of materialized dir: %v", err)
	}
	if stats.Total != 2 {
		t.Fatalf("imported %d memories, want 2", stats.Total)
	}
}

func TestBucketFetchRejectsTamperedBlob(t *testing.T) {
	f := newBucketFixture(t)
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	// Corrupt one ciphertext blob in place.
	for k := range f.store.objs {
		if shareBlobKeyRE.MatchString(strings.TrimPrefix(k, f.bc.objectPrefix())) {
			f.store.objs[k][0] ^= 0xff
			break
		}
	}
	_, _, err := bucketFetch(f.ctx, f.store, f.bc, f.sub(), []age.Identity{f.id}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "content-hash") {
		t.Fatalf("expected content-hash failure, got %v", err)
	}
}

func TestBucketFetchRejectsRollback(t *testing.T) {
	f := newBucketFixture(t)
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	sub := f.sub()
	sub.PinnedPubkey = f.priv.Public().(ed25519.PublicKey)
	sub.LastVersion = 5 // already saw a newer version
	_, _, err := bucketFetch(f.ctx, f.store, f.bc, sub, []age.Identity{f.id}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("expected rollback rejection, got %v", err)
	}
}

func TestBucketFetchRejectsCrossLocator(t *testing.T) {
	f := newBucketFixture(t)
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	// An attacker copies share A's objects verbatim under share B's prefix. The
	// B-subscriber finds a real, validly-signed manifest at its locator — but the
	// signature was bound to A's locator, so it must still be rejected.
	other := f.bc
	other.Prefix = "shares/beta"
	srcPre, dstPre := f.bc.objectPrefix(), other.objectPrefix()
	keys, _ := f.store.listKeys(f.ctx, srcPre)
	for _, k := range keys {
		b, _ := f.store.getObject(f.ctx, k)
		if err := f.store.putObject(f.ctx, dstPre+strings.TrimPrefix(k, srcPre), b); err != nil {
			t.Fatal(err)
		}
	}
	sub := shareSubscription{Name: "beta", Transport: &transportRef{Kind: "bucket", Bucket: &other}}
	_, _, err := bucketFetch(f.ctx, f.store, other, sub, []age.Identity{f.id}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected signature/destination rejection, got %v", err)
	}
}

func TestBucketPublishEgressAuditRefusesStrayPlaintext(t *testing.T) {
	f := newBucketFixture(t)
	// Something else drops a plaintext object under the prefix.
	if err := f.store.putObject(f.ctx, f.bc.objectPrefix()+"leak.md", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips())
	if err == nil || !strings.Contains(err.Error(), "non-ciphertext") {
		t.Fatalf("expected egress audit to refuse stray plaintext, got %v", err)
	}
}

func TestBucketRepublishIncrementsAndCleansOrphans(t *testing.T) {
	f := newBucketFixture(t)
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	if err := bucketPublish(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips()); err != nil {
		t.Fatal(err)
	}
	// After the second push: exactly len(mems) ciphertext blobs + one manifest.
	// Orphan blobs from the first push (age re-encrypts, so keys differ) are gone.
	keys, _ := f.store.listKeys(f.ctx, f.bc.objectPrefix())
	if len(keys) != len(f.mems)+1 {
		t.Fatalf("after republish: %d objects, want %d (blobs+manifest); orphans not cleaned: %v", len(keys), len(f.mems)+1, keys)
	}
	_, ver, err := bucketFetch(f.ctx, f.store, f.bc, f.sub(), []age.Identity{f.id}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if ver != 2 {
		t.Fatalf("version after republish = %d, want 2", ver)
	}
}
