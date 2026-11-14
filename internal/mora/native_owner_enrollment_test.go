package mora

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type nativeOwnerTestPeer struct {
	value   string
	trusted bool
}

func (p nativeOwnerTestPeer) identity() (string, bool) { return p.value, p.trusted }

func nativeOwnerTestKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key, elliptic.Marshal(elliptic.P256(), key.X, key.Y)
}

func nativeOwnerTestProof(t *testing.T, key *ecdsa.PrivateKey, bytes []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(bytes)
	proof, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func TestNativeOwnerEnrollmentBindsTrustedIdentityAndKey(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	peer := nativeOwnerTestPeer{value: "test-app", trusted: true}
	verifier := newNativeOwnerVerifierForPeer(cfg, &peer)
	key, public := nativeOwnerTestKey(t)
	now := time.Unix(1_700_000_000, 0)
	challenge, signed, err := verifier.beginEnrollment(public, "credential", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.completeEnrollment(challenge.Nonce, nativeOwnerTestProof(t, key, signed), now); err != nil {
		t.Fatal(err)
	}
	if installation, keyID, ok := verifier.TrustedIdentity(); !ok || installation != challenge.InstallationID || keyID != challenge.KeyID {
		t.Fatalf("trusted identity = %q %q %v", installation, keyID, ok)
	}
	if !verifier.Verify(challenge.InstallationID, challenge.KeyID, []byte("decision"), nativeOwnerTestProof(t, key, []byte("decision"))) {
		t.Fatal("expected decision proof to verify")
	}
	if verifier.Verify(challenge.InstallationID, challenge.KeyID, []byte("changed"), nativeOwnerTestProof(t, key, []byte("decision"))) {
		t.Fatal("accepted changed decision bytes")
	}
	peer.value = "other-app"
	if _, _, ok := verifier.TrustedIdentity(); ok {
		t.Fatal("accepted changed app identity")
	}
}

func TestNativeOwnerEnrollmentRefusesExpiredAndImportedProof(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	verifier := newNativeOwnerVerifierForPeer(cfg, nativeOwnerTestPeer{value: "test-app", trusted: true})
	key, public := nativeOwnerTestKey(t)
	challenge, signed, err := verifier.beginEnrollment(public, "", time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.completeEnrollment(challenge.Nonce, nativeOwnerTestProof(t, key, signed), time.Unix(1_700_000_091, 0)); err == nil {
		t.Fatal("accepted expired enrollment")
	}
	if _, _, ok := verifier.TrustedIdentity(); ok {
		t.Fatal("enrolled expired proof")
	}
}

func TestNativeOwnerEnrollmentPreservesCorruptRecord(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	if err := os.MkdirAll(nativeOwnerDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	path := nativeOwnerEnrollmentPath(cfg)
	broken := []byte("not enrollment json")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	verifier := newNativeOwnerVerifierForPeer(cfg, nativeOwnerTestPeer{value: "test-app", trusted: true})
	_, public := nativeOwnerTestKey(t)
	if _, _, err := verifier.beginEnrollment(public, "", time.Now()); err == nil {
		t.Fatal("accepted replacement of corrupt enrollment")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, broken) {
		t.Fatalf("corrupt enrollment changed: %q, %v", got, err)
	}
}

func TestNativeOwnerWebAuthnValidationRejectsChangedTrustFields(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	challenge := []byte("opaque-go-challenge")
	credential := []byte("credential")
	auth := make([]byte, 37)
	copy(auth, nativeOwnerWindowsRPHash())
	auth[32] = nativeOwnerWebAuthnUP | nativeOwnerWebAuthnUV
	client := nativeOwnerWebAuthnClientData("webauthn.get", challenge)
	clientHash := sha256.Sum256(client)
	signed := append(append([]byte(nil), auth...), clientHash[:]...)
	sum := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	proof := nativeOwnerWebAuthnProof{CredentialID: credential, ClientDataJSON: client, AuthenticatorData: auth, SignatureDER: signature}
	if !nativeOwnerVerifyWebAuthn(&key.PublicKey, credential, challenge, proof) {
		t.Fatal("valid assertion rejected")
	}
	for name, mutate := range map[string]func(*nativeOwnerWebAuthnProof){
		"credential": func(p *nativeOwnerWebAuthnProof) { p.CredentialID = []byte("other") },
		"origin": func(p *nativeOwnerWebAuthnProof) {
			p.ClientDataJSON = []byte(`{"type":"webauthn.get","challenge":"b3BhcXVlLWdvLWNoYWxsZW5nZQ","origin":"https://other","crossOrigin":false}`)
		},
		"up":        func(p *nativeOwnerWebAuthnProof) { p.AuthenticatorData[32] = nativeOwnerWebAuthnUV },
		"uv":        func(p *nativeOwnerWebAuthnProof) { p.AuthenticatorData[32] = nativeOwnerWebAuthnUP },
		"rp":        func(p *nativeOwnerWebAuthnProof) { p.AuthenticatorData[0] ^= 1 },
		"signature": func(p *nativeOwnerWebAuthnProof) { p.SignatureDER[len(p.SignatureDER)-1] ^= 1 },
	} {
		t.Run(name, func(t *testing.T) {
			altered := nativeOwnerWebAuthnProof{CredentialID: append([]byte(nil), proof.CredentialID...), ClientDataJSON: append([]byte(nil), proof.ClientDataJSON...), AuthenticatorData: append([]byte(nil), proof.AuthenticatorData...), SignatureDER: append([]byte(nil), proof.SignatureDER...)}
			mutate(&altered)
			if nativeOwnerVerifyWebAuthn(&key.PublicKey, credential, challenge, altered) {
				t.Fatal("accepted altered webauthn assertion")
			}
		})
	}
}

func TestNativeOwnerWebAuthnEnrollmentDerivesAttestedCredential(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	verifier := newNativeOwnerVerifierForPeer(cfg, nativeOwnerTestPeer{value: "test-app", trusted: true})
	challenge, signed, err := verifier.beginEnrollmentWebAuthn(time.Now())
	if err != nil || challenge.Nonce == "" || len(signed) == 0 {
		t.Fatalf("begin webauthn enrollment = %#v, %v", challenge, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credential := []byte("credential-id")
	auth := make([]byte, 37+16+2+len(credential))
	copy(auth, nativeOwnerWindowsRPHash())
	auth[32] = nativeOwnerWebAuthnUP | nativeOwnerWebAuthnUV | nativeOwnerWebAuthnAT
	binary.BigEndian.PutUint16(auth[53:55], uint16(len(credential)))
	copy(auth[55:], credential)
	cose := []byte{0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21, 0x58, 0x20}
	cose = append(cose, key.X.FillBytes(make([]byte, 32))...)
	cose = append(cose, 0x22, 0x58, 0x20)
	cose = append(cose, key.Y.FillBytes(make([]byte, 32))...)
	auth = append(auth, cose...)
	proof := nativeOwnerWebAuthnProof{CredentialID: credential, ClientDataJSON: nativeOwnerWebAuthnClientData("webauthn.create", signed), AuthenticatorData: auth}
	if err := verifier.completeEnrollmentWebAuthn(challenge.Nonce, proof, time.Now()); err != nil {
		t.Fatalf("complete webauthn enrollment = %v", err)
	}
	record, err := verifier.enrollment()
	if err != nil || record.CredentialID != base64.StdEncoding.EncodeToString(credential) || len(record.PublicKeyP256) != 65 {
		t.Fatalf("enrollment record = %#v, %v", record, err)
	}
}

func TestNativeOwnerWebAuthnEnrollmentDecisionRoundTrip(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "webauthn decision")
	peer := nativeOwnerTestPeer{value: "test-app", trusted: true}
	verifier := newNativeOwnerVerifierForPeer(cfg, &peer)
	key, credential := nativeOwnerWebAuthnTestEnrollment(t, verifier)
	preview, err := loadDesktopReviewPreview(cfg, reviewKindSaved, id)
	if err != nil {
		t.Fatal(err)
	}
	authority := newDesktopReviewAuthority(cfg, verifier)
	decision, err := authority.begin(desktopReviewKeep, reviewKindSaved, id, preview.CanonicalRevision, preview.DisplayDigest)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := decision.bytes()
	if err != nil {
		t.Fatal(err)
	}
	proof := nativeOwnerWebAuthnTestAssertion(t, key, credential, signed)
	session := nativeOwnerWebAuthnSessionVerifier{base: verifier, proof: proof}
	if out, err := newDesktopReviewAuthority(cfg, session).complete(testCtx(t), decision.Nonce, nil, false); err != nil || out.Status != "applied" {
		t.Fatalf("decision completion = %#v, %v", out, err)
	}
	peer.value = "other-app"
	if session.Verify(decision.InstallationID, decision.KeyID, signed, nil) {
		t.Fatal("accepted assertion from changed peer")
	}
	peer.value = "test-app"
	changed := proof
	changed.CredentialID = []byte("other-credential")
	if (nativeOwnerWebAuthnSessionVerifier{base: verifier, proof: changed}).Verify(decision.InstallationID, decision.KeyID, signed, nil) {
		t.Fatal("accepted replayed assertion with another credential")
	}
	record, err := verifier.enrollment()
	if err != nil {
		t.Fatal(err)
	}
	record.Host = "other"
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativeOwnerEnrollmentPath(cfg), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if session.Verify(decision.InstallationID, decision.KeyID, signed, nil) {
		t.Fatal("accepted assertion from changed host")
	}
}

func nativeOwnerWebAuthnTestEnrollment(t *testing.T, verifier *nativeOwnerVerifier) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	challenge, signed, err := verifier.beginEnrollmentWebAuthn(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credential := []byte("credential-id")
	auth := nativeOwnerWebAuthnTestAuthenticatorData(key, credential, true)
	proof := nativeOwnerWebAuthnProof{CredentialID: credential, ClientDataJSON: nativeOwnerWebAuthnClientData("webauthn.create", signed), AuthenticatorData: auth}
	if err := verifier.completeEnrollmentWebAuthn(challenge.Nonce, proof, time.Now()); err != nil {
		t.Fatal(err)
	}
	return key, credential
}

func nativeOwnerWebAuthnTestAssertion(t *testing.T, key *ecdsa.PrivateKey, credential, challenge []byte) nativeOwnerWebAuthnProof {
	t.Helper()
	client := nativeOwnerWebAuthnClientData("webauthn.get", challenge)
	auth := nativeOwnerWebAuthnTestAuthenticatorData(key, credential, false)
	clientHash := sha256.Sum256(client)
	signed := append(append([]byte(nil), auth...), clientHash[:]...)
	sum := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return nativeOwnerWebAuthnProof{CredentialID: credential, ClientDataJSON: client, AuthenticatorData: auth, SignatureDER: signature}
}

func nativeOwnerWebAuthnTestAuthenticatorData(key *ecdsa.PrivateKey, credential []byte, attested bool) []byte {
	auth := make([]byte, 37)
	copy(auth, nativeOwnerWindowsRPHash())
	auth[32] = nativeOwnerWebAuthnUP | nativeOwnerWebAuthnUV
	if !attested {
		return auth
	}
	auth[32] |= nativeOwnerWebAuthnAT
	auth = append(auth, make([]byte, 16)...)
	var width [2]byte
	binary.BigEndian.PutUint16(width[:], uint16(len(credential)))
	auth = append(auth, width[:]...)
	auth = append(auth, credential...)
	cose := []byte{0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21, 0x58, 0x20}
	cose = append(cose, key.X.FillBytes(make([]byte, 32))...)
	cose = append(cose, 0x22, 0x58, 0x20)
	cose = append(cose, key.Y.FillBytes(make([]byte, 32))...)
	return append(auth, cose...)
}

func TestNativeOwnerWindowsIdentityUpgradeRejectsUntrustedHashes(t *testing.T) {
	good := "sha256:" + strings.Repeat("a", 64)
	if !nativeOwnerCanUpgradeWindowsIdentity("windows", good, nativeOwnerWindowsAppIdentity) {
		t.Fatal("expected legacy pinned identity to require fresh enrollment")
	}
	for _, tc := range []struct{ host, previous, next string }{
		{"windows", "sha256:short", nativeOwnerWindowsAppIdentity},
		{"windows", "sha256:" + strings.Repeat("z", 64), nativeOwnerWindowsAppIdentity},
		{"darwin", good, nativeOwnerWindowsAppIdentity},
		{"windows", good, "sha256:" + strings.Repeat("b", 64)},
	} {
		if nativeOwnerCanUpgradeWindowsIdentity(tc.host, tc.previous, tc.next) {
			t.Fatalf("accepted untrusted upgrade %#v", tc)
		}
	}
}

func TestNativeOwnerHelperDigestRejectsEmptyWrongAndRebuiltBytes(t *testing.T) {
	good := sha256.Sum256([]byte("review helper"))
	for _, tc := range []struct {
		name, body, digest string
		want               bool
	}{
		{"empty", "review helper", "", false}, {"wrong", "review helper", strings.Repeat("0", 64), false},
		{"rebuilt", "review helper changed", hex.EncodeToString(good[:]), false}, {"exact", "review helper", hex.EncodeToString(good[:]), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := nativeOwnerHelperDigestMatches(strings.NewReader(tc.body), tc.digest)
			if (err == nil) != tc.want {
				t.Fatalf("digest result = %v", err)
			}
		})
	}
}

func TestNativeOwnerInstallationIDPreservesInvalidFile(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	if err := os.MkdirAll(nativeOwnerDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	path := nativeOwnerInstallationPath(cfg)
	broken := []byte("not-an-installation-id")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeOwnerInstallationID(cfg); err == nil {
		t.Fatal("accepted replacement of invalid installation ID")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, broken) {
		t.Fatalf("invalid installation ID changed: %q, %v", got, err)
	}
}

func TestNativeOwnerInstallationIDConcurrentFirstUseIsStable(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	const callers = 8
	ids := make(chan string, callers)
	errs := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			id, err := nativeOwnerInstallationID(cfg)
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	group.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("concurrent installation IDs differ: %q and %q", first, id)
		}
	}
	if first == "" {
		t.Fatal("no installation ID returned")
	}
}

func TestDesktopReviewFramesAreBoundedAndRejectTruncation(t *testing.T) {
	frame := desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "status"}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var request bytes.Buffer
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(raw)))
	request.Write(n[:])
	request.Write(raw)
	got, err := readDesktopReviewFrame(&request)
	if err != nil || got.Op != "status" {
		t.Fatalf("frame = %#v, %v", got, err)
	}
	if _, err := readDesktopReviewFrame(bytes.NewReader([]byte{0, 16, 0, 1})); err == nil {
		t.Fatal("accepted oversized frame")
	}
	if _, err := readDesktopReviewFrame(bytes.NewReader([]byte{0, 0, 0, 4, '{'})); err == nil || err == io.EOF {
		t.Fatalf("truncated frame error = %v", err)
	}
}

