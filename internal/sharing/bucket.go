package sharing

// BucketTransport — publish/subscribe a share over a user-owned S3-compatible
// bucket (Cloudflare R2, Backblaze B2, MinIO, …), Phase 3.
//
// A bucket has no write-ACL, no --ff-only, and its URL is a read-all bearer
// token, so every guarantee git got from the remote must travel in the data.
// This transport uses the Phase 2 manifest machinery to provide them:
//   - authenticity/identity  — the manifest is ed25519-signed by the publisher
//     and TOFU-pinned by the subscriber (VerifyEnvelope);
//   - freshness              — a monotonic version replaces --ff-only;
//   - confidentiality        — blobs AND the manifest are age-encrypted to the
//     recipients, so a URL holder sees only opaque ciphertext (not ids/sizes);
//   - ciphertext-only egress — a prefix audit (git's ls-files allowlist, ported)
//     refuses to publish if any non-ciphertext object sits under the prefix.
//
// The store is abstracted behind ObjectStore so the logic is testable against an
// in-memory fake; s3Store (share_bucket_s3.go) is the real adapter.
//
// Publish order is the manifest-pointer commit: write content-addressed blobs →
// audit the prefix → flip the signed manifest (the linearization point) → delete
// orphaned blobs. Fetch materializes a v1-layout dir (memories/<id>.md.age +
// share.json) that the existing shareImport validates and indexes unchanged.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/memory"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
)

// transportRef is the ledger discriminator: absent (nil) ⇒ git (v1), so existing
// shares.json rows parse and behave unchanged.

// BucketConfig locates an S3-compatible destination. Credentials are NEVER stored
// here — SecretRef names where they live (env), matching v1's PAT-redaction posture.

// objectPrefix is the key prefix (with a trailing slash) all of this share's
// objects live under, so many shares can share one bucket.

// locator is the stable canonical destination bound into every manifest signature
// (see manifestSigningMessage) so a manifest signed for this bucket/prefix cannot
// be replayed at another. Field-separated to stay unambiguous.

const ManifestObject = "manifest"

// MaxShareEntries caps how many memories one share may name, bounding a
// trusted-but-compromised (validly-signed) publisher's ability to make a
// subscriber download an unbounded set on a single pull.
const MaxShareEntries = 50000

// ObjectStore is the minimal S3 surface BucketTransport needs. Implementations:
// s3Store (real) and a test fake. GetObject returns ErrObjectNotFound for an
// absent key so callers can distinguish "not there yet" from a real failure.
type ObjectStore interface {
	PutObject(ctx context.Context, key string, data []byte) error
	GetObject(ctx context.Context, key string) ([]byte, error)
	ListKeys(ctx context.Context, prefix string) ([]string, error)
	DeleteObject(ctx context.Context, key string) error
}

var (
	ErrObjectNotFound = errors.New("share bucket: object not found")
	ErrNoManifest     = errors.New("share bucket: no manifest yet")
)

// CurrentEnvelope reads the current signed manifest envelope, or ErrNoManifest
// if the share has never been published.
func CurrentEnvelope(ctx context.Context, store ObjectStore, prefix string) (Envelope, error) {
	b, err := store.GetObject(ctx, prefix+ManifestObject)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			return Envelope{}, ErrNoManifest
		}
		return Envelope{}, err
	}
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return Envelope{}, fmt.Errorf("share bucket: manifest is not a valid envelope: %w", err)
	}
	return env, nil
}

