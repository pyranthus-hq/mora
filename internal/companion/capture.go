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
// "Only after" is the load-bearing half. The receipt state is derived from what
// the kernel's governed write path actually did, not from what it was asked to
// do, so `applied` cannot be claimed for a write that failed. A device may show
// the word "Saved" for exactly one of those three states.
//
// # One write path, not a second one
//
// The kernel side of Writer goes through the SAME governed write the CLI's
// `mora write` and the MCP write_memory tool use. This listener opens no second
// door into the vault: it decides who may knock and what the request must look
// like, and the kernel decides what happens to the vault.
//
// # Retries are the normal case
//
// A phone retries. See idempotency.go for the reservation ordering; the shape
// here is that the reservation is durable BEFORE the kernel is asked to write,
// and the terminal receipt is stored into that reservation afterwards, so the
// same key with the same payload returns the same receipt bytes and the same
// key with a different payload is a conflict rather than an overwrite.
//
// # What a receipt never carries
//
// Not the text, not a snippet of it, not a title derived from it. A receipt is
// identifiers, a state, a policy, a fingerprint and two timestamps.
// TestCaptureReceiptNeverEchoesThePayload drives the real handler and fails on
// any word from the capture appearing in the response.

import (
	"context"
	"errors"
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
}

// Writer is the kernel's governed-write seam, the mirror of Reader.
//
// It has two methods and neither takes a policy: the kernel reads the policy
// itself, per request, from the vault it owns. A seam that accepted a policy
// would be a seam a caller could lie to.
type Writer interface {
	// Policy reports the vault's current write policy. It is used only to stamp
	// a receipt for a capture that is refused before the kernel is asked to
	// write anything, so that even those receipts name the policy in force.
	Policy(ctx context.Context) (WritePolicy, error)
	// Publish runs the capture through the kernel's existing governed write
	// path and reports what happened.
	Publish(ctx context.Context, c Capture) (WriteOutcome, error)
}

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

	var receipt Receipt
	if !s.budgeted(w, r, func(ctx context.Context) error {
		var err error
		receipt, err = s.capture(ctx, dev, capture, received)
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
	if s.writePayload(w, &receipt) {
		s.markSeen(dev.DeviceID)
	}
}

// capture runs one governed capture: reserve, publish, settle.
func (s *Server) capture(ctx context.Context, dev Device, c Capture, received time.Time) (Receipt, error) {
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

	stored, claim, err := s.captures.Reserve(dev.DeviceID, c.IdempotencyKey, c.PayloadFingerprint)
	switch {
	case errors.Is(err, ErrIdempotencyConflict):
		// The key belongs to the first payload and keeps belonging to it. This
		// receipt is deliberately NOT stored: storing it would let the second
		// payload take the key it was just refused.
		return s.refuse(ctx, dev, c, ReasonIdempotencyConflict, received)
	case err != nil:
		return Receipt{}, err
	}
	if claim == nil {
		// The replay answer, returned from storage rather than rebuilt, so it is
		// the same bytes and the same receipt id the first attempt produced.
		return stored, nil
	}
	settled := false
	// The claim is released on every path. Abandoning leaves the PENDING record
	// on disk on purpose: it is the durable evidence that this key was claimed,
	// and it is what a retry after a crash completes instead of duplicating.
	defer func() {
		if !settled {
			claim.Abandon()
		}
	}()

	outcome, err := s.writer.Publish(ctx, c)
	if err != nil {
		// The kernel could not decide. No receipt is invented: a rejected
		// receipt would claim the capture will NEVER be applied, which is a
		// stronger statement than "this attempt failed", and it is the statement
		// that would stop the phone retrying.
		return Receipt{}, err
	}
	receipt, err := s.receipt(dev, c, outcome, received)
	if err != nil {
		return Receipt{}, err
	}
	if err := claim.Settle(receipt); err != nil {
		return Receipt{}, err
	}
	settled = true
	return receipt, nil
}

// refuse builds a terminal rejection for a capture the kernel was never asked to
// write.
//
// It still reads the policy, because a receipt names the policy in force whatever
// the reason — an operator reading a rejected receipt a week later should not
// have to guess which policy the Mac was under.
func (s *Server) refuse(ctx context.Context, dev Device, c Capture, reason RejectReason, received time.Time) (Receipt, error) {
	policy, err := s.writer.Policy(ctx)
	if err != nil {
		return Receipt{}, err
	}
	return s.receipt(dev, c, WriteOutcome{Policy: policy, State: ReceiptRejected, Reason: reason}, received)
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
