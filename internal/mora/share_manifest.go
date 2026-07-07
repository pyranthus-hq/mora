package mora

// Signed manifest + content-addressing for `mora share` (Phase 2).
//
// v1's git transport gets its guarantees from git itself: authenticity from the
// remote's write-ACL, freshness from `git pull --ff-only` (no non-fast-forward),
// and ciphertext-only egress from the `git ls-files` allowlist. A user-owned
// bucket or a relay has NONE of those, so the same guarantees must travel inside
// the data. This file builds that machinery — a signed, optionally-encrypted
// manifest that names the current set by content hash — as a transport-neutral,
// unit-tested library. The git path is deliberately unchanged (it already has
// equivalents); BucketTransport (Phase 3) is the first consumer.
//
// The three checks, in the order a subscriber MUST apply them on pull:
//  1. authenticity — the manifest is ed25519-signed by the publisher's key;
//  2. identity     — that key matches the one TOFU-pinned at subscribe time;
//  3. freshness    — the manifest's monotonic version is >= the last one seen
//                    (a signature proves who wrote it, NOT that it is current —
//                    this is what replaces git's --ff-only for other backends).
// Only after all three pass are any blobs fetched, hash-checked, and decrypted.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"filippo.io/age"
)

const shareManifestV2Schema = 2

// manifestEntry names one memory in the current set. Blob is the content address
// (hex sha256 of the ciphertext = its storage key), so an update yields a new key
// and readers never race an in-place overwrite.
type manifestEntry struct {
	ID   string `json:"id"`
	Blob string `json:"blob"`
	Size int64  `json:"size"`
}

// shareManifestV2 is the full current set for one publisher/scope. It carries no
// plaintext memory content — only the content-addressed index — and on a public
// locator it is itself age-encrypted inside the envelope, so a URL/relay holder
// sees neither ids nor sizes.
type shareManifestV2 struct {
	Schema      int             `json:"schema"`
	Name        string          `json:"name"`
	Scope       string          `json:"scope"`
	Owner       string          `json:"owner,omitempty"`
	Client      string          `json:"client"`
	Version     int             `json:"version"`
	PrevVersion int             `json:"prev_version"`
	UpdatedAt   string          `json:"updated_at"`
	Entries     []manifestEntry `json:"entries"`
}

// signedEnvelope wraps the manifest for transport. Version is mirrored OUTSIDE
// the (possibly encrypted) payload so freshness is checkable before any decrypt,
// and it is bound into the signature so it cannot be altered independently.
type signedEnvelope struct {
	Payload   []byte `json:"payload"`
	Encrypted bool   `json:"enc"`
	Version   int    `json:"version"`
	SignPub   []byte `json:"spk"` // publisher ed25519 public key
	Sig       []byte `json:"sig"` // ed25519 over manifestSigningMessage(Payload, Version)
}

// shareBlobKeyRE gates object keys on a bucket/relay to content-addressed
// ciphertext only — the namespace equivalent of git's ls-files allowlist.
var shareBlobKeyRE = regexp.MustCompile(`^[0-9a-f]{64}\.age$`)

// blobKey is the content address of a ciphertext blob: hex(sha256). Update =>
// new key (no in-place overwrite), so eventually-consistent stores and immutable
// event logs both behave.
func blobKey(ciphertext []byte) string {
	sum := sha256.Sum256(ciphertext)
	return hex.EncodeToString(sum[:])
}

// blobObjectName is the on-store name for a blob ("<hash>.age").
func blobObjectName(ciphertext []byte) string { return blobKey(ciphertext) + ".age" }

func shareSigningKeyPath(cfg Config) string {
	return filepath.Join(cfg.ConfigDir, "share", "signing.key")
}

// shareSigningKey loads (or, on first use, creates) the publisher's ed25519
// signing identity. It authenticates every manifest this machine publishes; the
// public half rides in the envelope and is TOFU-pinned by subscribers. It is a
// secret — 0600, beside the age identity, never in any share repo/bucket — and
// distinct from the age READ recipients (who can decrypt) by design: "who can
// write authoritatively" != "who can read". The user never types it.
func shareSigningKey(cfg Config) (ed25519.PrivateKey, error) {
	path := shareSigningKeyPath(cfg)
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("share signing key %s is malformed (%d bytes, want %d) — remove it to regenerate", path, len(b), ed25519.PrivateKeySize)
		}
		return ed25519.PrivateKey(b), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := atomicWrite(path, priv, 0o600); err != nil {
		return nil, err
	}
	return priv, nil
}

