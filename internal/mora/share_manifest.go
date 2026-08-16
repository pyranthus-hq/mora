package mora

import (
	"crypto/ed25519"
	"filippo.io/age"
	sharingpkg "github.com/pyranthus-hq/mora/internal/sharing"
)

const (
	shareManifestV2Schema = sharingpkg.ManifestSchema
	shareMaxManifestBytes = sharingpkg.MaxManifestBytes
)

type manifestEntry = sharingpkg.ManifestEntry
type shareManifestV2 = sharingpkg.Manifest
type signedEnvelope = sharingpkg.Envelope

var shareBlobKeyRE = sharingpkg.BlobKeyRE
var errShareKeyRotated = sharingpkg.ErrShareKeyRotated

func blobKey(ciphertext []byte) string        { return sharingpkg.BlobKey(ciphertext) }
func blobObjectName(ciphertext []byte) string { return sharingpkg.BlobObjectName(ciphertext) }
func shareSigningKeyPath(cfg Config) string   { return sharingpkg.SigningKeyPath(cfg.ConfigDir) }
func shareSigningKey(cfg Config) (ed25519.PrivateKey, error) {
	return sharingpkg.SigningKey(cfg.ConfigDir)
}
func readSigningKey(path string) (ed25519.PrivateKey, error) { return sharingpkg.ReadSigningKey(path) }
func sealManifest(priv ed25519.PrivateKey, locator string, body []byte, version int, recipients []age.Recipient, encrypt bool) (signedEnvelope, error) {
	return sharingpkg.SealManifest(priv, locator, body, version, recipients, encrypt)
}
func verifyEnvelope(env signedEnvelope, locator string, pinned ed25519.PublicKey, lastVersion int) error {
	return sharingpkg.VerifyEnvelope(env, locator, pinned, lastVersion)
}
func openManifestPayload(env signedEnvelope, identities []age.Identity) ([]byte, error) {
	return sharingpkg.OpenManifestPayload(env, identities)
}
func signPubFingerprint(pub ed25519.PublicKey) string { return sharingpkg.SignPubFingerprint(pub) }

func encryptShareBytes(recipients []age.Recipient, plaintext []byte) ([]byte, error) {
	return sharingpkg.EncryptBytes(recipients, plaintext)
}