func TestDesktopReviewFrameRejectsChangedEnrollmentFields(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	peer := nativeOwnerTestPeer{value: "test-app", trusted: true}
	verifier := newNativeOwnerVerifierForPeer(cfg, &peer)
	key, public := nativeOwnerTestKey(t)
	request := desktopReviewFrame{Op: "enroll.begin", PublicKeyP256: base64.StdEncoding.EncodeToString(public)}
	response := handleDesktopReviewFrame(testCtx(t), cfg, newDesktopReviewAuthority(cfg, verifier), verifier, request)
	if response.Error != nil || response.ChallengeBytesB64 == "" {
		t.Fatalf("begin = %#v", response)
	}
	challenge, err := base64.StdEncoding.DecodeString(response.ChallengeBytesB64)
	if err != nil {
		t.Fatal(err)
	}
	proof := nativeOwnerTestProof(t, key, append(challenge, 0))
	response = handleDesktopReviewFrame(testCtx(t), cfg, newDesktopReviewAuthority(cfg, verifier), verifier, desktopReviewFrame{Op: "enroll.complete", SignatureDER: base64.StdEncoding.EncodeToString(proof)})
	if response.Error == nil {
		t.Fatal("accepted field-mutated enrollment proof")
	}
}

func TestDesktopReviewStdioEnrollmentAndDecision(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "stdio cancel")
	peer := nativeOwnerTestPeer{value: "test-app", trusted: true}
	verifier := newNativeOwnerVerifierForPeer(cfg, &peer)
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	errCh := make(chan error, 1)
	go func() { errCh <- serveDesktopReviewStdio(testCtx(t), cfg, verifier, inputReader, outputWriter) }()
	writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "status"})
	status := readStdioResponse(t, outputReader)
	if status.Enrolled == nil || *status.Enrolled || status.InstallationID == "" {
		t.Fatalf("unenrolled status = %#v", status)
	}
	if runtime.GOOS == "windows" {
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "enroll.begin"})
		derBegin := readStdioResponse(t, outputReader)
		if derBegin.Nonce == "" {
			t.Fatalf("DER denial enrollment begin = %#v", derBegin)
		}
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "enroll.complete", Nonce: derBegin.Nonce, SignatureDER: base64.StdEncoding.EncodeToString([]byte("mac-der-must-not-work"))})
		if denied := readStdioResponse(t, outputReader); denied.Error == nil {
			t.Fatal("accepted generic DER enrollment on Windows")
		}
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "status"})
		if afterDER := readStdioResponse(t, outputReader); afterDER.Enrolled == nil || *afterDER.Enrolled {
			t.Fatalf("DER denial changed enrollment = %#v", afterDER)
		}
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "enroll.begin"})
		begin := readStdioResponse(t, outputReader)
		challenge, err := base64.StdEncoding.DecodeString(begin.ChallengeBytesB64)
		if err != nil || begin.Nonce == "" {
			t.Fatalf("webauthn enrollment begin = %#v, %v", begin, err)
		}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		credential := []byte("stdio-windows-credential")
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "enroll.complete", Nonce: begin.Nonce, CredentialID: base64.StdEncoding.EncodeToString(credential), ClientDataJSON: base64.StdEncoding.EncodeToString(nativeOwnerWebAuthnClientData("webauthn.create", challenge)), AuthenticatorData: base64.StdEncoding.EncodeToString(nativeOwnerWebAuthnTestAuthenticatorData(key, credential, true))})
		if complete := readStdioResponse(t, outputReader); complete.Enrolled == nil || !*complete.Enrolled || complete.CredentialIDB64 != base64.StdEncoding.EncodeToString(credential) {
			t.Fatalf("webauthn enrollment complete = %#v", complete)
		}
		preview, err := loadDesktopReviewPreview(cfg, reviewKindSaved, id)
		if err != nil {
			t.Fatal(err)
		}
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "decision.begin", Action: desktopReviewDiscard, Kind: reviewKindSaved, ID: id, CanonicalRevision: preview.CanonicalRevision, DisplayDigest: preview.DisplayDigest})
		decision := readStdioResponse(t, outputReader)
		decisionBytes, err := base64.StdEncoding.DecodeString(decision.ChallengeBytesB64)
		if err != nil || decision.Nonce == "" {
			t.Fatalf("webauthn decision begin = %#v, %v", decision, err)
		}
		assertion := nativeOwnerWebAuthnTestAssertion(t, key, credential, decisionBytes)
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "decision.complete", Nonce: decision.Nonce, CredentialID: base64.StdEncoding.EncodeToString(assertion.CredentialID), ClientDataJSON: base64.StdEncoding.EncodeToString(assertion.ClientDataJSON), AuthenticatorData: base64.StdEncoding.EncodeToString(assertion.AuthenticatorData), AssertionSignature: base64.StdEncoding.EncodeToString(assertion.SignatureDER)})
		if applied := readStdioResponse(t, outputReader); applied.Outcome == nil || applied.Outcome.Status != "applied" {
			t.Fatalf("webauthn decision complete = %#v", applied)
		}
	} else {
		key, public := nativeOwnerTestKey(t)
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "enroll.begin", PublicKeyP256: base64.StdEncoding.EncodeToString(public)})
		begin := readStdioResponse(t, outputReader)
		challenge, err := base64.StdEncoding.DecodeString(begin.ChallengeBytesB64)
		if err != nil || begin.Nonce == "" {
			t.Fatalf("enrollment begin = %#v, %v", begin, err)
		}
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "enroll.complete", Nonce: begin.Nonce, SignatureDER: base64.StdEncoding.EncodeToString(nativeOwnerTestProof(t, key, challenge))})
		if complete := readStdioResponse(t, outputReader); complete.Enrolled == nil || !*complete.Enrolled {
			t.Fatalf("enrollment complete = %#v", complete)
		}
		preview, err := loadDesktopReviewPreview(cfg, reviewKindSaved, id)
		if err != nil {
			t.Fatal(err)
		}
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "decision.begin", Action: desktopReviewDiscard, Kind: reviewKindSaved, ID: id, CanonicalRevision: preview.CanonicalRevision, DisplayDigest: preview.DisplayDigest})
		decision := readStdioResponse(t, outputReader)
		if decision.Nonce == "" || decision.ChallengeBytesB64 == "" {
			t.Fatalf("decision begin = %#v", decision)
		}
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "cancel", Nonce: decision.Nonce})
		if cancelled := readStdioResponse(t, outputReader); cancelled.Outcome == nil || cancelled.Outcome.Status != "cancelled" {
			t.Fatalf("cancel = %#v", cancelled)
		}
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		binding := projectBinding{Dir: root, Scope: "project:stdio-rule"}
		if err := saveProjectBindings(cfg, []projectBinding{binding}); err != nil {
			t.Fatal(err)
		}
		rule := projectRuleTestMemory(t, cfg, binding, "mem_stdio_rule", "Activate this framed native rule.")
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "project_rule.list", Client: "codex", ProjectDir: root})
		if listed := readStdioResponse(t, outputReader); listed.Error != nil || len(listed.Projects) != 1 || len(listed.Rules) != 1 {
			t.Fatalf("project rule list = %#v", listed)
		}
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "project_rule.show", Client: "codex", ProjectDir: root, ID: rule.RuleID})
		shown := readStdioResponse(t, outputReader)
		if shown.Error != nil || shown.Preview == nil || shown.Preview.Text != rule.Text || shown.Preview.CanonicalRevision != rule.RevisionSHA256 || len(shown.Preview.Details) != 4 {
			t.Fatalf("project rule show = %#v", shown)
		}
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "decision.begin", Action: desktopReviewActivate, Kind: desktopReviewProjectRule, ProjectDir: root, Client: "codex", ID: rule.RuleID, CanonicalRevision: shown.Preview.CanonicalRevision, DisplayDigest: shown.Preview.DisplayDigest})
		ruleDecision := readStdioResponse(t, outputReader)
		ruleBytes, err := base64.StdEncoding.DecodeString(ruleDecision.ChallengeBytesB64)
		if err != nil || ruleDecision.Nonce == "" {
			t.Fatalf("project rule begin = %#v, %v", ruleDecision, err)
		}
		writeStdioRequest(t, inputWriter, desktopReviewFrame{Schema: "mora.review.desktop", SchemaVersion: 1, Op: "decision.complete", Nonce: ruleDecision.Nonce, SignatureDER: base64.StdEncoding.EncodeToString(nativeOwnerTestProof(t, key, ruleBytes))})
		if applied := readStdioResponse(t, outputReader); applied.Outcome == nil || applied.Outcome.Status != "applied" {
			t.Fatalf("project rule complete = %#v", applied)
		}
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestDesktopReviewStdioRejectsUntrustedPeerBeforeStateWrite(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	verifier := newNativeOwnerVerifierForPeer(cfg, nativeOwnerTestPeer{value: "fake", trusted: false})
	err := serveDesktopReviewStdio(testCtx(t), cfg, verifier, bytes.NewReader(nil), io.Discard)
	if !errors.Is(err, errDesktopReviewProofUnavailable) {
		t.Fatalf("serve error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "desktop-review")); !os.IsNotExist(err) {
		t.Fatalf("untrusted peer wrote state: %v", err)
	}
}

func writeStdioRequest(t *testing.T, writer *io.PipeWriter, frame desktopReviewFrame) {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
	if _, err := writer.Write(append(header[:], raw...)); err != nil {
		t.Fatal(err)
	}
}
func readStdioResponse(t *testing.T, reader *io.PipeReader) desktopReviewFrameResponse {
	t.Helper()
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := io.ReadFull(reader, raw); err != nil {
		t.Fatal(err)
	}
	var response desktopReviewFrameResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	return response
}
