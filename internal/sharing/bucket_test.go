package sharing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"filippo.io/age"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/memory"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

type memStore struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func newMemStore() *memStore { return &memStore{objs: map[string][]byte{}} }
func (m *memStore) PutObject(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = append([]byte(nil), data...)
	return nil
}
func (m *memStore) GetObject(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, ErrObjectNotFound
	}
	return append([]byte(nil), b...), nil
}
func (m *memStore) ListKeys(_ context.Context, prefix string) ([]string, error) {
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
func (m *memStore) DeleteObject(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

type bucketFixture struct {
	ctx   context.Context
	id    *age.X25519Identity
	priv  ed25519.PrivateKey
	pub   Publish
	bc    BucketConfig
	store *memStore
	mems  []memory.Memory
}

func writeBucketMemory(t *testing.T, dir, id, scope, body string) memory.Memory {
	t.Helper()
	content := fmt.Sprintf("---\nid: %s\nscope: %s\ntype: note\ntitle: %s\ncreated_at: 2026-01-01T00:00:00Z\n---\n%s\n", id, scope, id, body)
	path := filepath.Join(dir, id+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return memory.Memory{ID: id, Scope: scope, Type: "note", Title: id, CreatedAt: "2026-01-01T00:00:00Z", Text: body, Path: path}
}
func newBucketFixture(t *testing.T) bucketFixture {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	mems := []memory.Memory{writeBucketMemory(t, root, "mem_20260101_000000_aaaaaaaa", "project:acme", "alpha body"), writeBucketMemory(t, root, "mem_20260101_000000_bbbbbbbb", "project:acme", "beta body")}
	return bucketFixture{ctx: context.Background(), id: id, priv: priv, pub: Publish{Name: "acme", Scope: "project:acme", Recipients: []string{id.Recipient().String()}, Owner: "adit"}, bc: BucketConfig{Bucket: "b", Prefix: "shares/acme"}, store: newMemStore(), mems: mems}
}
func (f bucketFixture) recips() []age.Recipient { return []age.Recipient{f.id.Recipient()} }
func (f bucketFixture) sub() Subscription {
	return Subscription{Name: "acme", Transport: &TransportRef{Kind: "bucket", Bucket: &f.bc}}
}
func TestBucketPublishFetchRoundtrip(t *testing.T) {
	f := newBucketFixture(t)
	if err := PublishBucket(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips(), "mora test"); err != nil {
		t.Fatalf("PublishBucket: %v", err)
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
	pin, ver, err := FetchBucket(f.ctx, f.store, f.bc, sub, []age.Identity{f.id}, dest)
	if err != nil {
		t.Fatalf("FetchBucket: %v", err)
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

}
func TestBucketFetchRejectsTamperedBlob(t *testing.T) {
	f := newBucketFixture(t)
	if err := PublishBucket(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips(), "mora test"); err != nil {
		t.Fatal(err)
	}
	// Corrupt one ciphertext blob in place.
	for k := range f.store.objs {
		if BlobKeyRE.MatchString(strings.TrimPrefix(k, f.bc.ObjectPrefix())) {
			f.store.objs[k][0] ^= 0xff
			break
		}
	}
	_, _, err := FetchBucket(f.ctx, f.store, f.bc, f.sub(), []age.Identity{f.id}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "content-hash") {
		t.Fatalf("expected content-hash failure, got %v", err)
	}
}

func TestBucketFetchRejectsRollback(t *testing.T) {
	f := newBucketFixture(t)
	if err := PublishBucket(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips(), "mora test"); err != nil {
		t.Fatal(err)
	}
	sub := f.sub()
	sub.PinnedPubkey = f.priv.Public().(ed25519.PublicKey)
	sub.LastVersion = 5 // already saw a newer version
	_, _, err := FetchBucket(f.ctx, f.store, f.bc, sub, []age.Identity{f.id}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("expected rollback rejection, got %v", err)
	}
}

func TestBucketFetchRejectsCrossLocator(t *testing.T) {
	f := newBucketFixture(t)
	if err := PublishBucket(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips(), "mora test"); err != nil {
		t.Fatal(err)
	}
	// An attacker copies share A's objects verbatim under share B's prefix. The
	// B-subscriber finds a real, validly-signed manifest at its locator — but the
	// signature was bound to A's locator, so it must still be rejected.
	other := f.bc
	other.Prefix = "shares/beta"
	srcPre, dstPre := f.bc.ObjectPrefix(), other.ObjectPrefix()
	keys, _ := f.store.ListKeys(f.ctx, srcPre)
	for _, k := range keys {
		b, _ := f.store.GetObject(f.ctx, k)
		if err := f.store.PutObject(f.ctx, dstPre+strings.TrimPrefix(k, srcPre), b); err != nil {
			t.Fatal(err)
		}
	}
	sub := Subscription{Name: "beta", Transport: &TransportRef{Kind: "bucket", Bucket: &other}}
	_, _, err := FetchBucket(f.ctx, f.store, other, sub, []age.Identity{f.id}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected signature/destination rejection, got %v", err)
	}
}

func TestBucketPublishEgressAuditRefusesStrayPlaintext(t *testing.T) {
	f := newBucketFixture(t)
	// Something else drops a plaintext object under the prefix.
	if err := f.store.PutObject(f.ctx, f.bc.ObjectPrefix()+"leak.md", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	err := PublishBucket(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips(), "mora test")
	if err == nil || !strings.Contains(err.Error(), "non-ciphertext") {
		t.Fatalf("expected egress audit to refuse stray plaintext, got %v", err)
	}
}

func TestBucketFetchRejectsDeclaredSizeMismatch(t *testing.T) {
	f := newBucketFixture(t)
	plain := []byte("---\nid: mem_20260101_000000_cccccccc\nscope: project:acme\ntype: note\ntitle: x\ncreated_at: 2026-01-01T00:00:00Z\n---\nbody\n")
	ct, err := EncryptBytes(f.recips(), plain)
	if err != nil {
		t.Fatal(err)
	}
	prefix := f.bc.ObjectPrefix()
	if err := f.store.PutObject(f.ctx, prefix+BlobObjectName(ct), ct); err != nil {
		t.Fatal(err)
	}
	man := Manifest{
		Schema: ManifestSchema, Name: "acme", Scope: "project:acme", Client: "t", Version: 1,
		Entries: []ManifestEntry{{ID: "mem_20260101_000000_cccccccc", Blob: BlobKey(ct), Size: 999999}},
	}
	mj, _ := json.Marshal(man)
	env, err := SealManifest(f.priv, f.bc.Locator(), mj, 1, f.recips(), true)
	if err != nil {
		t.Fatal(err)
	}
	eb, _ := json.Marshal(env)
	if err := f.store.PutObject(f.ctx, prefix+ManifestObject, eb); err != nil {
		t.Fatal(err)
	}
	_, _, err = FetchBucket(f.ctx, f.store, f.bc, f.sub(), []age.Identity{f.id}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "declared") {
		t.Fatalf("expected declared-size mismatch rejection, got %v", err)
	}
}

func TestBucketRepublishIncrementsAndCleansOrphans(t *testing.T) {
	f := newBucketFixture(t)
	if err := PublishBucket(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips(), "mora test"); err != nil {
		t.Fatal(err)
	}
	if err := PublishBucket(f.ctx, f.store, f.bc, f.pub, f.mems, f.priv, f.recips(), "mora test"); err != nil {
		t.Fatal(err)
	}
	// After the second push: exactly len(mems) ciphertext blobs + one manifest.
	// Orphan blobs from the first push (age re-encrypts, so keys differ) are gone.
	keys, _ := f.store.ListKeys(f.ctx, f.bc.ObjectPrefix())
	if len(keys) != len(f.mems)+1 {
		t.Fatalf("after republish: %d objects, want %d (blobs+manifest); orphans not cleaned: %v", len(keys), len(f.mems)+1, keys)
	}
	_, ver, err := FetchBucket(f.ctx, f.store, f.bc, f.sub(), []age.Identity{f.id}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if ver != 2 {
		t.Fatalf("version after republish = %d, want 2", ver)
	}
}

func putTestManifest(t *testing.T, f bucketFixture, man Manifest, outerVersion int) {
	t.Helper()
	body, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	env, err := SealManifest(f.priv, f.bc.Locator(), body, outerVersion, f.recips(), true)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutObject(f.ctx, f.bc.ObjectPrefix()+ManifestObject, encoded); err != nil {
		t.Fatal(err)
	}
}
func TestCurrentEnvelopeRefusals(t *testing.T) {
	store := newMemStore()
	if _, err := CurrentEnvelope(context.Background(), store, ""); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("missing error=%v", err)
	}
	if err := store.PutObject(context.Background(), ManifestObject, []byte("{")); err != nil {
		t.Fatal(err)
	}
	if _, err := CurrentEnvelope(context.Background(), store, ""); err == nil || !strings.Contains(err.Error(), "valid envelope") {
		t.Fatalf("malformed error=%v", err)
	}
}
func TestFetchManifestRefusals(t *testing.T) {
	base := func() bucketFixture { return newBucketFixture(t) }
	cases := []struct {
		name  string
		man   func(bucketFixture) Manifest
		outer int
		want  string
	}{{"schema", func(f bucketFixture) Manifest { return Manifest{Schema: 99, Name: "x", Scope: "project:x", Version: 1} }, 1, "schema"}, {"inner version", func(f bucketFixture) Manifest {
		return Manifest{Schema: ManifestSchema, Name: "x", Scope: "project:x", Version: 2}
	}, 1, "inner version"}, {"scope", func(f bucketFixture) Manifest {
		return Manifest{Schema: ManifestSchema, Name: "x", Scope: "../x", Version: 1}
	}, 1, "invalid scope"}, {"too many", func(f bucketFixture) Manifest {
		return Manifest{Schema: ManifestSchema, Name: "x", Scope: "project:x", Version: 1, Entries: make([]ManifestEntry, MaxShareEntries+1)}
	}, 1, "over the"}, {"unsafe id", func(f bucketFixture) Manifest {
		return Manifest{Schema: ManifestSchema, Name: "x", Scope: "project:x", Version: 1, Entries: []ManifestEntry{{ID: "../x", Blob: strings.Repeat("a", 64), Size: 1}}}
	}, 1, "unsafe"}, {"bad blob", func(f bucketFixture) Manifest {
		return Manifest{Schema: ManifestSchema, Name: "x", Scope: "project:x", Version: 1, Entries: []ManifestEntry{{ID: "safe", Blob: "bad", Size: 1}}}
	}, 1, "malformed blob"}, {"bad size", func(f bucketFixture) Manifest {
		return Manifest{Schema: ManifestSchema, Name: "x", Scope: "project:x", Version: 1, Entries: []ManifestEntry{{ID: "safe", Blob: strings.Repeat("a", 64), Size: -1}}}
	}, 1, "out-of-range"}, {"missing blob", func(f bucketFixture) Manifest {
		return Manifest{Schema: ManifestSchema, Name: "x", Scope: "project:x", Version: 1, Entries: []ManifestEntry{{ID: "safe", Blob: strings.Repeat("a", 64), Size: 1}}}
	}, 1, "missing"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := base()
			putTestManifest(t, f, tc.man(f), tc.outer)
			_, _, err := FetchBucket(f.ctx, f.store, f.bc, f.sub(), []age.Identity{f.id}, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want %q", err, tc.want)
			}
		})
	}
}
func TestFetchRejectsCaseFoldDuplicateIDs(t *testing.T) {
	f := newBucketFixture(t)
	plain := []byte("ciphertext input")
	ct, err := EncryptBytes(f.recips(), plain)
	if err != nil {
		t.Fatal(err)
	}
	blob := BlobKey(ct)
	if err := f.store.PutObject(f.ctx, f.bc.ObjectPrefix()+blob+".age", ct); err != nil {
		t.Fatal(err)
	}
	man := Manifest{Schema: ManifestSchema, Name: "x", Scope: "project:x", Version: 1, Entries: []ManifestEntry{{ID: "Safe", Blob: blob, Size: int64(len(ct))}, {ID: "safe", Blob: blob, Size: int64(len(ct))}}}
	putTestManifest(t, f, man, 1)
	_, _, err = FetchBucket(f.ctx, f.store, f.bc, f.sub(), []age.Identity{f.id}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "differ only by case") {
		t.Fatalf("error=%v", err)
	}
}
