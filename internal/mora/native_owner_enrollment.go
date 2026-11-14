package mora

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

const nativeOwnerEnrollmentLifetime = 90 * time.Second
const nativeOwnerWindowsAppIdentity = "windows-pinned-helper-v1"

type nativeOwnerPeer interface{ identity() (string, bool) }

type nativeOwnerEnrollment struct {
	Schema          string `json:"schema"`
	InstallationID  string `json:"installation_id"`
	Host            string `json:"host"`
	AppIdentity     string `json:"app_identity"`
	KeyID           string `json:"key_id"`
	PublicKeyP256   []byte `json:"public_key_p256"`
	CredentialID    string `json:"credential_id,omitempty"`
	EnrolledAt      int64  `json:"enrolled_at"`
	VerifierVersion string `json:"verifier_version"`
}

type nativeOwnerEnrollmentChallenge struct {
	Nonce          string `json:"nonce"`
	ExpiresAt      int64  `json:"expires_at_unix"`
	InstallationID string `json:"installation_id"`
	Host           string `json:"host"`
	AppIdentity    string `json:"app_identity"`
	KeyID          string `json:"key_id"`
	PublicKeyP256  []byte `json:"public_key_p256"`
	CredentialID   string `json:"credential_id,omitempty"`
	ReplaceLegacy  bool   `json:"replace_legacy,omitempty"`
}

type nativeOwnerVerifier struct {
	cfg  Config
	peer nativeOwnerPeer
}

func nativeOwnerDir(cfg Config) string { return filepath.Join(cfg.StateDir, "desktop-review") }
func nativeOwnerEnrollmentPath(cfg Config) string {
	return filepath.Join(nativeOwnerDir(cfg), "enrollment.json")
}
func nativeOwnerEnrollmentChallengePath(cfg Config) string {
	return filepath.Join(nativeOwnerDir(cfg), "enrollment-challenge.json")
}
func nativeOwnerInstallationPath(cfg Config) string {
	return filepath.Join(nativeOwnerDir(cfg), "installation-id")
}
func nativeOwnerEnrollmentLockPath(cfg Config) string {
	return filepath.Join(nativeOwnerDir(cfg), "enrollment.lock")
}
func nativeOwnerInstallationLockPath(cfg Config) string {
	return filepath.Join(nativeOwnerDir(cfg), "installation.lock")
}

func newNativeOwnerVerifierForPeer(cfg Config, peer nativeOwnerPeer) *nativeOwnerVerifier {
	return &nativeOwnerVerifier{cfg: cfg, peer: peer}
}

func (v *nativeOwnerVerifier) TrustedIdentity() (string, string, bool) {
	identity, ok := v.peerTrusted()
	if !ok {
		return "", "", false
	}
	record, err := v.enrollment()
	if err != nil || record.AppIdentity != identity || record.Host != runtime.GOOS {
		return "", "", false
	}
	return record.InstallationID, record.KeyID, true
}

func (v *nativeOwnerVerifier) peerTrusted() (string, bool) { return v.peer.identity() }

func (v *nativeOwnerVerifier) Verify(installationID, keyID string, signed, proof []byte) bool {
	identity, ok := v.peer.identity()
	if !ok {
		return false
	}
	record, err := v.enrollment()
	if err != nil || record.AppIdentity != identity || record.InstallationID != installationID || record.KeyID != keyID {
		return false
	}
	pub, err := nativeOwnerP256(record.PublicKeyP256)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(signed)
	return ecdsa.VerifyASN1(pub, sum[:], proof)
}

