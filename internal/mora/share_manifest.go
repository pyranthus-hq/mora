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
	if k, err := readSigningKey(path); err == nil {
		return k, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	// O_EXCL so two concurrent first-time publishes CONVERGE on one key: the
	// loser reads the winner's key rather than returning the throwaway it just
	// generated. A last-writer-wins atomicWrite would leave the loser signing
	// with a key its subscribers never pinned — a silent, permanent lock-out.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return readSigningKey(path)
		}
		return nil, err
	}
	if _, werr := f.Write(priv); werr != nil {
		_ = f.Close()
		return nil, werr
	}
	if cerr := f.Close(); cerr != nil {
		return nil, cerr
	}
	return priv, nil
}

func readSigningKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("share signing key %s is malformed (%d bytes, want %d) — remove it to regenerate", path, len(b), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(b), nil
}

// manifestSigningMessage binds the LOCATOR and the version — both of which live
// outside the (optionally encrypted) payload — into the signed bytes under a
// domain-separation tag. Binding the locator is load-bearing: one machine signs
// ALL its shares with the same key, so without it a genuine, validly-signed
// envelope from share A can be replayed at share B's destination (a bucket/relay
// has no write-ACL) — passing signature, pin, and freshness while confusing
// content and poisoning B's version counter. The 32-byte fixed prefix plus a
// length-prefixed locator keeps the map (locator, version, payload) -> message
// injective, so no two distinct inputs collide.
func manifestSigningMessage(locator string, payload []byte, version int) []byte {
	var msg []byte // append grows safely without overflow-prone attacker-controlled capacity arithmetic
	msg = append(msg, "mora-share-manifest-v2\x00"...)
	msg = binary.BigEndian.AppendUint64(msg, uint64(len(locator)))
	msg = append(msg, locator...)
	msg = binary.BigEndian.AppendUint64(msg, uint64(version))
	return append(msg, payload...)
}

// sealManifest signs the manifest for a specific locator (and, for a public
// locator, age-encrypts it to the recipients first so ids/sizes never leak).
// encrypt=false keeps it plaintext for a private git repo, matching v1. The
// locator must be the stable canonical destination (git remote, or bucket
// endpoint+bucket+prefix) so verifyEnvelope can reject a cross-share replay.
func sealManifest(priv ed25519.PrivateKey, locator string, manifestJSON []byte, version int, recipients []age.Recipient, encrypt bool) (signedEnvelope, error) {
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
		Sig:       ed25519.Sign(priv, manifestSigningMessage(locator, payload, version)),
	}, nil
}

var errShareKeyRotated = errors.New("share manifest: signing key changed since subscribe — the publisher rotated their share (or this is an impostor); remove and re-subscribe")

// verifyEnvelope enforces authenticity, identity (TOFU pin), and freshness before
// any blob is fetched or the payload decrypted. locator is the destination the
// subscriber pulled from (bound into the signature, so a manifest signed for a
// different share/destination is rejected here). pinnedPub is the key recorded at
// subscribe time (empty on the very first contact); lastVersion is the highest
// version this subscriber has already accepted.
func verifyEnvelope(env signedEnvelope, locator string, pinnedPub ed25519.PublicKey, lastVersion int) error {
	if len(env.SignPub) != ed25519.PublicKeySize {
		return errors.New("share manifest: malformed signing key")
	}
	if len(env.Sig) != ed25519.SignatureSize ||
		!ed25519.Verify(ed25519.PublicKey(env.SignPub), manifestSigningMessage(locator, env.Payload, env.Version), env.Sig) {
		return errors.New("share manifest: signature does not verify — refusing (tampered payload, wrong signer, or wrong destination)")
	}
	if len(pinnedPub) != 0 && !bytes.Equal(env.SignPub, pinnedPub) {
		return errShareKeyRotated
	}
	if env.Version < lastVersion {
		return fmt.Errorf("share manifest: served version %d is older than the last seen %d — refusing (rollback/replay)", env.Version, lastVersion)
	}
	return nil
}

// shareMaxManifestBytes caps the manifest — the index of the WHOLE set, so much
// larger than one memory (shareMaxMemoryBytes). A legit share won't approach it;
// anything past it is malformed/hostile and is refused loudly, never truncated.
const shareMaxManifestBytes = 16 << 20

// openManifestPayload returns the manifest JSON, decrypting it first if the
// envelope was sealed for a public locator. Call ONLY after verifyEnvelope. Both
// branches bound the size and REFUSE (never silently truncate) an over-cap
// manifest. The transport is still responsible for bounding the envelope object
// itself before it is unmarshalled.
func openManifestPayload(env signedEnvelope, identities []age.Identity) ([]byte, error) {
	if !env.Encrypted {
		if len(env.Payload) > shareMaxManifestBytes {
			return nil, fmt.Errorf("share manifest is %d bytes, over the %d-byte cap — refusing", len(env.Payload), shareMaxManifestBytes)
		}
		return env.Payload, nil
	}
	r, err := age.Decrypt(bytes.NewReader(env.Payload), identities...)
	if err != nil {
		return nil, fmt.Errorf("decrypting share manifest: %w (is your key among this share's recipients?)", err)
	}
	plain, err := io.ReadAll(io.LimitReader(r, shareMaxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(plain) > shareMaxManifestBytes {
		return nil, fmt.Errorf("decrypted share manifest exceeds the %d-byte cap — refusing", shareMaxManifestBytes)
	}
	return plain, nil
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