// PublishBucket encrypts the current scope, content-addresses each blob, seals a
// signed+encrypted manifest one version past whatever the remote advertises (so a
// lost local state can't regress subscribers), then commits in manifest-pointer
// order. v1 re-encrypts the full set each push (authored notes are small); dedup
// is a later optimization.
func PublishBucket(ctx context.Context, store ObjectStore, cfg BucketConfig, pub Publish, mems []memory.Memory, priv ed25519.PrivateKey, recips []age.Recipient, client string) error {
	prefix := cfg.ObjectPrefix()

	version := 1
	if cur, err := CurrentEnvelope(ctx, store, prefix); err == nil {
		version = cur.Version + 1
	} else if !errors.Is(err, ErrNoManifest) {
		return err
	}

	blobs := make(map[string][]byte, len(mems)) // objectName ("<hash>.age") -> ciphertext
	entries := make([]ManifestEntry, 0, len(mems))
	for _, m := range mems {
		plain, err := os.ReadFile(m.Path)
		if err != nil {
			return err
		}
		ct, err := EncryptBytes(recips, plain)
		if err != nil {
			return fmt.Errorf("encrypting %s: %w", m.ID, err)
		}
		name := BlobObjectName(ct)
		blobs[name] = ct
		entries = append(entries, ManifestEntry{ID: m.ID, Blob: BlobKey(ct), Size: int64(len(ct))})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })

	man := Manifest{
		Schema: ManifestSchema, Name: pub.Name, Scope: pub.Scope, Owner: pub.Owner,
		Client: client, Version: version, PrevVersion: version - 1,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339), Entries: entries,
	}
	manJSON, err := json.Marshal(man)
	if err != nil {
		return err
	}
	env, err := SealManifest(priv, cfg.Locator(), manJSON, version, recips, true)
	if err != nil {
		return err
	}
	envBytes, err := json.Marshal(env)
	if err != nil {
		return err
	}

	// 1. blobs first — content-addressed, so an interrupted publish leaves only
	//    unreferenced orphans, never a manifest pointing at a missing blob.
	for name, ct := range blobs {
		if err := store.PutObject(ctx, prefix+name, ct); err != nil {
			return err
		}
	}
	// 2. ciphertext-only egress audit (git's ls-files allowlist, ported).
	if err := EgressAudit(ctx, store, prefix); err != nil {
		return err
	}
	// 3. flip the manifest — the single linearization point.
	if err := store.PutObject(ctx, prefix+ManifestObject, envBytes); err != nil {
		return err
	}
	// 4. delete now-orphaned blobs from earlier versions.
	return DeleteOrphans(ctx, store, prefix, blobs)
}

// EgressAudit refuses to proceed if any object under the prefix is not the
// manifest or a content-addressed ciphertext blob — the whole-namespace guarantee
// git got from `git ls-files`, so a stray plaintext a URL holder could read never
// slips out. Orphaned ciphertext from a prior version is allowed here (it is still
// ciphertext) and cleaned up by DeleteOrphans.
func EgressAudit(ctx context.Context, store ObjectStore, prefix string) error {
	keys, err := store.ListKeys(ctx, prefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		base := strings.TrimPrefix(k, prefix)
		if base == ManifestObject || BlobKeyRE.MatchString(base) {
			continue
		}
		return fmt.Errorf("refusing to publish: bucket prefix holds a non-ciphertext object %q — remove it before publishing", k)
	}
	return nil
}

func DeleteOrphans(ctx context.Context, store ObjectStore, prefix string, keep map[string][]byte) error {
	keys, err := store.ListKeys(ctx, prefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		base := strings.TrimPrefix(k, prefix)
		if base == ManifestObject {
			continue
		}
		if _, ok := keep[base]; ok {
			continue
		}
		if BlobKeyRE.MatchString(base) {
			if err := store.DeleteObject(ctx, k); err != nil {
				return err
			}
		}
	}
	return nil
}

