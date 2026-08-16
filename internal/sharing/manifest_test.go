package sharing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

const testLocator = "git@example.test:me/vault.git"

func TestShareSigningKeyStableAndCreated(t *testing.T) {
	configDir := t.TempDir()
	k1, err := SigningKey(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(k1) != ed25519.PrivateKeySize {
		t.Fatalf("signing key size = %d, want %d", len(k1), ed25519.PrivateKeySize)
	}
	if _, err := os.Stat(SigningKeyPath(configDir)); err != nil {
		t.Fatalf("signing key not persisted: %v", err)
	}
	// A second load returns the SAME key — it is the only thing that can sign
	// manifests subscribers have pinned; regenerating would lock them out.
	k2, err := SigningKey(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("signing key changed between loads")
	}
}

func TestSealVerifyRoundtrip(t *testing.T) {
	priv, _ := SigningKey(t.TempDir())
	pub := priv.Public().(ed25519.PublicKey)
	env, err := SealManifest(priv, testLocator, []byte(`{"schema":2,"version":5}`), 5, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if env.Encrypted {
		t.Fatal("git-style envelope should be plaintext")
	}
	if err := VerifyEnvelope(env, testLocator, pub, 0); err != nil {
		t.Fatalf("verify with matching pin: %v", err)
	}
	// First contact (no pin yet) must also pass — the pin is recorded FROM this.
	if err := VerifyEnvelope(env, testLocator, nil, 0); err != nil {
		t.Fatalf("first-contact verify: %v", err)
	}
}

func TestVerifyRejectsTamperAndVersionForgery(t *testing.T) {
	priv, _ := SigningKey(t.TempDir())
	pub := priv.Public().(ed25519.PublicKey)
	env, _ := SealManifest(priv, testLocator, []byte("hello"), 3, nil, false)

	tampered := env
	tampered.Payload = []byte("hellp")
	if err := VerifyEnvelope(tampered, testLocator, pub, 0); err == nil {
		t.Fatal("payload tamper accepted")
	}
	// The version lives outside the ciphertext but is bound into the signature —
	// bumping it (a replay dressed as fresh) must break verification.
	forged := env
	forged.Version = 99
	if err := VerifyEnvelope(forged, testLocator, pub, 0); err == nil {
		t.Fatal("version forgery accepted")
	}
}

func TestVerifyRejectsRollback(t *testing.T) {
	priv, _ := SigningKey(t.TempDir())
	pub := priv.Public().(ed25519.PublicKey)
	env, _ := SealManifest(priv, testLocator, []byte("x"), 2, nil, false)
	if err := VerifyEnvelope(env, testLocator, pub, 5); err == nil {
		t.Fatal("rollback (v2 served when v5 already seen) accepted")
	}
	if err := VerifyEnvelope(env, testLocator, pub, 2); err != nil {
		t.Fatalf("equal version should be accepted (idempotent re-pull): %v", err)
	}
}

func TestVerifyRejectsKeyRotation(t *testing.T) {
	priv, _ := SigningKey(t.TempDir())
	env, _ := SealManifest(priv, testLocator, []byte("x"), 1, nil, false)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := VerifyEnvelope(env, testLocator, otherPub, 0); !errors.Is(err, ErrShareKeyRotated) {
		t.Fatalf("expected ErrShareKeyRotated, got %v", err)
	}
}

// TestVerifyRejectsCrossShareReplay is the regression for the P2 the locator
// binding fixes: one machine signs every share with the same key, so a genuine,
// validly-signed envelope from share A — replayed at share B's locator by an
// attacker who can write B's (ACL-less) destination — must still be refused.
func TestVerifyRejectsCrossShareReplay(t *testing.T) {
	priv, _ := SigningKey(t.TempDir())
	pub := priv.Public().(ed25519.PublicKey)
	env, _ := SealManifest(priv, "s3://bucket/acme", []byte("A's content"), 4, nil, false)

	if err := VerifyEnvelope(env, "s3://bucket/acme", pub, 0); err != nil {
		t.Fatalf("own-locator verify should pass: %v", err)
	}
	if err := VerifyEnvelope(env, "s3://bucket/beta", pub, 0); err == nil {
		t.Fatal("A's manifest served at B's locator was accepted — cross-share replay")
	}
}

func TestSealManifestEncryptsForPublicLocator(t *testing.T) {
	priv, _ := SigningKey(t.TempDir())
	pub := priv.Public().(ed25519.PublicKey)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"schema":2,"scope":"project:acme","version":7}`)
	env, err := SealManifest(priv, "s3://bucket/acme", manifest, 7, []age.Recipient{id.Recipient()}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !env.Encrypted {
		t.Fatal("public-locator envelope must be encrypted")
	}
	if bytes.Contains(env.Payload, []byte("project:acme")) {
		t.Fatal("scope leaked in the encrypted payload — a URL holder would see it")
	}
	if err := VerifyEnvelope(env, "s3://bucket/acme", pub, 0); err != nil {
		t.Fatalf("verify (before decrypt): %v", err)
	}
	got, err := OpenManifestPayload(env, []age.Identity{id})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, manifest) {
		t.Fatalf("decrypted manifest mismatch: %q", got)
	}
	// A non-recipient cannot open it.
	other, _ := age.GenerateX25519Identity()
	if _, err := OpenManifestPayload(env, []age.Identity{other}); err == nil {
		t.Fatal("non-recipient decrypted the manifest")
	}
}

func TestBlobKeyAndAllowlist(t *testing.T) {
	a := BlobKey([]byte("abc"))
	if len(a) != 64 {
		t.Fatalf("BlobKey length = %d, want 64", len(a))
	}
	if a != BlobKey([]byte("abc")) {
		t.Fatal("BlobKey not deterministic")
	}
	if a == BlobKey([]byte("abd")) {
		t.Fatal("BlobKey collided on distinct input")
	}
	if !BlobKeyRE.MatchString(BlobObjectName([]byte("abc"))) {
		t.Fatalf("allowlist rejected a valid blob object name %q", BlobObjectName([]byte("abc")))
	}
	for _, bad := range []string{"../evil.age", "note.md", "deadbeef.age", "ABC.age", a + ".md", a} {
		if BlobKeyRE.MatchString(bad) {
			t.Fatalf("allowlist accepted a non-ciphertext key %q", bad)
		}
	}
}

func TestSignPubFingerprintStableAndDistinct(t *testing.T) {
	p1, _, _ := ed25519.GenerateKey(rand.Reader)
	p2, _, _ := ed25519.GenerateKey(rand.Reader)
	// Two calls compared via vars (not f(x) != f(x)) so the stability check isn't
	// flagged as an identical-expression comparison (staticcheck SA4000).
	if a, b := SignPubFingerprint(p1), SignPubFingerprint(p1); a != b {
		t.Fatal("fingerprint not stable")
	}
	if SignPubFingerprint(p1) == SignPubFingerprint(p2) {
		t.Fatal("fingerprint collision across distinct keys")
	}
	if SignPubFingerprint(p1) == "" {
		t.Fatal("empty fingerprint")
	}
}

func TestManifestFailClosedEdges(t *testing.T) {
	if _, err := EncryptBytes(nil, []byte("x")); err == nil {
		t.Fatal("encryption without recipients succeeded")
	}
	if _, err := ReadSigningKey(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing key error=%v", err)
	}
	malformed := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(malformed, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSigningKey(malformed); err == nil {
		t.Fatal("malformed key accepted")
	}
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SigningKey(notDir); err == nil {
		t.Fatal("non-directory config accepted")
	}
	if err := VerifyEnvelope(Envelope{}, testLocator, nil, 0); err == nil {
		t.Fatal("malformed signer accepted")
	}
	priv, _ := SigningKey(t.TempDir())
	env, _ := SealManifest(priv, testLocator, []byte("x"), 1, nil, false)
	env.Sig = env.Sig[:1]
	if err := VerifyEnvelope(env, testLocator, nil, 0); err == nil {
		t.Fatal("malformed signature accepted")
	}
	tooLarge := Envelope{Payload: make([]byte, MaxManifestBytes+1)}
	if _, err := OpenManifestPayload(tooLarge, nil); err == nil {
		t.Fatal("oversized plaintext manifest accepted")
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	huge := make([]byte, MaxManifestBytes+1)
	sealed, err := SealManifest(priv, testLocator, huge, 2, []age.Recipient{id.Recipient()}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenManifestPayload(sealed, []age.Identity{id}); err == nil {
		t.Fatal("oversized decrypted manifest accepted")
	}
}
