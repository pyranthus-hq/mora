package companion

// pairing_route.go is the kernel-side half of pairing (graph node N12b).
//
// N11 shipped `mora companion pair`, which registers a PENDING device and
// prints a one-time code, and Registry.Confirm, which spends that code and mints
// the bearer token. Nothing called Confirm. A phone could scan a QR payload and
// then had no way to hand the code back, so no device ever reached ACTIVE and
// every request the listener served was a 401. This file is the missing call.
//
// # It is the only unauthenticated route, and that is the whole design problem
//
// Every other route on this listener is reached with a device token. This one
// cannot be: the request that asks for the token cannot carry it. So the route
// is the one place a stranger on the listener's network path gets to run kernel
// code, and it is written accordingly:
//
//	one slot         at most ONE confirmation is in flight process-wide, and
//	                 the next is refused immediately rather than queued
//	one refusal      wrong code, expired code, replayed confirmation and an
//	                 unknown device are the same opaque 401
//	one floor        every answer, refusal and success alike, takes at least
//	                 PairingFloor, so the cheap refusals and the expensive ones
//	                 are not separable by a stopwatch
//	one budget       MaxPairingAttempts wrong codes against a live pairing and
//	                 the pairing is REVOKED, with a receipt. The count is
//	                 DURABLE — it lives in the pending device's record, so a
//	                 restart does not hand an attacker five fresh guesses
//	no logging       the handler writes nothing to the listener's log, and the
//	                 token it hands back exists only in the response body
//
// # Why a lockout can never revoke an ACTIVE device
//
// The attempt counter is keyed by device id, and a device id is not a secret —
// it is printed by `mora companion pair`, it travels in the QR payload, and it
// is short enough to guess. If any failed confirmation incremented the counter,
// anyone who could name a device id could spend five requests and revoke a
// working phone.
//
// So only ONE outcome counts: a wrong code against a device that is pending and
// still holds a live pairing code (ErrPairingCode). An unknown device id is not
// counted, and neither is a confirmation for a device that is already active or
// already revoked (ErrNotPending) — there is no code there to brute force, so
// there is nothing for a lockout to protect and nothing for it to break. The
// only device a lockout can revoke is one that is mid-pairing, which is the
// device whose code is under attack.
//
// # What the floor buys, and what it does not
//
// The refusal paths through Registry.Confirm are not equally expensive. An
// unknown device id returns before any comparison; a wrong code runs the
// constant-time comparison and writes nothing; a matching-but-expired code burns
// the code and writes the record file and a receipt. Those differ by orders of
// magnitude, and the difference is measurable over a network.
//
// The floor removes that difference by making every answer take the same
// MINIMUM time. It is honest about its limits: it is a floor, not a clamp, so a
// path that overruns it is still slower than one that does not, and it is not a
// cryptographic constant-time guarantee at the level of branches or cache lines.
// What it does is collapse the three-orders-of-magnitude gap between "returned
// immediately" and "wrote two files" into noise, on a route that is already
// limited to one request at a time. The comparison that actually decides the
// answer is constant-time in Registry.Confirm and always has been.

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// SchemaPairingGrant names the document a successful confirmation returns.
//
// It is a NEW published schema rather than a reuse of an existing one, because
// no existing one may carry a token: mora.companion.device exists precisely to
// identify a credential WITHOUT carrying it, and mora.companion.pairing.confirmation
// is the phone's request, not the kernel's answer. Adding a name is additive —
// nothing that already decodes the v1 contract has to change — which is the same
// rule N02 fixed for adding an enum value.
const SchemaPairingGrant = "mora.companion.pairing.grant"

const (
	// MaxPairingAttempts is how many wrong codes one pending pairing survives.
	//
	// Five, not because five is special, but because a human mistyping a code
	// in the fallback flow gets a few tries and a program guessing a 160-bit
	// secret gets nowhere either way. The value that actually stops a brute
	// force is the code's entropy; this bound stops the ATTEMPTS from being
	// free, and turns a sustained guess into a revocation the operator can see
	// in the receipts directory.
	MaxPairingAttempts = 5

	// maxInFlightConfirmations is the route's own concurrency budget. ONE, and
	// it is deliberately not shared with maxInFlightKernelCalls: a confirmation
	// takes the registry's cross-process write lock rather than reading the
	// vault, so neither budget should be able to exhaust the other.
	maxInFlightConfirmations = 1

	// PairingFloor is the minimum time one confirmation takes. See the file
	// comment for what it buys.
	//
	// 250ms is chosen against the slowest path it has to cover: a matching but
	// expired code takes the registry write lock and publishes two files. It is
	// also the rate limit's other half — one slot times this floor caps the
	// whole route at four attempts a second, listener-wide.
	PairingFloor = 250 * time.Millisecond
)

