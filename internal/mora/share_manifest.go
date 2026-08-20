package mora

import (
	"crypto/ed25519"
	"filippo.io/age"
	sharingpkg "github.com/pyranthus-hq/mora/internal/sharing"
)

var errShareKeyRotated = sharingpkg.ErrShareKeyRotated

func shareSigningKeyPath(cfg Config) string { return sharingpkg.SigningKeyPath(cfg.ConfigDir) }
func shareSigningKey(cfg Config) (ed25519.PrivateKey, error) {
	return sharingpkg.SigningKey(cfg.ConfigDir)
}
func readSigningKey(path string) (ed25519.PrivateKey, error) { return sharingpkg.ReadSigningKey(path) }

func signPubFingerprint(pub ed25519.PublicKey) string { return sharingpkg.SignPubFingerprint(pub) }

func encryptShareBytes(recipients []age.Recipient, plaintext []byte) ([]byte, error) {
	return sharingpkg.EncryptBytes(recipients, plaintext)
}
