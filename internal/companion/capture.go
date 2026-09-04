package companion

// capture.go is governed capture: the ONE route on this listener that writes
// (graph node N21).
//
// Every other companion route is a projection over a vault the phone may not
// change. This one hands the kernel something to remember, and the whole file is
// about making that safe to do from a device on a hostile network.
//
// # The gate is the vault's write policy, and the phone never sees the lever
//
// The policy is read from the vault's own configuration on every request. It is
// not a field on the capture, not a header, not a query parameter, and there is
// no way for a device to express a preference about it. What the policy decides
// is N02's published table, and Receipt.Validate enforces it on the way out:
//
//	readonly -> rejected, reason policy    nothing is staged, nothing is written
//	propose  -> accepted                   staged for local approval, NOT in the vault
//	open     -> applied                    in the vault, and only after the write landed
//
// A policy that cannot be READ is readonly. That is the fail-closed direction and
// it is not a detail: a malformed config used to surface as a 503 and leave the
// key claimed, so an unreadable vault turned every capture into a pending record
// and the store filled up with claims nothing could settle.
//
// # One write path, and one place a memory can land
//
// The kernel side of Writer goes through the SAME governed write the CLI's
// `mora write` and the MCP write_memory tool use. This listener opens no second
// door into the vault: it decides who may knock and what the request must look
// like, and the kernel decides what happens to the vault.
//
// # Retries are the normal case, and the id is what makes them safe
//
// The vault id a capture publishes under is DERIVED — see captureMemoryID —
// before the key is reserved, and the kernel is asked to create exactly that id.
// The create-exclusive publish then settles every race the reservation cannot:
// two attempts, a crash between the write and the settle, a swept reservation,
// a duplicate delivered by a proxy. Whoever gets the link wins; everybody else
// is told the memory already exists and settles `applied` without writing.
//
// # What a receipt never carries
//
// Not the text, not a snippet of it, not a title derived from it. A receipt is
// identifiers, a state, a policy, a fingerprint and two timestamps.
// TestCaptureReceiptNeverEchoesThePayload drives the real handler and fails on
// any word from the capture appearing in the response.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// WriteOutcome is what the kernel's governed write path DID with a capture.
//
// The kernel returns the outcome rather than a raw success, because the outcome
// is the part this package cannot infer: whether the vault publication completed
// is a fact about the vault, and a listener that guessed at it would be the thing
// that lets a phone say "Saved" for a write that never landed.
type WriteOutcome struct {
	// Policy is the write policy the kernel read for THIS request.
	Policy WritePolicy
	// State is the terminal receipt state: applied, accepted or rejected.
	State ReceiptState
	// Reason is set if and only if State is rejected.
	Reason RejectReason
	// MemoryID is set if and only if State is applied. It is the identifier the
	// phone will see again on an evidence row for the same memory.
	MemoryID string
	// IntegrityDetail is set only when the kernel refused because the VAULT is
	// in a state it cannot explain — a memory at a pinned id that is not this
	// capture. It never reaches the device: it is the one thing this listener
	// logs, and it carries identifiers the kernel derived and nothing the user
	// wrote. See handleCapture.
	IntegrityDetail string
}

// Writer is the kernel's governed-write seam, the mirror of Reader.
//
// No method takes a policy: the kernel reads the policy itself, per request,
// from the vault it owns. A seam that accepted a policy would be a seam a caller
// could lie to.
type Writer interface {
	// Policy reports the vault's current write policy. It is used only to stamp
	// a receipt for a capture that is refused before the kernel is asked to
	// write anything, so that even those receipts name the policy in force. An
	// error means the policy could not be READ, and the caller fails closed.
	Policy(ctx context.Context) (WritePolicy, error)
	// PublishedForKey reports the capture identity this device's key has already
	// published, if any.
	//
	// It is asked before a FRESH reservation, and it answers what the reservation
	// store cannot: a capture killed after its publication leaves a pending row
	// the sweep collects, and after that the key looks unused. Without it, a
	// re-stamped retry of such a capture is a new identity, a new derived id, and
	// a second memory.
	PublishedForKey(ctx context.Context, deviceID, key string) (identity string, found bool, err error)
	// Published reports whether the pinned id is already in the vault, and if so
	// the outcome that describes it. It is asked only when a crashed reservation
	// is reclaimed, so the retry can finish somebody else's work rather than
	// repeat it.
	//
	// It takes the whole CaptureIdentity because "is it published?" is not a
	// question about a path. A file at the pinned id that is NOT this capture is
	// a vault-integrity failure, and the kernel is the only side that can tell
	// the difference — so it returns the outcome, including the rejection.
	Published(ctx context.Context, c Capture, id CaptureIdentity) (WriteOutcome, bool, error)
	// Publish runs the capture through the kernel's existing governed write
	// path, pinned to id.MemoryID, and reports what happened. The publication is
	// durable — the file and its directory are synced — before it returns.
	Publish(ctx context.Context, c Capture, id CaptureIdentity) (WriteOutcome, error)
}