func (v *nativeOwnerVerifier) enrollment() (nativeOwnerEnrollment, error) {
	raw, err := os.ReadFile(nativeOwnerEnrollmentPath(v.cfg))
	if err != nil {
		return nativeOwnerEnrollment{}, err
	}
	var record nativeOwnerEnrollment
	if json.Unmarshal(raw, &record) != nil || record.Schema != "mora.review.enrollment" || record.InstallationID == "" || record.KeyID == "" || record.Host == "" || record.AppIdentity == "" || record.VerifierVersion != "native-owner-v1" {
		return nativeOwnerEnrollment{}, errors.New("invalid native owner enrollment")
	}
	pub, err := nativeOwnerP256(record.PublicKeyP256)
	if err != nil {
		return nativeOwnerEnrollment{}, err
	}
	_ = pub
	if record.KeyID != nativeOwnerKeyID(record.Host, record.CredentialID, record.PublicKeyP256) {
		return nativeOwnerEnrollment{}, errors.New("native owner enrollment key mismatch")
	}
	return record, nil
}

func (v *nativeOwnerVerifier) beginEnrollment(publicKey []byte, credentialID string, now time.Time) (challenge nativeOwnerEnrollmentChallenge, signed []byte, err error) {
	identity, trusted := v.peerTrusted()
	if !trusted {
		return challenge, nil, errDesktopReviewProofUnavailable
	}
	if _, err := nativeOwnerP256(publicKey); err != nil {
		return challenge, nil, errors.New("invalid native owner public key")
	}
	err = leasefile.WithGuard(nativeOwnerEnrollmentLockPath(v.cfg), func() error {
		if _, err := v.enrollment(); err == nil {
			return errors.New("native owner is already enrolled")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("existing native owner enrollment is unreadable; preserve it and repair %s: %w", nativeOwnerEnrollmentPath(v.cfg), err)
		}
		installationID, idErr := nativeOwnerInstallationID(v.cfg)
		if idErr != nil {
			return idErr
		}
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		challenge = nativeOwnerEnrollmentChallenge{Nonce: hex.EncodeToString(random[:]), ExpiresAt: now.Add(nativeOwnerEnrollmentLifetime).Unix(), InstallationID: installationID, Host: runtime.GOOS, AppIdentity: identity, KeyID: nativeOwnerKeyID(runtime.GOOS, credentialID, publicKey), PublicKeyP256: append([]byte(nil), publicKey...), CredentialID: credentialID}
		var bytesErr error
		signed, bytesErr = challenge.bytes()
		if bytesErr != nil {
			return bytesErr
		}
		raw, marshalErr := json.Marshal(challenge)
		if marshalErr != nil {
			return marshalErr
		}
		if err := os.MkdirAll(nativeOwnerDir(v.cfg), 0o700); err != nil {
			return err
		}
		return atomicio.WriteDurable(nativeOwnerEnrollmentChallengePath(v.cfg), raw, 0o600)
	})
	return challenge, signed, err
}

func (v *nativeOwnerVerifier) completeEnrollment(nonce string, proof []byte, now time.Time) error {
	identity, trusted := v.peerTrusted()
	if !trusted {
		return errDesktopReviewProofUnavailable
	}
	return leasefile.WithGuard(nativeOwnerEnrollmentLockPath(v.cfg), func() error {
		if _, err := v.enrollment(); err == nil {
			return errDesktopReviewProofUnavailable
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("existing native owner enrollment is unreadable; preserve it and repair %s: %w", nativeOwnerEnrollmentPath(v.cfg), err)
		}
		raw, err := os.ReadFile(nativeOwnerEnrollmentChallengePath(v.cfg))
		if err != nil {
			return errDesktopReviewUnavailable
		}
		var c nativeOwnerEnrollmentChallenge
		if json.Unmarshal(raw, &c) != nil || c.Nonce != nonce || c.ExpiresAt <= now.Unix() || c.Host != runtime.GOOS || c.AppIdentity != identity {
			return errDesktopReviewUnavailable
		}
		bytes, err := c.bytes()
		if err != nil {
			return errDesktopReviewUnavailable
		}
		pub, err := nativeOwnerP256(c.PublicKeyP256)
		if err != nil {
			return errDesktopReviewUnavailable
		}
		sum := sha256.Sum256(bytes)
		if !ecdsa.VerifyASN1(pub, sum[:], proof) {
			return errDesktopReviewProofUnavailable
		}
		record := nativeOwnerEnrollment{Schema: "mora.review.enrollment", InstallationID: c.InstallationID, Host: c.Host, AppIdentity: c.AppIdentity, KeyID: c.KeyID, PublicKeyP256: c.PublicKeyP256, CredentialID: c.CredentialID, EnrolledAt: now.Unix(), VerifierVersion: "native-owner-v1"}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := atomicio.WriteDurable(nativeOwnerEnrollmentPath(v.cfg), encoded, 0o600); err != nil {
			return err
		}
		_ = os.Remove(nativeOwnerEnrollmentChallengePath(v.cfg))
		return nil
	})
}