// FetchBucket verifies the signed manifest (authenticity → TOFU pin → freshness),
// downloads and hash-checks every named blob, and materializes a v1-layout dir
// (memories/<id>.md.age + share.json) into destDir for shareImport to validate and
// index. Nothing is written to the real corpus here; a mid-fetch failure just
// discards the throwaway destDir. Returns the pin + version to persist on success.
func FetchBucket(ctx context.Context, store ObjectStore, cfg BucketConfig, sub Subscription, ids []age.Identity, destDir string) (ed25519.PublicKey, int, error) {
	prefix := cfg.ObjectPrefix()

	env, err := CurrentEnvelope(ctx, store, prefix)
	if err != nil {
		if errors.Is(err, ErrNoManifest) {
			return nil, 0, fmt.Errorf("subscription %q: the share has no manifest yet — has the publisher pushed?", sub.Name)
		}
		return nil, 0, err
	}
	if err := VerifyEnvelope(env, cfg.Locator(), sub.PinnedPubkey, sub.LastVersion); err != nil {
		return nil, 0, fmt.Errorf("subscription %q: %w", sub.Name, err)
	}
	manJSON, err := OpenManifestPayload(env, ids)
	if err != nil {
		return nil, 0, fmt.Errorf("subscription %q: %w", sub.Name, err)
	}
	var man Manifest
	if err := json.Unmarshal(manJSON, &man); err != nil {
		return nil, 0, fmt.Errorf("subscription %q: share manifest is not valid JSON: %w", sub.Name, err)
	}
	if man.Schema != ManifestSchema {
		return nil, 0, fmt.Errorf("subscription %q: manifest schema %d unsupported (want %d) — upgrade mora", sub.Name, man.Schema, ManifestSchema)
	}
	// The inner and outer versions are both under the signature; a mismatch means a
	// malformed publisher, not an attacker, but refuse rather than trust either.
	if man.Version != env.Version {
		return nil, 0, fmt.Errorf("subscription %q: manifest inner version %d != envelope %d — refusing", sub.Name, man.Version, env.Version)
	}
	if !ValidScope(man.Scope) {
		return nil, 0, fmt.Errorf("subscription %q: manifest declares invalid scope %q", sub.Name, man.Scope)
	}

	if len(man.Entries) > MaxShareEntries {
		return nil, 0, fmt.Errorf("subscription %q: manifest names %d entries, over the %d cap — refusing", sub.Name, len(man.Entries), MaxShareEntries)
	}
	memDir := filepath.Join(destDir, "memories")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		return nil, 0, err
	}
	foldSeen := make(map[string]string, len(man.Entries))
	for _, e := range man.Entries {
		if !ExportIDRE.MatchString(e.ID) {
			return nil, 0, fmt.Errorf("subscription %q: manifest entry id %q is unsafe — refusing", sub.Name, e.ID)
		}
		if !BlobKeyRE.MatchString(e.Blob + ".age") {
			return nil, 0, fmt.Errorf("subscription %q: manifest entry %q has a malformed blob reference %q", sub.Name, e.ID, e.Blob)
		}
		if prior, dup := foldSeen[strings.ToLower(e.ID)]; dup {
			return nil, 0, fmt.Errorf("subscription %q: manifest ids %s and %s differ only by case", sub.Name, prior, e.ID)
		}
		foldSeen[strings.ToLower(e.ID)] = e.ID
		if e.Size < 0 || e.Size > MaxMemoryBytes+(1<<20) {
			return nil, 0, fmt.Errorf("subscription %q: manifest entry %s declares an out-of-range size %d", sub.Name, e.ID, e.Size)
		}
		ct, err := store.GetObject(ctx, prefix+e.Blob+".age")
		if err != nil {
			if errors.Is(err, ErrObjectNotFound) {
				return nil, 0, fmt.Errorf("subscription %q: the blob for %s is missing — the share may be mid-update; retry `mora share pull %s`", sub.Name, e.ID, sub.Name)
			}
			return nil, 0, err
		}
		if int64(len(ct)) != e.Size {
			return nil, 0, fmt.Errorf("subscription %q: the blob for %s is %d bytes but the manifest declared %d — refusing", sub.Name, e.ID, len(ct), e.Size)
		}
		if BlobKey(ct) != e.Blob {
			return nil, 0, fmt.Errorf("subscription %q: the blob for %s failed its content-hash check — refusing", sub.Name, e.ID)
		}
		if err := atomicio.Write(filepath.Join(memDir, e.ID+".md.age"), ct, 0o644); err != nil {
			return nil, 0, err
		}
	}
	// A v1-schema share.json so the existing shareImport reads this exactly as it
	// reads a git clone.
	v1 := ExportManifest{
		Schema: ExportManifestSchema, Name: man.Name, Scope: man.Scope, Owner: man.Owner,
		CreatedAt: man.UpdatedAt, Client: man.Client,
	}
	mb, err := json.MarshalIndent(v1, "", "  ")
	if err != nil {
		return nil, 0, err
	}
	if err := atomicio.Write(filepath.Join(destDir, "share.json"), append(mb, '\n'), 0o644); err != nil {
		return nil, 0, err
	}
	return env.SignPub, env.Version, nil
}