// ---------------------------------------------------------------------------
// Capture identity
// ---------------------------------------------------------------------------

// captureIdentity is the digest that answers "is this the same capture?".
//
// It covers every field that changes WHAT GETS WRITTEN, and nothing else. The
// wire `payload_fingerprint` cannot do this job: N02 defines it as SHA-256 over
// the text alone, so the same key with the same text and a different SCOPE
// hashed identically and replayed the first receipt — the second capture
// silently inherited the first one's placement. The two are kept separate rather
// than reconciled because the fingerprint is a published contract field and this
// is the kernel's own idempotency identity.
//
// # captured_at is part of the identity, and that is load-bearing
//
// It was excluded in round two, on the reasoning that a retry which re-stamps
// its clock is still the same capture. That reasoning was wrong in a way that
// cost exactly-once: the vault id derives its timestamp from captured_at, so a
// retry with a fresh stamp derived a DIFFERENT id, aimed at a different vault
// path, and the create-exclusive publish had nothing to refuse. Including it
// makes the id stable by construction — the identity and the id move together or
// not at all — and turns a re-stamped retry into an idempotency_conflict, which
// is the honest answer: a capture whose claimed time changed is not the capture
// the key was issued for.
//
// The client-side rule that falls out is stated in the contract:
// **captured_at is immutable for a given idempotency key.** A phone queue that
// preserves the stamp across a relaunch (N23) satisfies it by construction.
//
// Excluded deliberately: the idempotency key itself, which is the LOOKUP and
// would make every capture its own identity, so no reuse could ever conflict.
//
// The digest is over canonical JSON — a fixed field order, one encoder — so two
// runs of the same build agree byte for byte.
func captureIdentity(c Capture) string {
	canonical := struct {
		Schema     string `json:"schema"`
		Version    int    `json:"schema_version"`
		Device     string `json:"device_id"`
		CapturedAt string `json:"captured_at"`
		Lane       Lane   `json:"requested_lane"`
		Intent     Intent `json:"intent"`
		Scope      string `json:"scope"`
		Text       string `json:"text"`
	}{
		Schema:     SchemaCapture,
		Version:    SchemaVersion,
		Device:     c.DeviceID,
		CapturedAt: c.CapturedAt,
		Lane:       c.RequestedLane,
		Intent:     c.Intent,
		Scope:      c.Scope,
		Text:       c.Text,
	}
	// The encoder cannot fail on this struct: every field is a string or an int.
	body, _ := json.Marshal(canonical)
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// captureMemoryID derives the vault id a capture publishes under.
//
// It is the exactly-once primitive. Because the id is derived rather than minted,
// every attempt at the same capture aims at the same vault path, and the kernel's
// create-exclusive publish refuses the second write — so a crash anywhere between
// the reservation and the settle costs a retry, never a duplicate memory. The
// reservation records the id, but nothing DEPENDS on the record surviving: a
// swept reservation re-derives the same id.
//
// The shape is Mora's own, `mem_YYYYMMDD_HHMMSS_<8 hex>`, which is what the
// contract corpus normalises. The time half is the capture's own captured_at
// rather than the mint instant — a real timestamp, and the more honest of the
// two, because when the user wrote the note is a fact about the note and when
// the Mac got round to it is not.
//
// captured_at is inside the identity as well as inside the id, which is what
// makes the id stable: the two cannot disagree. A capture that changes its stamp
// changes its identity, so it is refused as a conflict long before it could
// derive a second path.
func captureMemoryID(c Capture, identity string) string {
	sum := sha256.Sum256([]byte(c.DeviceID + "\x00" + c.IdempotencyKey + "\x00" + identity))
	stamp := "00000000_000000"
	if t, err := time.Parse(time.RFC3339, c.CapturedAt); err == nil {
		stamp = t.UTC().Format("20060102_150405")
	}
	return fmt.Sprintf("%s%s_%s", PrefixMemory, stamp, hex.EncodeToString(sum[:4]))
}

// ---------------------------------------------------------------------------
// The handler
// ---------------------------------------------------------------------------

// handleCapture serves POST RouteCapture.
//
// The body is read and decoded BEFORE the work budget is taken, for the reason
// handleContext explains: a slow client dribbling a body must not hold the Mac's
// only kernel slot, and a malformed body should cost a decode rather than a slot.
func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.admit(w, r, http.MethodPost)
	if !ok {
		return
	}
	received := s.now().UTC().Truncate(time.Second)

	// A capture is bounded more tightly than any other request: MaxCaptureBytes,
	// not MaxRequestBytes. The guard chain's bound is the ceiling for the whole
	// surface; this is the one for the route that turns bytes into a file.
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxCaptureBytes+1))
	if err != nil || len(body) > MaxCaptureBytes {
		writeOpaque(w, http.StatusRequestEntityTooLarge, CodeTooLarge)
		return
	}
	// Strict inbound, and into a ZERO value rather than NewCapture(): decoding
	// into a constructor would pre-fill schema and schema_version, so a body that
	// omitted them would inherit the right answer and pass a check it never
	// faced. The rejection carries the schema code and the field path and never
	// the value that failed.
	var capture Capture
	if err := Unmarshal(body, &capture); err != nil {
		var schemaErr *Error
		if errors.As(err, &schemaErr) {
			writeRejection(w, http.StatusBadRequest, schemaErr)
			return
		}
		writeOpaque(w, http.StatusBadRequest, CodeMalformed)
		return
	}

	// stillLive re-asks the registry the question the guard chain answered when
	// the request arrived. It is a closure rather than a stored Device because
	// the answer has to be recomputed, not remembered: revocation is the whole
	// point, and a Device value captured at admission is exactly the stale answer.
	stillLive := func() bool {
		_, ok := s.authorize(r)
		return ok
	}

	var response []byte
	if !s.budgeted(w, r, func(ctx context.Context) error {
		var err error
		response, err = s.capture(ctx, dev, capture, received, stillLive)
		return err
	}) {
		return
	}
	// Every DECODABLE capture answers 200 with a receipt, rejections included.
	//
	// That is N12's rule applied to a write: the status code says whether the
	// request was served, and the body says what happened to the vault. A policy
	// refusal is a served request that produced a terminal receipt, and a client
	// that had to read a rejection out of a 4xx body would be parsing two
	// vocabularies for one answer. The 4xx and 5xx codes are reserved for the
	// cases where there is no receipt to give: no credential, an oversize body,
	// a body that does not decode, a busy or unreachable kernel.
	if s.writeCaptureBody(w, response) {
		s.markSeen(dev.DeviceID)
	}
}