func (v *nativeOwnerVerifier) cancelEnrollment(nonce string) error {
	if _, trusted := v.peerTrusted(); !trusted {
		return errDesktopReviewProofUnavailable
	}
	return leasefile.WithGuard(nativeOwnerEnrollmentLockPath(v.cfg), func() error {
		raw, err := os.ReadFile(nativeOwnerEnrollmentChallengePath(v.cfg))
		if err != nil {
			return errDesktopReviewUnavailable
		}
		var challenge nativeOwnerEnrollmentChallenge
		if json.Unmarshal(raw, &challenge) != nil || challenge.Nonce != nonce {
			return errDesktopReviewUnavailable
		}
		return os.Remove(nativeOwnerEnrollmentChallengePath(v.cfg))
	})
}

func nativeOwnerInstallationID(cfg Config) (string, error) {
	var installationID string
	err := leasefile.WithGuard(nativeOwnerInstallationLockPath(cfg), func() error {
		path := nativeOwnerInstallationPath(cfg)
		raw, err := os.ReadFile(path)
		if err == nil {
			if len(raw) == 64 {
				if _, err := hex.DecodeString(string(raw)); err == nil {
					installationID = string(raw)
					return nil
				}
			}
			return fmt.Errorf("existing installation ID is invalid; preserve it and repair %s", path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("existing installation ID is unreadable; preserve it and repair %s: %w", path, err)
		}
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		id := hex.EncodeToString(random[:])
		if err := os.MkdirAll(nativeOwnerDir(cfg), 0o700); err != nil {
			return err
		}
		if err := atomicio.WriteDurable(path, []byte(id), 0o600); err != nil {
			return err
		}
		installationID = id
		return nil
	})
	return installationID, err
}

func nativeOwnerP256(value []byte) (*ecdsa.PublicKey, error) {
	if len(value) != 65 || value[0] != 4 {
		return nil, errors.New("invalid p256 public key")
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), value)
	if err != nil {
		return nil, errors.New("invalid p256 public key")
	}
	return pub, nil
}
func nativeOwnerKeyID(host, credentialID string, publicKey []byte) string {
	sum := sha256.Sum256(append(append(append([]byte(host), 0), []byte(credentialID)...), append([]byte{0}, publicKey...)...))
	return hex.EncodeToString(sum[:])
}

func (c nativeOwnerEnrollmentChallenge) bytes() ([]byte, error) {
	if len(c.Nonce) != 64 || c.ExpiresAt <= 0 || c.InstallationID == "" || c.Host == "" || c.AppIdentity == "" || c.KeyID == "" {
		return nil, errors.New("invalid native owner enrollment challenge")
	}
	if _, err := nativeOwnerP256(c.PublicKeyP256); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString("mora-review-enrollment-v1\x00")
	for _, value := range []string{c.Nonce, c.InstallationID, c.Host, c.AppIdentity, c.KeyID, hex.EncodeToString(sha256Bytes(c.PublicKeyP256)), c.CredentialID} {
		if len(value) > 1<<20 {
			return nil, errors.New("native owner enrollment field is too large")
		}
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(value)))
		out.Write(n[:])
		out.WriteString(value)
	}
	var expires [8]byte
	binary.BigEndian.PutUint64(expires[:], uint64(c.ExpiresAt))
	out.Write(expires[:])
	return out.Bytes(), nil
}
func sha256Bytes(value []byte) []byte { sum := sha256.Sum256(value); return sum[:] }