// Confirmer is the pairing seam. *Registry implements it.
//
// Revoke is on this interface and not on Authenticator because the lockout is
// this route's own act: five wrong codes end the pairing, and the thing that
// decides that is the thing that must be able to carry it out.
type Confirmer interface {
	// PairingBudget is the DURABLE attempt count, read before any guess is
	// tried, so a restart does not hand an attacker five fresh attempts.
	PairingBudget(deviceID string) (attempts int, pending bool, err error)
	RecordPairingFailure(deviceID string) (int, error)
	Confirm(c PairingConfirmation) (string, Device, error)
	Revoke(deviceID string) (Device, bool, error)
}

// ---------------------------------------------------------------------------
// The grant
// ---------------------------------------------------------------------------

// PairingGrant is what a successful confirmation returns. It is the ONLY
// document in this contract that carries a bearer token, it is returned exactly
// once per pairing, and it is never written to disk or to a log.
//
// TokenFingerprint travels beside the token deliberately. It is the same string
// `mora companion list` prints, so a person can hold the phone next to the Mac
// and check that the credential the phone stored is the credential the Mac
// issued — the confirmation half of the host fingerprint the QR payload carries.
type PairingGrant struct {
	Header
	DeviceID         string `json:"device_id"`
	Token            string `json:"token"`
	TokenFingerprint string `json:"token_fingerprint"`
	IssuedAt         string `json:"issued_at"`
}

// NewPairingGrant returns a grant with its envelope filled in.
func NewPairingGrant() PairingGrant {
	return PairingGrant{Header: newHeader(SchemaPairingGrant)}
}

func (g *PairingGrant) SchemaName() string { return SchemaPairingGrant }

// ByteLimit is the small bound, not the projection bound. This document is four
// short strings and will never legitimately be larger; giving it 4 MiB of
// headroom would only widen what a producer bug could emit.
func (g *PairingGrant) ByteLimit() int { return MaxOperationBytes }

// Redacted returns a copy with the token masked, for any path that prints, logs
// or reports a grant. Nothing in the kernel does — this exists so that the next
// caller who reaches for one has a safe way to.
func (g PairingGrant) Redacted() PairingGrant {
	if g.Token != "" {
		g.Token = "[redacted]"
	}
	return g
}

func (g *PairingGrant) Validate() error {
	if err := g.validate(SchemaPairingGrant); err != nil {
		return err
	}
	if err := validateID("device_id", PrefixDevice, g.DeviceID); err != nil {
		return err
	}
	// The token's format is a kernel decision and is opaque on the wire — the
	// same treatment the pairing code gets — so it is bounded and required, and
	// nothing here parses it.
	if err := validateText("token", g.Token, MaxIdempotencyKeyBytes, true); err != nil {
		return err
	}
	if err := validateFingerprint("token_fingerprint", g.TokenFingerprint); err != nil {
		return err
	}
	// The fingerprint must cover the token it travels WITH, through the same
	// derivation N11 stores and `mora companion list` prints. A well-formed
	// digest of some other string is the shape this check exists to refuse: the
	// whole reason the field is here is that a person compares it against the
	// Mac, and a grant whose two halves disagree would send them to compare a
	// value that describes nothing they hold.
	if g.TokenFingerprint != Fingerprint(g.Token) {
		return errf(CodeInvalidValue, "token_fingerprint",
			"the fingerprint does not cover the token it travels with")
	}
	return validateTimestamp("issued_at", g.IssuedAt)
}

// ---------------------------------------------------------------------------
// The attempt budget
// ---------------------------------------------------------------------------

