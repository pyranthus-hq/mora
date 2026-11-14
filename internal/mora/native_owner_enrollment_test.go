package mora

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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

func TestDesktopReviewStdioEnrollsThenCancelsDecision(t *testing.T) {
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
