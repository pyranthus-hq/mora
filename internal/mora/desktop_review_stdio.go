package mora

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"time"
)

const desktopReviewFrameLimit = 1 << 20

type desktopReviewFrame struct {
	Schema             string              `json:"schema"`
	SchemaVersion      int                 `json:"schema_version"`
	Op                 string              `json:"op"`
	Kind               string              `json:"kind,omitempty"`
	ID                 string              `json:"id,omitempty"`
	Action             desktopReviewAction `json:"action,omitempty"`
	CanonicalRevision  string              `json:"canonical_revision,omitempty"`
	DisplayDigest      string              `json:"display_digest,omitempty"`
	Nonce              string              `json:"nonce,omitempty"`
	PublicKeyP256      string              `json:"public_key_p256_b64,omitempty"`
	CredentialID       string              `json:"credential_id,omitempty"`
	SignatureDER       string              `json:"signature_der_b64,omitempty"`
	ClientDataJSON     string              `json:"client_data_json_b64,omitempty"`
	AuthenticatorData  string              `json:"authenticator_data_b64,omitempty"`
	AssertionSignature string              `json:"assertion_signature_der_b64,omitempty"`
}

type desktopReviewFrameError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type desktopReviewFrameResponse struct {
	Schema            string                   `json:"schema"`
	SchemaVersion     int                      `json:"schema_version"`
	Notes             []reviewRow              `json:"notes,omitempty"`
	Preview           *desktopReviewPreview    `json:"preview,omitempty"`
	Enrolled          *bool                    `json:"enrolled,omitempty"`
	ReadOnly          *bool                    `json:"read_only,omitempty"`
	InstallationID    string                   `json:"installation_id,omitempty"`
	CredentialIDB64   string                   `json:"credential_id_b64,omitempty"`
	Nonce             string                   `json:"nonce,omitempty"`
	ChallengeBytesB64 string                   `json:"challenge_bytes_b64,omitempty"`
	KeyID             string                   `json:"key_id,omitempty"`
	Outcome           *desktopReviewOutcome    `json:"outcome,omitempty"`
	Error             *desktopReviewFrameError `json:"error,omitempty"`
}

func readDesktopReviewFrame(r io.Reader) (desktopReviewFrame, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return desktopReviewFrame{}, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > desktopReviewFrameLimit {
		return desktopReviewFrame{}, errors.New("desktop review frame exceeds limit")
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(r, raw); err != nil {
		return desktopReviewFrame{}, err
	}
	if !json.Valid(raw) {
		return desktopReviewFrame{}, errors.New("invalid desktop review frame")
	}
	var frame desktopReviewFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		return desktopReviewFrame{}, errors.New("invalid desktop review frame")
	}
	if frame.Schema != "mora.review.desktop" || frame.SchemaVersion != desktopReviewProtocolVersion || frame.Op == "" {
		return desktopReviewFrame{}, errors.New("unsupported desktop review frame")
	}
	return frame, nil
}

func writeDesktopReviewFrame(w io.Writer, response desktopReviewFrameResponse) error {
	response.Schema, response.SchemaVersion = "mora.review.desktop", desktopReviewProtocolVersion
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if len(raw) > desktopReviewFrameLimit {
		return errors.New("desktop review response exceeds limit")
	}
	frame := make([]byte, 4+len(raw))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(raw)))
	copy(frame[4:], raw)
	_, err = w.Write(frame)
	return err
}

func desktopReviewFrameFailure(code, message string) desktopReviewFrameResponse {
	return desktopReviewFrameResponse{Error: &desktopReviewFrameError{Code: code, Message: message}}
}

func serveDesktopReviewStdio(ctx context.Context, cfg Config, verifier desktopReviewVerifier, stdin io.Reader, stdout io.Writer) error {
	native, ok := verifier.(*nativeOwnerVerifier)
	if !ok {
		return errDesktopReviewProofUnavailable
	}
	if _, trusted := native.peerTrusted(); !trusted {
		return errDesktopReviewProofUnavailable
	}
	authority := newDesktopReviewAuthority(cfg, verifier)
	for {
		request, err := readDesktopReviewFrame(stdin)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		response := handleDesktopReviewFrame(ctx, cfg, authority, native, request)
		if err := writeDesktopReviewFrame(stdout, response); err != nil {
			return err
		}
	}
}