// The budget is DURABLE and lives in the pending device's own record. It used
// to be a map in this process, and that was wrong in a way a restart makes
// obvious: an in-memory counter hands an attacker five fresh guesses every time
// the listener is restarted, and an attacker who can make the process exit — or
// who simply waits for the operator to restart it — has no budget at all.
//
// Registry.PairingBudget is the read and Registry.RecordPairingFailure is the
// write. Both refuse to touch a device that is not pending with a live code, so
// the counter can never accumulate against an active or revoked one; see
// RecordPairingFailure for why that restriction is the security property.

// pairingRefused reports whether err is one of Registry.Confirm's refusals.
//
// It exists because the ONE error the handler tolerates — ErrReceiptNotWritten,
// meaning the change committed and only its audit row did not — can arrive
// JOINED to a refusal. Registry.Confirm returns errors.Join(ErrPairingExpired,
// ErrReceiptNotWritten) when a matching-but-late code is burned and the burn's
// receipt fails to write. Treating any receipt warning as success there built a
// grant out of an empty token and answered 500, which is both a distinguishable
// refusal and a lie about what happened.
func pairingRefused(err error) bool {
	return errors.Is(err, ErrPairingCode) ||
		errors.Is(err, ErrPairingExpired) ||
		errors.Is(err, ErrNotPending) ||
		errors.Is(err, ErrNoSuchDevice)
}

// grantIssued reports whether Confirm actually minted a credential.
//
// Two independent conditions, deliberately: a token was returned, AND no
// refusal is joined to the error. Either alone would do today — Confirm returns
// "" for every refusal — but this is the predicate that decides whether an
// unauthenticated caller gets a 401 or a bearer token, and it should not depend
// on one invariant in another file staying true.
func grantIssued(token string, err error) bool {
	return token != "" && !pairingRefused(err)
}

// ---------------------------------------------------------------------------
// The route
// ---------------------------------------------------------------------------

