package mora

// This file is deliberately platform-neutral: fixtures must exercise the same
// parser as the Windows build. The Windows adapter is only the trusted pipe.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

const (
	nativeOwnerWindowsRPID   = "mora-review.local"
	nativeOwnerWindowsOrigin = "https://mora-review.local"
	nativeOwnerWebAuthnUP    = byte(0x01)
	nativeOwnerWebAuthnUV    = byte(0x04)
	nativeOwnerWebAuthnAT    = byte(0x40)
)

type nativeOwnerWebAuthnProof struct {
	CredentialID      []byte
	ClientDataJSON    []byte
	AuthenticatorData []byte
	SignatureDER      []byte
}

// completeEnrollmentWebAuthn keeps the existing Go-issued enrollment nonce,
// but derives the saved key and credential only from authenticator data.
func (v *nativeOwnerVerifier) completeEnrollmentWebAuthn(nonce string, proof nativeOwnerWebAuthnProof, now time.Time) error {
	identity, trusted := v.peerTrusted()
	if !trusted {
		return errDesktopReviewProofUnavailable
	}
	return leasefile.WithGuard(nativeOwnerEnrollmentLockPath(v.cfg), func() error {
		if _, err := v.enrollment(); err == nil {
			return errDesktopReviewProofUnavailable
		} else if !errors.Is(err, os.ErrNotExist) {
			return errDesktopReviewUnavailable
		}
		raw, err := os.ReadFile(nativeOwnerEnrollmentChallengePath(v.cfg))
		if err != nil {
			return errDesktopReviewUnavailable
		}
		var c nativeOwnerEnrollmentChallenge
		if json.Unmarshal(raw, &c) != nil || c.Nonce != nonce || c.ExpiresAt <= now.Unix() || c.AppIdentity != identity {
			return errDesktopReviewUnavailable
		}
		signed, err := c.webAuthnBytes()
		if err != nil || nativeOwnerValidateClientData(proof.ClientDataJSON, signed, "webauthn.create") != nil {
			return errDesktopReviewProofUnavailable
		}
		credential, pub, err := nativeOwnerWebAuthnEnrollmentKey(proof.AuthenticatorData)
		if err != nil || subtle.ConstantTimeCompare(credential, proof.CredentialID) != 1 {
			return errDesktopReviewProofUnavailable
		}
		derived := elliptic.Marshal(elliptic.P256(), pub.X, pub.Y)
		credentialID := base64.StdEncoding.EncodeToString(credential)
		record := nativeOwnerEnrollment{Schema: "mora.review.enrollment", InstallationID: c.InstallationID, Host: c.Host, AppIdentity: c.AppIdentity, KeyID: nativeOwnerKeyID(c.Host, credentialID, derived), PublicKeyP256: derived, CredentialID: credentialID, EnrolledAt: now.Unix(), VerifierVersion: "native-owner-v1"}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := atomicio.WriteDurable(nativeOwnerEnrollmentPath(v.cfg), encoded, 0o600); err != nil {
			return err
		}
		return os.Remove(nativeOwnerEnrollmentChallengePath(v.cfg))
	})
}

