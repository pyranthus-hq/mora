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
//	                 the pairing is REVOKED, with a receipt
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

	// maxTrackedPairings bounds the attempt table.
	//
	// Only a device the registry recognizes AND finds pending can enter it, and
	// pending devices are bounded by MaxDevices, so a remote caller cannot grow
	// this table at all — every entry costs a local `mora companion pair`. The
	// cap is the backstop for a listener that outlives many pairing cycles.
	// Reaching it fails CLOSED: the route stops confirming until the listener
	// is restarted, because a full table means the lockout can no longer be
	// enforced, and a route that cannot enforce its own rate limit must not
	// keep answering.
	maxTrackedPairings = 256

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
	return validateTimestamp("issued_at", g.IssuedAt)
}

// ---------------------------------------------------------------------------
// The attempt budget
// ---------------------------------------------------------------------------

// pairingLimiter is the per-listener attempt budget. It lives in memory: a
// restart forgets the count, which is correct, because a restart also drops
// every socket an attacker held and the operator is the one who did it.
type pairingLimiter struct {
	mu       sync.Mutex
	failures map[string]int
	// full latches once the table has hit its cap. See maxTrackedPairings for
	// why that fails closed rather than falling back to no limit at all.
	full bool
}

func newPairingLimiter() *pairingLimiter {
	return &pairingLimiter{failures: map[string]int{}}
}

// blocked reports whether a confirmation for this device must be refused
// without reaching the registry at all.
func (l *pairingLimiter) blocked(deviceID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.full || l.failures[deviceID] >= MaxPairingAttempts
}

// fail records one wrong code against a live pending pairing and reports
// whether this attempt was the one that spent the budget.
//
// It reports true EXACTLY once per device, on the attempt that reaches the cap,
// so the revocation below happens once rather than on every subsequent request.
func (l *pairingLimiter) fail(deviceID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, tracked := l.failures[deviceID]; !tracked && len(l.failures) >= maxTrackedPairings {
		l.full = true
		return false
	}
	l.failures[deviceID]++
	return l.failures[deviceID] == MaxPairingAttempts
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

	// The budget is checked BEFORE the registry, so a device whose pairing is
	// already spent cannot make a locked-out caller cost a file lock per
	// request.
	if s.pairings.blocked(c.DeviceID) {
		settle()
		writeUnauthorized(w)
		return
	}

	token, dev, err := s.confirms.Confirm(c)
	// ErrReceiptNotWritten means the device IS active and this token is the
	// only copy of its credential — see Registry.Confirm. Discarding it here
	// because an audit row failed would strand exactly the credential nobody
	// holds that the record-first ordering exists to prevent.
	if err != nil && !errors.Is(err, ErrReceiptNotWritten) {
		// Only a wrong code against a live pending pairing spends the budget.
		// See the file comment for why an unknown device and an already-settled
		// one deliberately do not.
		if errors.Is(err, ErrPairingCode) && s.pairings.fail(c.DeviceID) {
			// The revocation is what makes the budget mean something: the
			// pairing under attack ends, and Registry.Revoke writes the
			// `revoked` receipt record-first. Its outcome does not change the
			// answer — a caller that just spent the budget gets the same 401
			// either way — so it is deliberately not inspected.
			_, _, _ = s.confirms.Revoke(c.DeviceID)
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