// manifestSigningMessage binds the version (which lives outside the ciphertext
// so it can be read before decryption) into the signed bytes, under a domain-
// separation tag, so neither the version nor the payload can be altered without
// invalidating the signature.
func manifestSigningMessage(payload []byte, version int) []byte {
	msg := make([]byte, 0, len(payload)+32)
	msg = append(msg, "mora-share-manifest-v2\x00"...)
	msg = binary.BigEndian.AppendUint64(msg, uint64(version))
	msg = append(msg, 0)
	return append(msg, payload...)
}

// sealManifest signs the manifest (and, for a public locator, age-encrypts it to
// the recipients first so ids/sizes never leak). encrypt=false keeps it plaintext
// for a private git repo, matching v1.
func sealManifest(priv ed25519.PrivateKey, manifestJSON []byte, version int, recipients []age.Recipient, encrypt bool) (signedEnvelope, error) {
	payload := manifestJSON
	if encrypt {
		ct, err := encryptShareBytes(recipients, manifestJSON)
		if err != nil {
			return signedEnvelope{}, err
		}
		payload = ct
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return signedEnvelope{}, errors.New("share manifest: signing key has no ed25519 public half")
	}
	return signedEnvelope{
		Payload:   payload,
		Encrypted: encrypt,
		Version:   version,
		SignPub:   pub,
		Sig:       ed25519.Sign(priv, manifestSigningMessage(payload, version)),
	}, nil
}

var errShareKeyRotated = errors.New("share manifest: signing key changed since subscribe — the publisher rotated their share (or this is an impostor); remove and re-subscribe")

// verifyEnvelope enforces authenticity, identity (TOFU pin), and freshness before
// any blob is fetched or the payload decrypted. pinnedPub is the key recorded at
// subscribe time (empty on the very first contact); lastVersion is the highest
// version this subscriber has already accepted.
func verifyEnvelope(env signedEnvelope, pinnedPub ed25519.PublicKey, lastVersion int) error {
	if len(env.SignPub) != ed25519.PublicKeySize {
		return errors.New("share manifest: malformed signing key")
	}
	if len(env.Sig) != ed25519.SignatureSize ||
		!ed25519.Verify(ed25519.PublicKey(env.SignPub), manifestSigningMessage(env.Payload, env.Version), env.Sig) {
		return errors.New("share manifest: signature does not verify — refusing (tampered payload or wrong signer)")
	}
	if len(pinnedPub) != 0 && !bytes.Equal(env.SignPub, pinnedPub) {
		return errShareKeyRotated
	}
	if env.Version < lastVersion {
		return fmt.Errorf("share manifest: served version %d is older than the last seen %d — refusing (rollback/replay)", env.Version, lastVersion)
	}
	return nil
}

// openManifestPayload returns the manifest JSON, decrypting it first if the
// envelope was sealed for a public locator. Call ONLY after verifyEnvelope.
func openManifestPayload(env signedEnvelope, identities []age.Identity) ([]byte, error) {
	if !env.Encrypted {
		return env.Payload, nil
	}
	r, err := age.Decrypt(bytes.NewReader(env.Payload), identities...)
	if err != nil {
		return nil, fmt.Errorf("decrypting share manifest: %w (is your key among this share's recipients?)", err)
	}
	return io.ReadAll(io.LimitReader(r, shareMaxMemoryBytes))
}

// signPubFingerprint is the short, human-verifiable string a subscriber confirms
// out of band (`--confirm-pin`) so a pasted bucket URL / npub can be authenticated
// on first contact rather than blindly TOFU-pinned. Lowercase base32 of a
// SHA-256 prefix — enough bits to defeat a preimage, short enough to read aloud.
func signPubFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:10])
	return strings.ToLower(enc)
}
