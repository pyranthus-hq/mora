package mora

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"testing"

	"filippo.io/age"
)

func TestShareSigningKeyStableAndCreated(t *testing.T) {
	cfg := Config{ConfigDir: t.TempDir()}
	k1, err := shareSigningKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(k1) != ed25519.PrivateKeySize {
		t.Fatalf("signing key size = %d, want %d", len(k1), ed25519.PrivateKeySize)
	}
	if _, err := os.Stat(shareSigningKeyPath(cfg)); err != nil {
		t.Fatalf("signing key not persisted: %v", err)
	}
	// A second load returns the SAME key — it is the only thing that can sign
	// manifests subscribers have pinned; regenerating would lock them out.
	k2, err := shareSigningKey(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("signing key changed between loads")
	}
}

func TestSealVerifyRoundtrip(t *testing.T) {
	priv, _ := shareSigningKey(Config{ConfigDir: t.TempDir()})
	pub := priv.Public().(ed25519.PublicKey)
	env, err := sealManifest(priv, []byte(`{"schema":2,"version":5}`), 5, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if env.Encrypted {
		t.Fatal("git-style envelope should be plaintext")
	}
	if err := verifyEnvelope(env, pub, 0); err != nil {
		t.Fatalf("verify with matching pin: %v", err)
	}
	// First contact (no pin yet) must also pass — the pin is recorded FROM this.
	if err := verifyEnvelope(env, nil, 0); err != nil {
		t.Fatalf("first-contact verify: %v", err)
	}
}

func TestVerifyRejectsTamperAndVersionForgery(t *testing.T) {
	priv, _ := shareSigningKey(Config{ConfigDir: t.TempDir()})
	pub := priv.Public().(ed25519.PublicKey)
	env, _ := sealManifest(priv, []byte("hello"), 3, nil, false)

	tampered := env
	tampered.Payload = []byte("hellp")
	if err := verifyEnvelope(tampered, pub, 0); err == nil {
		t.Fatal("payload tamper accepted")
	}
	// The version lives outside the ciphertext but is bound into the signature —
	// bumping it (a replay dressed as fresh) must break verification.
	forged := env
	forged.Version = 99
	if err := verifyEnvelope(forged, pub, 0); err == nil {
		t.Fatal("version forgery accepted")
	}
}

func TestVerifyRejectsRollback(t *testing.T) {
	priv, _ := shareSigningKey(Config{ConfigDir: t.TempDir()})
	pub := priv.Public().(ed25519.PublicKey)
	env, _ := sealManifest(priv, []byte("x"), 2, nil, false)
	if err := verifyEnvelope(env, pub, 5); err == nil {
		t.Fatal("rollback (v2 served when v5 already seen) accepted")
	}
	if err := verifyEnvelope(env, pub, 2); err != nil {
		t.Fatalf("equal version should be accepted (idempotent re-pull): %v", err)
	}
}

func TestVerifyRejectsKeyRotation(t *testing.T) {
	priv, _ := shareSigningKey(Config{ConfigDir: t.TempDir()})
	env, _ := sealManifest(priv, []byte("x"), 1, nil, false)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := verifyEnvelope(env, otherPub, 0); !errors.Is(err, errShareKeyRotated) {
		t.Fatalf("expected errShareKeyRotated, got %v", err)
	}
}

func TestSealManifestEncryptsForPublicLocator(t *testing.T) {
	priv, _ := shareSigningKey(Config{ConfigDir: t.TempDir()})
	pub := priv.Public().(ed25519.PublicKey)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"schema":2,"scope":"project:acme","version":7}`)
	env, err := sealManifest(priv, manifest, 7, []age.Recipient{id.Recipient()}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !env.Encrypted {
		t.Fatal("public-locator envelope must be encrypted")
	}
	if bytes.Contains(env.Payload, []byte("project:acme")) {
		t.Fatal("scope leaked in the encrypted payload — a URL holder would see it")
	}
	if err := verifyEnvelope(env, pub, 0); err != nil {
		t.Fatalf("verify (before decrypt): %v", err)
	}
	got, err := openManifestPayload(env, []age.Identity{id})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, manifest) {
		t.Fatalf("decrypted manifest mismatch: %q", got)
	}
	// A non-recipient cannot open it.
	other, _ := age.GenerateX25519Identity()
	if _, err := openManifestPayload(env, []age.Identity{other}); err == nil {
		t.Fatal("non-recipient decrypted the manifest")
	}
}

func TestBlobKeyAndAllowlist(t *testing.T) {
	a := blobKey([]byte("abc"))
	if len(a) != 64 {
		t.Fatalf("blobKey length = %d, want 64", len(a))
	}
	if a != blobKey([]byte("abc")) {
		t.Fatal("blobKey not deterministic")
	}
	if a == blobKey([]byte("abd")) {
		t.Fatal("blobKey collided on distinct input")
	}
	if !shareBlobKeyRE.MatchString(blobObjectName([]byte("abc"))) {
		t.Fatalf("allowlist rejected a valid blob object name %q", blobObjectName([]byte("abc")))
	}
	for _, bad := range []string{"../evil.age", "note.md", "deadbeef.age", "ABC.age", a + ".md", a} {
		if shareBlobKeyRE.MatchString(bad) {
			t.Fatalf("allowlist accepted a non-ciphertext key %q", bad)
		}
	}
}

func TestSignPubFingerprintStableAndDistinct(t *testing.T) {
	p1, _, _ := ed25519.GenerateKey(rand.Reader)
	p2, _, _ := ed25519.GenerateKey(rand.Reader)
	if signPubFingerprint(p1) != signPubFingerprint(p1) {
		t.Fatal("fingerprint not stable")
	}
	if signPubFingerprint(p1) == signPubFingerprint(p2) {
		t.Fatal("fingerprint collision across distinct keys")
	}
	if signPubFingerprint(p1) == "" {
		t.Fatal("empty fingerprint")
	}
}