// handleConfirm serves POST RouteConfirm.
//
// The order is: method, one slot, body, decode, budget, registry, floor,
// answer. Everything before the slot is free; everything after it is serialized
// process-wide.
func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	// The method check is first and costs nothing, exactly as it is in admit.
	// A method this route does not serve is not a question about a credential,
	// and answering it with a 401 would be a lie.
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOpaque(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}

	// ONE confirmation at a time, listener-wide, refused immediately rather
	// than queued. This is a separate slot from the kernel's: a confirmation
	// takes the registry's cross-process write lock and must not be able to
	// shut a paired phone out of `today` while it does, and a phone reading
	// `today` must not be able to hold pairing up either.
	release, ok := s.pairingSlot(w)
	if !ok {
		return
	}
	defer release()

	// The floor starts once the request is actually being served. Time spent
	// waiting for the slot is not part of it: a caller that was refused the
	// slot never reached a credential decision, so padding it would only make
	// the refusal slower without hiding anything.
	settle := s.pairingFloor()

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil || len(body) > MaxRequestBytes {
		settle()
		writeOpaque(w, http.StatusRequestEntityTooLarge, CodeTooLarge)
		return
	}
	// Strict inbound, exactly as the context and capture routes decode: unknown
	// fields, unknown enum values, oversize text and malformed timestamps are
	// refused here. The zero value is load-bearing — decoding into
	// NewPairingConfirmation() would pre-fill the envelope a body omitted.
	//
	// A malformed body is answered 400 with the schema code, NOT the opaque
	// 401. It is not a claim about a credential: the caller wrote the body and
	// already knows what is in it, so a coded refusal tells an attacker nothing
	// and tells a real client implementer everything.
	var c PairingConfirmation
	if err := Unmarshal(body, &c); err != nil {
		settle()
		var schemaErr *Error
		if errors.As(err, &schemaErr) {
			writeRejection(w, http.StatusBadRequest, schemaErr)
			return
		}
		writeOpaque(w, http.StatusBadRequest, CodeMalformed)
		return
	}

	// The durable budget is read BEFORE any guess is tried. A locked-out caller
	// costs one bounded registry READ — never the cross-process write lock —
	// so it cannot stall `mora companion pair` or `revoke` from the network.
	attempts, pending, err := s.confirms.PairingBudget(c.DeviceID)
	if err != nil {
		// Fail CLOSED. A budget that cannot be read is a budget that cannot be
		// enforced, and a route that cannot enforce its own rate limit must not
		// keep answering guesses.
		settle()
		writeUnauthorized(w)
		return
	}
	if attempts >= MaxPairingAttempts {
		// The budget is spent. If the record is STILL pending, the revocation
		// the budget called for did not take — a transient lock loss, a failed
		// write — so it is retried here rather than left undone. This is what
		// "the revoke result is checked" buys: not a different answer to this
		// caller, who gets the same 401 either way, but a revocation that is
		// retried on every subsequent attempt until it lands.
		if pending {
			_, _, rerr := s.confirms.Revoke(c.DeviceID)
			if rerr != nil && !errors.Is(rerr, ErrReceiptNotWritten) {
				// Still refused, and the record stays pending, so the next
				// attempt comes back through this branch and tries again.
				settle()
				writeUnauthorized(w)
				return
			}
		}
		settle()
		writeUnauthorized(w)
		return
	}

	token, dev, err := s.confirms.Confirm(c)
	// ErrReceiptNotWritten is the ONE error tolerated here, and only alongside a
	// credential that was actually minted: it means the device IS active and
	// this token is the only copy of it, so discarding it because an audit row
	// failed would strand exactly the credential nobody holds that the
	// record-first ordering exists to prevent.
	//
	// It is NOT tolerated on a refusal. Confirm joins it to ErrPairingExpired
	// when a matching-but-late code is burned and the burn's receipt fails to
	// write, and treating that as success built a grant out of an empty token
	// and answered 500 — a refusal an attacker could tell apart from every
	// other one, and a 500 that claimed something had gone wrong on the Mac
	// rather than with the code.
	// The ONE tolerated failure, named rather than inlined: a credential was
	// actually minted, and the only thing that went wrong is its audit row.
	unaudited := grantIssued(token, err) && errors.Is(err, ErrReceiptNotWritten)
	if err != nil && !unaudited {
		// Only a wrong code against a live pending pairing spends the budget.
		// See RecordPairingFailure for why an unknown device and an
		// already-settled one deliberately do not.
		if errors.Is(err, ErrPairingCode) {
			spent, ferr := s.confirms.RecordPairingFailure(c.DeviceID)
			if ferr == nil && spent >= MaxPairingAttempts {
				// The revocation is what makes the budget mean something. Its
				// result is CHECKED rather than discarded: a failure leaves the
				// record pending with the count already durable, so the branch
				// above retries it on the next attempt.
				_, _, _ = s.confirms.Revoke(c.DeviceID)
			}
		}
		settle()
		writeUnauthorized(w)
		return
	}

	grant := NewPairingGrant()
	grant.DeviceID = dev.DeviceID
	grant.Token = token
	grant.TokenFingerprint = dev.TokenFingerprint
	grant.IssuedAt = s.now().UTC().Truncate(time.Second).Format(time.RFC3339)

	settle()
	// The stamp is gated on the response actually being a 2xx, the same rule
	// every other route follows: a grant the contract refuses is a 500, and a
	// 500 served nothing. This is the device's FIRST seen, so the debounce in
	// markSeen always lets it through.
	if s.writePayload(w, &grant) {
		s.markSeen(dev.DeviceID)
	}
}

// pairingSlot takes the route's single slot or refuses immediately.
//
// It is deliberately not s.acquire: that one guards the kernel's read budget,
// and sharing it would let a confirmation and a projection block each other for
// no reason. The refusal is the same shape — 503 with a Retry-After — because
// the remedy is the same: come back in a moment.
func (s *Server) pairingSlot(w http.ResponseWriter) (func(), bool) {
	select {
	case s.pairing <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.pairing }) }, true
	default:
		w.Header().Set("Retry-After", strconv.Itoa(RetryAfterSeconds))
		writeOpaque(w, http.StatusServiceUnavailable, "busy")
		return nil, false
	}
}

// pairingFloor starts the minimum-duration clock and returns the function that
// waits out whatever is left of it.
//
// It reads time.Now rather than the injected clock on purpose. s.now is the
// contract's clock — a test pins it to a constant, and a constant cannot measure
// an elapsed interval. The floor is a real-time property of the wire, so it is
// measured in real time, and it is the DURATION that is injectable instead.
func (s *Server) pairingFloor() func() {
	if s.pairingMinimum <= 0 {
		return func() {}
	}
	start := time.Now()
	return func() {
		if rest := s.pairingMinimum - time.Since(start); rest > 0 {
			time.Sleep(rest)
		}
	}
}