// logIntegrityEvent is the ONE line this listener writes about a request, and
// the documented exception to "it logs nothing per request".
//
// The rule that exception has to respect is what the rule is FOR: a per-request
// log leaks what a device asked and what it was told. This is neither. It fires
// only when the vault holds a memory at a pinned id that is not the capture
// claiming it — a tampered or collided vault, which is an incident rather than a
// served request — and it carries two kernel-derived identifiers and nothing
// else. Not the token, not the text, not the receipt.
//
// It goes to the Server's own log sink, the one N12 gave it for the startup
// banner, so a test that hands the listener a buffer sees silence across a
// healthy exchange (TestServerLogsNothingPerRequest) and sees this when the
// vault is broken.
func (s *Server) logIntegrityEvent(detail string) {
	if detail == "" {
		return
	}
	fmt.Fprintf(s.log, "companion capture: refusing to claim a memory — %s; "+
		"inspect it before re-pairing, and see docs/companion-contract.md\n", detail)
}

// writeCaptureBody writes the exact bytes a capture settled with.
//
// It writes BYTES rather than re-marshalling a receipt, because a replay must be
// byte-identical on the wire: a client that hashes or caches a response body is
// entitled to have the retry match it exactly, and a re-marshalling is only ever
// equal by luck of the encoder. The headers are the ones every other projection
// carries.
func (s *Server) writeCaptureBody(w http.ResponseWriter, body []byte) bool {
	if len(body) == 0 {
		writeOpaque(w, http.StatusInternalServerError, "internal")
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return true
}

// ---------------------------------------------------------------------------
// The capture path
// ---------------------------------------------------------------------------

// capture runs one governed capture: reserve, publish at the pinned id, settle.
// It returns the exact bytes the response body carries.
func (s *Server) capture(ctx context.Context, dev Device, c Capture, received time.Time, stillLive func() bool) ([]byte, error) {
	// A device may not capture as another device. The claim is compared, never
	// trusted, and the receipt is stamped with the AUTHENTICATED id — echoing the
	// claimed one back would put a device identifier of the caller's choosing
	// into a document the phone stores.
	if c.DeviceID != dev.DeviceID {
		return s.refuse(ctx, dev, c, ReasonUnknownDevice, received)
	}
	// v1 captures the memory lane only. `ask` is what the context route is for,
	// and `investigate` is the async research lane, which has no vault and no
	// worker yet — saying so with the published reason is honest, where routing
	// it to a spine that does not exist would not be.
	if c.Intent != IntentRemember {
		return s.refuse(ctx, dev, c, ReasonUnsupportedLane, received)
	}

	identity := captureIdentity(c)

	// The published index is consulted BEFORE anything is reserved.
	//
	// A capture killed after its publication leaves a pending reservation, and
	// past the sweep window that row is collected — so the key looks unused, a
	// re-stamped retry is a new identity, and it derives a second vault id that
	// nothing holds. The ownership record is the durable audit trail and outlives
	// the reservation, so it is what answers "has this key already published?".
	//
	// A key that published something ELSE is a conflict. A key that published
	// THIS capture falls through: the derivation is deterministic, so the write
	// below finds its own memory already there and settles applied without
	// writing again.
	if published, found, perr := s.writer.PublishedForKey(ctx, dev.DeviceID, c.IdempotencyKey); perr != nil {
		return nil, perr
	} else if found && published != identity {
		return s.refuse(ctx, dev, c, ReasonIdempotencyConflict, received)
	}

	replay, claim, err := s.captures.Reserve(CaptureIdentity{
		DeviceID:    dev.DeviceID,
		Key:         c.IdempotencyKey,
		Identity:    identity,
		Fingerprint: c.PayloadFingerprint,
		MemoryID:    captureMemoryID(c, identity),
	})
	switch {
	case errors.Is(err, ErrIdempotencyConflict):
		// The key belongs to the first capture and keeps belonging to it. This
		// receipt is deliberately NOT stored: storing it would let the second
		// capture take the key it was just refused.
		return s.refuse(ctx, dev, c, ReasonIdempotencyConflict, received)
	case err != nil:
		return nil, err
	}
	if claim == nil {
		// The replay answer: the bytes the first attempt returned, not a
		// re-marshalling of the same fields.
		return replay, nil
	}
	settled := false
	// The claim is released on every path. Abandoning leaves the PENDING record
	// on disk on purpose: inside the takeover window it is what a concurrent
	// retry sees instead of racing. Past it the record is swept, and losing it
	// costs nothing — the pinned id is what stops a second write.
	defer func() {
		if !settled {
			claim.Abandon()
		}
	}()

	outcome, err := s.publish(ctx, c, claim, stillLive)
	if err != nil {
		// The kernel could not decide. No receipt is invented: a rejected
		// receipt would claim the capture will NEVER be applied, which is a
		// stronger statement than "this attempt failed", and it is the statement
		// that would stop the phone retrying.
		return nil, err
	}
	s.logIntegrityEvent(outcome.IntegrityDetail)
	receipt, err := s.receipt(dev, c, outcome, received)
	if err != nil {
		return nil, err
	}
	body, err := Marshal(&receipt)
	if err != nil {
		return nil, err
	}
	if err := claim.Settle(receipt, body); err != nil {
		return nil, err
	}
	settled = true
	return body, nil
}

// publish decides what actually happens to the vault for one held claim.
//
// Two checks stand between the reservation and the write, and both exist because
// the request has been admitted for a while by the time it gets here:
//
//   - A reclaimed claim asks whether the pinned id is ALREADY published. It
//     usually is not, and the create-exclusive publish would catch it anyway;
//     asking first is what turns "the retry cannot duplicate" into "the retry
//     does not even try", which is the difference between a bounded race and a
//     wasted vault write on every recovery.
//   - Every claim re-checks the credential. Authentication happened when the
//     request arrived; an operator who revokes a device while a request is
//     sitting in the work-budget queue has said no, and a write that lands
//     afterwards would be a write the operator revoked the right to make.
func (s *Server) publish(ctx context.Context, c Capture, claim *Claim, stillLive func() bool) (WriteOutcome, error) {
	if claim.TakenOver {
		outcome, published, err := s.writer.Published(ctx, c, claim.Identity())
		if err != nil {
			return WriteOutcome{}, err
		}
		if published {
			// A crashed attempt got its write in before it died. Finishing its
			// receipt is the whole job; writing again is the duplicate. The
			// outcome may also be a REJECTION: a file at the pinned id that is
			// not this capture is a vault-integrity failure, and settling it is
			// how the key stops being retried forever.
			return outcome, nil
		}
	}
	if !stillLive() {
		// The device was revoked between admission and the write. Nothing is
		// written and the reservation SETTLES rather than staying pending, so a
		// revoked device's claim is closed rather than left for a takeover.
		policy, err := s.writer.Policy(ctx)
		if err != nil {
			policy = PolicyReadonly
		}
		return WriteOutcome{Policy: policy, State: ReceiptRejected, Reason: ReasonUnknownDevice}, nil
	}
	return s.writer.Publish(ctx, c, claim.Identity())
}

// refuse builds a terminal rejection for a capture the kernel was never asked to
// write, and returns its bytes.
//
// It still reads the policy, because a receipt names the policy in force whatever
// the reason — an operator reading a rejected receipt a week later should not
// have to guess which policy the Mac was under. A policy that cannot be read is
// readonly: the fail-closed direction, and the only one that cannot describe a
// write as permitted.
func (s *Server) refuse(ctx context.Context, dev Device, c Capture, reason RejectReason, received time.Time) ([]byte, error) {
	policy, err := s.writer.Policy(ctx)
	if err != nil {
		policy = PolicyReadonly
	}
	receipt, err := s.receipt(dev, c, WriteOutcome{Policy: policy, State: ReceiptRejected, Reason: reason}, received)
	if err != nil {
		return nil, err
	}
	return Marshal(&receipt)
}

// receipt assembles and validates one terminal receipt.
//
// Validate is what binds the state to the policy (N02's table) and what forbids
// an applied receipt that names no memory. Building the receipt and validating it
// in one place means no caller can assemble a shape the contract does not permit.
func (s *Server) receipt(dev Device, c Capture, o WriteOutcome, received time.Time) (Receipt, error) {
	receiptID, err := s.captures.NewID(PrefixReceipt)
	if err != nil {
		return Receipt{}, err
	}
	requestID, err := s.captures.NewID(PrefixRequest)
	if err != nil {
		return Receipt{}, err
	}
	out := NewReceipt()
	out.ReceiptID = receiptID
	out.RequestID = requestID
	out.IdempotencyKey = c.IdempotencyKey
	out.DeviceID = dev.DeviceID
	out.State = o.State
	out.Reason = o.Reason
	out.MemoryID = o.MemoryID
	out.PayloadFingerprint = c.PayloadFingerprint
	out.Policy = o.Policy
	out.ReceivedAt = reservationStamp(received)
	// An accepted capture is staged and NOT settled: it is waiting for a human
	// at the Mac, and stamping settled_at would say the question is closed.
	// Applied and rejected are both terminal and both carry the stamp.
	if o.State != ReceiptAccepted {
		out.SettledAt = reservationStamp(s.now())
	}
	if err := out.Validate(); err != nil {
		return Receipt{}, err
	}
	return out, nil
}