// beginEnrollmentWebAuthn intentionally does not accept helper-provided key
// material. The authenticator creates the credential after receiving this
// opaque challenge; completion derives the key and credential from its data.
func (v *nativeOwnerVerifier) beginEnrollmentWebAuthn(now time.Time) (challenge nativeOwnerEnrollmentChallenge, signed []byte, err error) {
	identity, trusted := v.peerTrusted()
	if !trusted {
		return challenge, nil, errDesktopReviewProofUnavailable
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
		challenge = nativeOwnerEnrollmentChallenge{Nonce: hex.EncodeToString(random[:]), ExpiresAt: now.Add(nativeOwnerEnrollmentLifetime).Unix(), InstallationID: installationID, Host: runtime.GOOS, AppIdentity: identity}
		var bytesErr error
		signed, bytesErr = challenge.webAuthnBytes()
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

func (c nativeOwnerEnrollmentChallenge) webAuthnBytes() ([]byte, error) {
	if len(c.Nonce) != 64 || c.ExpiresAt <= 0 || c.InstallationID == "" || c.Host != runtime.GOOS || c.AppIdentity == "" {
		return nil, errors.New("invalid webauthn enrollment challenge")
	}
	var out bytes.Buffer
	out.WriteString("mora-review-windows-enrollment-v1\x00")
	for _, value := range []string{c.Nonce, c.InstallationID, c.Host, c.AppIdentity} {
		if len(value) > 1<<20 {
			return nil, errors.New("webauthn enrollment field is too large")
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

type nativeOwnerWebAuthnSessionVerifier struct {
	base  *nativeOwnerVerifier
	proof nativeOwnerWebAuthnProof
}

func (v nativeOwnerWebAuthnSessionVerifier) TrustedIdentity() (string, string, bool) {
	return v.base.TrustedIdentity()
}
func (v nativeOwnerWebAuthnSessionVerifier) Verify(installationID, keyID string, signed, _ []byte) bool {
	record, err := v.base.enrollment()
	if err != nil || record.InstallationID != installationID || record.KeyID != keyID {
		return false
	}
	pub, err := nativeOwnerP256(record.PublicKeyP256)
	if err != nil {
		return false
	}
	return nativeOwnerVerifyWebAuthn(pub, []byte(record.CredentialID), signed, v.proof)
}

// nativeOwnerValidateClientData binds the Go-issued opaque bytes to this exact
// relying party. It intentionally accepts no browser- or caller-selected RP.
func nativeOwnerValidateClientData(raw, challenge []byte, wantType string) error {
	var data struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &data) != nil || data.Type != wantType || data.Origin != nativeOwnerWindowsOrigin || data.CrossOrigin {
		return errors.New("invalid webauthn client data")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(data.Challenge)
	if err != nil || subtle.ConstantTimeCompare(decoded, challenge) != 1 {
		return errors.New("webauthn challenge mismatch")
	}
	return nil
}

func nativeOwnerValidateAuthenticatorData(data []byte, requireAttested bool) error {
	if len(data) < 37 || subtle.ConstantTimeCompare(data[:32], nativeOwnerWindowsRPHash()) != 1 {
		return errors.New("webauthn rp id mismatch")
	}
	flags := data[32]
	if flags&(nativeOwnerWebAuthnUP|nativeOwnerWebAuthnUV) != nativeOwnerWebAuthnUP|nativeOwnerWebAuthnUV {
		return errors.New("webauthn user verification missing")
	}
	if requireAttested && flags&nativeOwnerWebAuthnAT == 0 {
		return errors.New("webauthn attested credential missing")
	}
	return nil
}

func nativeOwnerWindowsRPHash() []byte {
	sum := sha256.Sum256([]byte(nativeOwnerWindowsRPID))
	return sum[:]
}

// nativeOwnerWebAuthnEnrollmentKey derives both credential and public key
// solely from attested authenticator data. A helper-supplied key field is never
// read by Go.
func nativeOwnerWebAuthnEnrollmentKey(data []byte) ([]byte, *ecdsa.PublicKey, error) {
	if err := nativeOwnerValidateAuthenticatorData(data, true); err != nil {
		return nil, nil, err
	}
	if len(data) < 55 {
		return nil, nil, errors.New("truncated attested credential")
	}
	n := int(binary.BigEndian.Uint16(data[53:55]))
	if n == 0 || n > 1024 || len(data) <= 55+n {
		return nil, nil, errors.New("invalid webauthn credential")
	}
	credential := append([]byte(nil), data[55:55+n]...)
	pub, consumed, err := nativeOwnerParseES256COSE(data[55+n:])
	if err != nil || consumed != len(data[55+n:]) {
		return nil, nil, errors.New("invalid webauthn cose key")
	}
	return credential, pub, nil
}

func nativeOwnerVerifyWebAuthn(public *ecdsa.PublicKey, credentialID, challenge []byte, proof nativeOwnerWebAuthnProof) bool {
	if public == nil || subtle.ConstantTimeCompare(credentialID, proof.CredentialID) != 1 ||
		nativeOwnerValidateClientData(proof.ClientDataJSON, challenge, "webauthn.get") != nil ||
		nativeOwnerValidateAuthenticatorData(proof.AuthenticatorData, false) != nil {
		return false
	}
	clientHash := sha256.Sum256(proof.ClientDataJSON)
	signed := append(append([]byte(nil), proof.AuthenticatorData...), clientHash[:]...)
	sum := sha256.Sum256(signed)
	return ecdsa.VerifyASN1(public, sum[:], proof.SignatureDER)
}

// Supports exactly the five-value, definite-length ES256 EC2 COSE map emitted
// by the fixed helper. Rejecting extensions avoids a permissive generic CBOR
// decoder at this trust boundary.
func nativeOwnerParseES256COSE(raw []byte) (*ecdsa.PublicKey, int, error) {
	if len(raw) < 1 || raw[0] != 0xa5 {
		return nil, 0, errors.New("expected cose map")
	}
	i := 1
	seen := map[int]bool{}
	var x, y []byte
	for range 5 {
		label, next, err := nativeOwnerCBORInt(raw, i)
		if err != nil || label == 0 || seen[label] {
			return nil, 0, errors.New("invalid cose label")
		}
		i = next
		seen[label] = true
		switch label {
		case 1:
			if !nativeOwnerCBORIsInt(raw, &i, 2) {
				return nil, 0, errors.New("invalid cose kty")
			}
		case 3:
			if !nativeOwnerCBORIsInt(raw, &i, -7) {
				return nil, 0, errors.New("invalid cose alg")
			}
		case -1:
			if !nativeOwnerCBORIsInt(raw, &i, 1) {
				return nil, 0, errors.New("invalid cose curve")
			}
		case -2, -3:
			v, next, err := nativeOwnerCBORBytes(raw, i)
			if err != nil || len(v) != 32 {
				return nil, 0, errors.New("invalid cose coordinate")
			}
			i = next
			if label == -2 {
				x = v
			} else {
				y = v
			}
		default:
			return nil, 0, errors.New("unsupported cose label")
		}
	}
	if len(x) != 32 || len(y) != 32 {
		return nil, 0, errors.New("incomplete cose key")
	}
	px, py := elliptic.P256().Params().Name, elliptic.P256()
	_ = px
	X, Y := elliptic.Unmarshal(py, append(append([]byte{4}, x...), y...))
	if X == nil || Y == nil {
		return nil, 0, errors.New("invalid p256 point")
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: X, Y: Y}, i, nil
}

func nativeOwnerCBORIsInt(raw []byte, at *int, want int) bool {
	v, n, err := nativeOwnerCBORInt(raw, *at)
	if err != nil || v != want {
		return false
	}
	*at = n
	return true
}
func nativeOwnerCBORInt(raw []byte, at int) (int, int, error) {
	if at >= len(raw) {
		return 0, at, errors.New("truncated cbor")
	}
	b := raw[at]
	major, add := b>>5, b&31
	if add >= 24 {
		return 0, at, errors.New("non-canonical cbor integer")
	}
	if major == 0 {
		return int(add), at + 1, nil
	}
	if major == 1 {
		return -1 - int(add), at + 1, nil
	}
	return 0, at, errors.New("expected cbor integer")
}
func nativeOwnerCBORBytes(raw []byte, at int) ([]byte, int, error) {
	if at+2 > len(raw) || raw[at] != 0x58 {
		return nil, at, errors.New("expected cbor bytes")
	}
	n := int(raw[at+1])
	if n == 0 || at+2+n > len(raw) {
		return nil, at, errors.New("truncated cbor bytes")
	}
	return append([]byte(nil), raw[at+2:at+2+n]...), at + 2 + n, nil
}

func nativeOwnerWebAuthnClientData(kind string, challenge []byte) []byte {
	return []byte(fmt.Sprintf(`{"type":%q,"challenge":%q,"origin":%q,"crossOrigin":false}`, kind, base64.RawURLEncoding.EncodeToString(challenge), nativeOwnerWindowsOrigin))
}

func nativeOwnerWebAuthnEnrollmentChallengeBytes(nonce string) []byte {
	return bytes.Join([][]byte{[]byte("mora-review-windows-enrollment-v1\x00"), []byte(nonce)}, nil)
}