func handleDesktopReviewFrame(ctx context.Context, cfg Config, authority *desktopReviewAuthority, verifier *nativeOwnerVerifier, request desktopReviewFrame) desktopReviewFrameResponse {
	decode := func(value string) ([]byte, bool) {
		bytes, err := base64.StdEncoding.DecodeString(value)
		return bytes, err == nil && len(bytes) > 0
	}
	webProof := func(requireSignature bool) (nativeOwnerWebAuthnProof, bool) {
		credential, ok := decode(request.CredentialID)
		if !ok {
			return nativeOwnerWebAuthnProof{}, false
		}
		client, ok := decode(request.ClientDataJSON)
		if !ok {
			return nativeOwnerWebAuthnProof{}, false
		}
		auth, ok := decode(request.AuthenticatorData)
		if !ok {
			return nativeOwnerWebAuthnProof{}, false
		}
		signature, ok := decode(request.AssertionSignature)
		if requireSignature && !ok {
			return nativeOwnerWebAuthnProof{}, false
		}
		return nativeOwnerWebAuthnProof{CredentialID: credential, ClientDataJSON: client, AuthenticatorData: auth, SignatureDER: signature}, true
	}
	switch request.Op {
	case "list":
		notes, err := listReviewNotes(cfg, io.Discard)
		if err != nil {
			return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "review queue is unavailable")
		}
		rows := make([]reviewRow, 0, len(notes))
		for _, note := range notes {
			rows = append(rows, reviewRow{ID: note.ID, Kind: note.Kind, Scope: note.Scope, Type: note.Type, Title: note.Title})
		}
		return desktopReviewFrameResponse{Notes: rows}
	case "show":
		preview, err := loadDesktopReviewPreview(cfg, request.Kind, request.ID)
		if err != nil {
			return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "review item is unavailable")
		}
		return desktopReviewFrameResponse{Preview: &preview}
	case "status":
		_, _, enrolled := verifier.TrustedIdentity()
		readOnly := !enrolled
		installationID, err := nativeOwnerInstallationID(cfg)
		if err != nil {
			return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "desktop status is unavailable")
		}
		response := desktopReviewFrameResponse{Enrolled: &enrolled, ReadOnly: &readOnly, InstallationID: installationID}
		if enrolled && runtime.GOOS == "windows" {
			record, err := verifier.enrollment()
			if err != nil || record.CredentialID == "" {
				return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "desktop status is unavailable")
			}
			response.CredentialIDB64 = record.CredentialID
		}
		return response
	case "enroll.begin":
		var challenge nativeOwnerEnrollmentChallenge
		var signed []byte
		var err error
		if runtime.GOOS == "windows" && request.PublicKeyP256 == "" && request.CredentialID == "" {
			challenge, signed, err = verifier.beginEnrollmentWebAuthn(time.Now())
		} else {
			public, ok := decode(request.PublicKeyP256)
			if !ok {
				return desktopReviewFrameFailure("ERR_INVALID_REQUEST", "invalid enrollment request")
			}
			challenge, signed, err = verifier.beginEnrollment(public, request.CredentialID, time.Now())
		}
		if err != nil {
			return desktopReviewFrameFailure("ERR_OWNER_VERIFICATION_UNAVAILABLE", "desktop owner verification is unavailable")
		}
		return desktopReviewFrameResponse{ChallengeBytesB64: base64.StdEncoding.EncodeToString(signed), KeyID: challenge.KeyID, Nonce: challenge.Nonce}
	case "enroll.complete":
		if runtime.GOOS == "windows" {
			proof, ok := webProof(false)
			if !ok {
				return desktopReviewFrameFailure("ERR_INVALID_REQUEST", "invalid enrollment proof")
			}
			if err := verifier.completeEnrollmentWebAuthn(request.Nonce, proof, time.Now()); err != nil {
				return desktopReviewFrameFailure("ERR_OWNER_VERIFICATION_UNAVAILABLE", "desktop enrollment was not accepted")
			}
			record, err := verifier.enrollment()
			if err != nil || record.CredentialID == "" {
				return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "desktop enrollment is unavailable")
			}
			enrolled, readOnly := true, false
			return desktopReviewFrameResponse{Enrolled: &enrolled, ReadOnly: &readOnly, CredentialIDB64: record.CredentialID}
		}
		proof, ok := decode(request.SignatureDER)
		if !ok {
			return desktopReviewFrameFailure("ERR_INVALID_REQUEST", "invalid enrollment proof")
		}
		if err := verifier.completeEnrollment(request.Nonce, proof, time.Now()); err != nil {
			return desktopReviewFrameFailure("ERR_OWNER_VERIFICATION_UNAVAILABLE", "desktop enrollment was not accepted")
		}
		enrolled, readOnly := true, false
		return desktopReviewFrameResponse{Enrolled: &enrolled, ReadOnly: &readOnly}
	case "decision.begin":
		challenge, err := authority.begin(request.Action, request.Kind, request.ID, request.CanonicalRevision, request.DisplayDigest)
		if err != nil {
			return desktopReviewFrameFailure(desktopReviewErrorCode(err), "desktop decision is unavailable")
		}
		signed, err := challenge.bytes()
		if err != nil {
			return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "desktop decision is unavailable")
		}
		return desktopReviewFrameResponse{ChallengeBytesB64: base64.StdEncoding.EncodeToString(signed), KeyID: challenge.KeyID, Nonce: challenge.Nonce, Outcome: nil}
	case "decision.complete":
		if runtime.GOOS == "windows" {
			proof, ok := webProof(true)
			if !ok {
				return desktopReviewFrameFailure("ERR_INVALID_REQUEST", "invalid decision proof")
			}
			out, err := newDesktopReviewAuthority(cfg, nativeOwnerWebAuthnSessionVerifier{base: verifier, proof: proof}).complete(ctx, request.Nonce, nil, false)
			if err != nil {
				return desktopReviewFrameFailure(desktopReviewErrorCode(err), "desktop decision was not accepted")
			}
			return desktopReviewFrameResponse{Outcome: &out}
		}
		proof, ok := decode(request.SignatureDER)
		if !ok {
			return desktopReviewFrameFailure("ERR_INVALID_REQUEST", "invalid decision proof")
		}
		out, err := authority.complete(ctx, request.Nonce, proof, false)
		if err != nil {
			return desktopReviewFrameFailure(desktopReviewErrorCode(err), "desktop decision was not accepted")
		}
		return desktopReviewFrameResponse{Outcome: &out}
	case "enroll.cancel":
		if strings.TrimSpace(request.Nonce) == "" || verifier.cancelEnrollment(request.Nonce) != nil {
			return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "desktop enrollment cancellation is unavailable")
		}
		readOnly := true
		return desktopReviewFrameResponse{ReadOnly: &readOnly}
	case "cancel":
		if strings.TrimSpace(request.Nonce) == "" {
			return desktopReviewFrameFailure("ERR_INVALID_REQUEST", "invalid cancellation")
		}
		out, err := authority.complete(ctx, request.Nonce, nil, true)
		if err != nil {
			return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "desktop cancellation is unavailable")
		}
		return desktopReviewFrameResponse{Outcome: &out}
	default:
		return desktopReviewFrameFailure("ERR_INVALID_REQUEST", "unsupported desktop operation")
	}
}

func desktopReviewErrorCode(err error) string {
	if errors.Is(err, errDesktopReviewProofUnavailable) {
		return "ERR_OWNER_VERIFICATION_UNAVAILABLE"
	}
	return "ERR_REVIEW_UNAVAILABLE"
}
