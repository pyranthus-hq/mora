package companion

// pairing_route_test.go drives the ONE unauthenticated route.
//
// Every test here is about a property that only matters because the route takes
// no credential: what a stranger can learn from a refusal, what a stranger can
// cost the Mac, and what a stranger cannot do to a device that is already
// paired. The happy path is one test; the rest are the boundary.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// confirmServer is testServer with a registry that has NO confirmed device, so
// a test can prove the route works before anything can authenticate — which is
// the situation a real first pairing is in.
func confirmServer(t *testing.T) (*Server, *Registry, *bytes.Buffer) {
	t.Helper()
	reg, _, _, _ := testRegistry(t)
	log := &bytes.Buffer{}
	srv, err := NewServer(ServerOptions{
		Addr:         "127.0.0.1:7778",
		Devices:      reg,
		Pairings:     reg,
		Reader:       newStubReader(),
		Writer:       newStubWriter(),
		Captures:     NewReservationStore(t.TempDir(), WithReservationClock(func() time.Time { return testNow })),
		Now:          func() time.Time { return testNow },
		PairingFloor: -1,
		Log:          log,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, reg, log
}

// postConfirm drives the route with no Authorization header at all, which is
// how a phone that has never paired reaches it.
func postConfirm(t *testing.T, srv *Server, c PairingConfirmation) *httptest.ResponseRecorder {
	t.Helper()
	return postConfirmBytes(t, srv, marshalConfirmation(t, c))
}

func postConfirmBytes(t *testing.T, srv *Server, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, request(http.MethodPost, RouteConfirm, "", bytes.NewReader(body)))
	return rec
}

func marshalConfirmation(t *testing.T, c PairingConfirmation) []byte {
	t.Helper()
	body, err := Marshal(&c)
	if err != nil {
		t.Fatalf("marshal pairing confirmation: %v", err)
	}
	return body
}

func decodeGrant(t *testing.T, rec *httptest.ResponseRecorder) PairingGrant {
	t.Helper()
	var g PairingGrant
	if err := Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatalf("the grant does not decode through the strict path: %v\n%s", err, rec.Body.String())
	}
	return g
}

// ---------------------------------------------------------------------------
// The happy path
// ---------------------------------------------------------------------------

// TestPairingConfirmMintsATokenThatAuthenticates is the whole point of the node:
// before it there was no production caller of Registry.Confirm, so no device
// could reach ACTIVE and the listener could only ever answer 401.
//
// It ends by using the token, because a grant that does not authenticate is a
// document, not a credential.
func TestPairingConfirmMintsATokenThatAuthenticates(t *testing.T) {
	srv, reg, log := confirmServer(t)
	c := pendingPairing(t, reg, "Adit iPhone")

	rec := postConfirm(t, srv, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	grant := decodeGrant(t, rec)
	if grant.DeviceID != c.DeviceID {
		t.Fatalf("grant names %s, the confirmation named %s", grant.DeviceID, c.DeviceID)
	}
	if grant.TokenFingerprint != Fingerprint(grant.Token) {
		t.Fatal("the grant's fingerprint does not cover its own token; a phone cannot check it against `mora companion list`")
	}
	// A grant is a credential in transit. A cache anywhere on the path holding
	// it is the same leak as writing it to a file.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store on a response carrying a bearer token", got)
	}

	// The device is ACTIVE and the token opens the authenticated routes.
	dev, err := reg.Authenticate(grant.Token)
	if err != nil {
		t.Fatalf("the minted token does not authenticate: %v", err)
	}
	if dev.State != DeviceActive {
		t.Fatalf("state after confirm = %s, want active", dev.State)
	}
	today := httptest.NewRecorder()
	srv.Handler().ServeHTTP(today, request(http.MethodGet, RouteToday, grant.Token, nil))
	if today.Code != http.StatusOK {
		t.Fatalf("today with the minted token = %d, want 200\n%s", today.Code, today.Body.String())
	}

	// The listener still logs nothing per request, and the one request that
	// carries secrets in BOTH directions is this one.
	for _, secret := range []string{grant.Token, c.PairingCode, c.DeviceID} {
		if strings.Contains(log.String(), secret) {
			t.Fatalf("the log holds a pairing secret or a device id:\n%s", log.String())
		}
	}
	if log.Len() != 0 {
		t.Fatalf("the pairing route wrote to the listener log:\n%s", log.String())
	}
}

// TestPairingConfirmStampsFirstSeenOnlyOnA2xx pins the MarkSeen rule: the stamp
// records that a device was SERVED, so a refusal must not write one.
func TestPairingConfirmStampsFirstSeenOnlyOnA2xx(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	c := pendingPairing(t, reg, "phone")

	// A refusal first. Nothing was served, so nothing may be stamped — and the
	// device is still pending, so a stamp here would also be a claim about a
	// device that has never authenticated.
	wrong := c
	wrong.PairingCode = "not-the-code"
	if rec := postConfirm(t, srv, wrong); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong code = %d, want 401", rec.Code)
	}
	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		if d.LastSeenAt != "" {
			t.Fatalf("%s was stamped last_seen_at by a refused confirmation", d.DeviceID)
		}
	}

	if rec := postConfirm(t, srv, c); rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d, want 200", rec.Code)
	}
	devices, err = reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].LastSeenAt == "" {
		t.Fatal("a served confirmation left no first-seen stamp")
	}
}

// TestPairingConfirmDoesNotStampWhenTheGrantIsRefusedOnTheWayOut is the other
// half of the 2xx rule, and it is the half a refusal test cannot reach.
//
// The refusals above return before markSeen is anywhere near the code. The case
// that actually distinguishes "stamp on 2xx" from "stamp once Confirm returned"
// is the one where Confirm SUCCEEDS and the outbound contract check then
// refuses the grant: the response is a 500, nothing was served, and a
// last_seen_at written there would be a durable claim that a device was
// answered when it was not.
func TestPairingConfirmDoesNotStampWhenTheGrantIsRefusedOnTheWayOut(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	c := pendingPairing(t, reg, "phone")
	spy := &markSeenSpy{Authenticator: reg}
	srv.devices = spy
	// An ACTIVE device with no token fingerprint fails Device.Validate, so the
	// grant built from it fails Marshal and writePayload answers 500.
	srv.confirms = &brokenConfirmer{token: "a-token", dev: Device{
		Header: newHeader(SchemaDevice), DeviceID: c.DeviceID, Label: "phone",
		Platform: PlatformIOS, State: DeviceActive, CreatedAt: "2026-09-03T12:00:00Z",
	}}

	rec := postConfirm(t, srv, c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a grant the contract refuses = %d, want 500\n%s", rec.Code, rec.Body.String())
	}
	if spy.calls != 0 {
		t.Fatalf("MarkSeen ran %d times on a response that served nothing", spy.calls)
	}

	// And the same listener stamps exactly once when the grant IS served, so the
	// assertion above is about the gate and not about markSeen being unreachable.
	srv.confirms = reg
	if got := postConfirm(t, srv, c); got.Code != http.StatusOK {
		t.Fatalf("confirm = %d, want 200\n%s", got.Code, got.Body.String())
	}
	if spy.calls != 1 {
		t.Fatalf("MarkSeen ran %d times for one served confirmation, want 1", spy.calls)
	}
}

// markSeenSpy counts the durable last-seen writes without changing what
// authentication does.
type markSeenSpy struct {
	Authenticator
	calls int
}

func (m *markSeenSpy) MarkSeen(deviceID string) error {
	m.calls++
	return m.Authenticator.MarkSeen(deviceID)
}

// brokenConfirmer returns a device the contract will refuse on the way out.
type brokenConfirmer struct {
	token string
	dev   Device
}

func (b *brokenConfirmer) Confirm(PairingConfirmation) (string, Device, error) {
	return b.token, b.dev, nil
}

func (b *brokenConfirmer) Revoke(string) (Device, bool, error) { return Device{}, false, nil }

// ---------------------------------------------------------------------------
// One refusal
// ---------------------------------------------------------------------------

// TestPairingConfirmAnswersEveryCredentialFailureIdentically is the oracle test.
//
// Wrong code, expired code, replayed confirmation, unknown device and a device
// that was revoked mid-pairing are five different things to an operator and must
// be ONE thing to a caller: the same status, the same headers and the same
// bytes. A caller that can tell them apart can enumerate device ids, learn
// whether a pairing window is open, and tell "your code is wrong" from "your
// code is late" — which is a map of where to spend the next guess.
func TestPairingConfirmAnswersEveryCredentialFailureIdentically(t *testing.T) {
	// The reference refusal: no device of this id has ever existed.
	srv, reg, _ := confirmServer(t)
	stranger := pendingPairing(t, reg, "reference")
	// Take the shape, then point it at a device id the registry never issued.
	stranger.DeviceID = "dev_20260903_120000_deadbeef"
	reference := postConfirm(t, srv, stranger)
	if reference.Code != http.StatusUnauthorized {
		t.Fatalf("an unknown device = %d, want 401\n%s", reference.Code, reference.Body.String())
	}

	for _, tc := range []struct {
		name string
		body func(t *testing.T, srv *Server, reg *Registry) PairingConfirmation
	}{
		{"a wrong code against a live pairing", func(t *testing.T, srv *Server, reg *Registry) PairingConfirmation {
			c := pendingPairing(t, reg, "phone")
			c.PairingCode = "wrong-code"
			return c
		}},
		{"no pairing window open at all", func(t *testing.T, srv *Server, reg *Registry) PairingConfirmation {
			// Nothing is paired. This is the case the task called out: the
			// answer must not be a 404, because a 404 says "no window", which
			// is exactly the fact a prober wants.
			c := pendingPairing(t, reg, "phone")
			if _, _, err := reg.Revoke(c.DeviceID); err != nil {
				t.Fatal(err)
			}
			return c
		}},
		{"a replayed confirmation", func(t *testing.T, srv *Server, reg *Registry) PairingConfirmation {
			c := pendingPairing(t, reg, "phone")
			if rec := postConfirm(t, srv, c); rec.Code != http.StatusOK {
				t.Fatalf("the first confirmation = %d, want 200", rec.Code)
			}
			return c
		}},
		{"an unknown device id", func(t *testing.T, srv *Server, reg *Registry) PairingConfirmation {
			c := pendingPairing(t, reg, "phone")
			c.DeviceID = "dev_20260903_120000_0badc0de"
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, reg, _ := confirmServer(t)
			rec := postConfirm(t, srv, tc.body(t, srv, reg))
			if rec.Code != reference.Code {
				t.Fatalf("status = %d, the reference refusal is %d", rec.Code, reference.Code)
			}
			if rec.Body.String() != reference.Body.String() {
				t.Fatalf("body =\n%s\nthe reference refusal is\n%s", rec.Body.String(), reference.Body.String())
			}
			// A WWW-Authenticate challenge's realm and error parameters are
			// exactly the discrimination this response exists not to give.
			if got := rec.Header().Get("WWW-Authenticate"); got != "" {
				t.Fatalf("the refusal carries a challenge header: %q", got)
			}
			for _, h := range []string{"Content-Type", "Cache-Control"} {
				if rec.Header().Get(h) != reference.Header().Get(h) {
					t.Fatalf("%s = %q, the reference refusal is %q", h, rec.Header().Get(h), reference.Header().Get(h))
				}
			}
		})
	}
}

// TestPairingConfirmExpiredCodeIsBurnedAndRefusedOpaquely covers the one refusal
// path that WRITES.
//
// A matching-but-late code is burned by Registry.Confirm — deliberately, so a
// clock rolled back cannot revive yesterday's photographed QR code — and the
// route must still answer the same opaque 401 as every other refusal.
func TestPairingConfirmExpiredCodeIsBurnedAndRefusedOpaquely(t *testing.T) {
	reg, clock, _, _ := testRegistry(t)
	log := &bytes.Buffer{}
	srv, err := NewServer(ServerOptions{
		Addr: "127.0.0.1:7778", Devices: reg, Pairings: reg,
		Reader: newStubReader(), Writer: newStubWriter(),
		Captures:     NewReservationStore(t.TempDir()),
		Now:          func() time.Time { return *clock },
		PairingFloor: -1,
		Log:          log,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	c := pendingPairing(t, reg, "phone")
	*clock = clock.Add(PairingTTL + time.Second)

	rec := postConfirm(t, srv, c)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an expired code = %d, want the opaque 401\n%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != unauthorizedBody {
		t.Fatalf("an expired code answers a different body:\n%s", rec.Body.String())
	}
	// The burn: rolling the clock BACK must not revive the code.
	*clock = clock.Add(-2 * PairingTTL)
	if rec := postConfirm(t, srv, c); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the code survived a clock rollback: %d", rec.Code)
	}
	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].State != DevicePending {
		t.Fatalf("state after an expired confirmation = %s, want pending (no credential was minted)", devices[0].State)
	}
	if devices[0].TokenFingerprint != "" {
		t.Fatal("an expired confirmation minted a credential")
	}
}

// TestPairingConfirmRefusesAMalformedBodyWithACodeAndNoValue pins the ONE
// refusal that is deliberately not the opaque 401.
//
// A malformed body is not a claim about a credential — the caller wrote it and
// already knows what is in it — so it gets the schema code, which is what makes
// a client implementable. It must still never echo a value back.
func TestPairingConfirmRefusesAMalformedBodyWithACodeAndNoValue(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	good := pendingPairing(t, reg, "phone")
	secret := good.PairingCode

	for _, tc := range []struct{ name, body string }{
		{"not JSON", "{"},
		{"an empty document", "{}"},
		// The zero-Header case: a body that omits the envelope must not inherit
		// it from a constructor.
		{"no envelope", fmt.Sprintf(`{"device_id":%q,"pairing_code":%q,"label":"p","platform":"ios","public_key":%q,"confirmed_at":"2026-09-03T12:00:00Z"}`, good.DeviceID, secret, testPublicKey)},
		{"an unknown field", strings.Replace(string(marshalConfirmation(t, good)), `"label"`, `"labels"`, 1)},
		{"a wrong schema name", strings.Replace(string(marshalConfirmation(t, good)), SchemaPairingOK, SchemaPairing, 1)},
		{"trailing data", string(marshalConfirmation(t, good)) + "}"},
		{"an unpublished platform", strings.Replace(string(marshalConfirmation(t, good)), `"ios"`, `"android"`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postConfirmBytes(t, srv, []byte(tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400\n%s", rec.Code, rec.Body.String())
			}
			// The refusal carries this package's own vocabulary and nothing the
			// caller sent. A pairing code echoed into an error body would be a
			// live secret in whatever logs that body.
			if strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Body.String(), good.DeviceID) {
				t.Fatalf("the rejection echoed the request:\n%s", rec.Body.String())
			}
			var doc map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("the rejection is not JSON: %v", err)
			}
			if _, ok := doc["error"].(string); !ok {
				t.Fatalf("the rejection carries no error code:\n%s", rec.Body.String())
			}
		})
	}

	// The good body still works afterwards: none of the above spent the code.
	if rec := postConfirm(t, srv, good); rec.Code != http.StatusOK {
		t.Fatalf("a malformed request spent the pairing code: %d", rec.Code)
	}
}

// TestPairingConfirmRefusesAnOversizeBodyBeforeDecoding proves the size bound is
// enforced on a route no credential guards, which is the one place an unbounded
// body costs the Mac memory for free.
func TestPairingConfirmRefusesAnOversizeBodyBeforeDecoding(t *testing.T) {
	srv, _, _ := confirmServer(t)
	rec := postConfirmBytes(t, srv, bytes.Repeat([]byte("a"), MaxRequestBytes+1))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversize body = %d, want 413\n%s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// The attempt budget
// ---------------------------------------------------------------------------

// TestPairingConfirmRevokesThePairingAfterTheAttemptBudget is the brute-force
// bound. The MaxPairingAttempts-th wrong code ends the pairing and writes a
// receipt; the code cannot be used afterwards even if it is guessed correctly.
func TestPairingConfirmRevokesThePairingAfterTheAttemptBudget(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	c := pendingPairing(t, reg, "phone")
	wrong := c
	wrong.PairingCode = "wrong-code"

	for i := 0; i < MaxPairingAttempts; i++ {
		if rec := postConfirm(t, srv, wrong); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].State != DeviceRevoked {
		t.Fatalf("state after %d wrong codes = %s, want revoked", MaxPairingAttempts, devices[0].State)
	}
	// The receipt is the operator's evidence that this happened.
	revocations := receiptFiles(t, reg.stateDir, "revoked")
	if len(revocations) != 1 {
		t.Fatalf("the lockout wrote %d revocation receipts, want 1", len(revocations))
	}
	if !strings.Contains(revocations[0], c.DeviceID) {
		t.Fatalf("the revocation receipt names the wrong device: %s", revocations[0])
	}
	// The RIGHT code now buys nothing.
	if rec := postConfirm(t, srv, c); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the correct code worked after the lockout: %d", rec.Code)
	}
}

// TestPairingLockoutCannotRevokeADeviceItDoesNotProtect is the counterpart, and
// it is the reason the counter is keyed the way it is.
//
// A device id is not a secret: `mora companion pair` prints it and the QR
// payload carries it. If any failed confirmation counted, anyone who could name
// a device id could spend MaxPairingAttempts requests and revoke a working
// phone. Only a wrong code against a LIVE pairing counts, so a confirmation
// aimed at an already-active device is refused without ever touching the budget.
func TestPairingLockoutCannotRevokeADeviceItDoesNotProtect(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	c := pendingPairing(t, reg, "phone")
	rec := postConfirm(t, srv, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d, want 200", rec.Code)
	}
	token := decodeGrant(t, rec).Token

	// Ten replays — twice the budget — against the now-ACTIVE device.
	for i := 0; i < MaxPairingAttempts*2; i++ {
		if got := postConfirm(t, srv, c); got.Code != http.StatusUnauthorized {
			t.Fatalf("replay %d = %d, want 401", i+1, got.Code)
		}
	}
	dev, err := reg.Authenticate(token)
	if err != nil {
		t.Fatalf("replaying a spent confirmation revoked a working device: %v", err)
	}
	if dev.State != DeviceActive {
		t.Fatalf("state after %d replays = %s, want active", MaxPairingAttempts*2, dev.State)
	}

	// And the same for a device id that was never issued: an unknown id must
	// not be able to fill the attempt table either.
	unknown := c
	for i := 0; i < MaxPairingAttempts*2; i++ {
		unknown.DeviceID = fmt.Sprintf("dev_20260903_120000_%08x", i)
		if got := postConfirm(t, srv, unknown); got.Code != http.StatusUnauthorized {
			t.Fatalf("unknown-device attempt %d = %d, want 401", i+1, got.Code)
		}
	}
	fresh := pendingPairing(t, reg, "second phone")
	if got := postConfirm(t, srv, fresh); got.Code != http.StatusOK {
		t.Fatalf("a new pairing was locked out by attempts against unknown ids: %d", got.Code)
	}
}

// TestPairingLimiterFailsClosedWhenItsTableIsFull drives the limiter directly.
//
// The table can only be grown by local pairings, so this state is not reachable
// from the network — but a rate limiter that silently stops limiting when it
// runs out of room is the failure mode worth pinning, so the unit is asserted
// rather than argued.
func TestPairingLimiterFailsClosedWhenItsTableIsFull(t *testing.T) {
	l := newPairingLimiter()
	for i := 0; i < maxTrackedPairings; i++ {
		l.fail(fmt.Sprintf("dev_%d", i))
	}
	if l.blocked("dev_0") {
		t.Fatal("one failure blocked a device; the budget is MaxPairingAttempts")
	}
	// The table is full. The next unknown device latches it closed.
	if l.fail("dev_overflow") {
		t.Fatal("an untracked device reported spending a budget it never had")
	}
	if !l.blocked("dev_0") || !l.blocked("dev_overflow") {
		t.Fatal("a full attempt table did not fail closed; the lockout is no longer enforceable")
	}
}

// TestPairingConfirmBudgetIsCheckedBeforeTheRegistry proves a locked-out caller
// costs nothing durable. Once the budget is spent, further attempts must not
// reach Confirm at all — otherwise a locked-out id is a free way to take the
// registry's cross-process write lock, once per request.
func TestPairingConfirmBudgetIsCheckedBeforeTheRegistry(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	c := pendingPairing(t, reg, "phone")
	counting := &countingConfirmer{inner: reg}
	srv.confirms = counting

	wrong := c
	wrong.PairingCode = "wrong-code"
	for i := 0; i < MaxPairingAttempts; i++ {
		postConfirm(t, srv, wrong)
	}
	spent := counting.confirms
	if spent != MaxPairingAttempts {
		t.Fatalf("the budget allowed %d confirmations, want %d", spent, MaxPairingAttempts)
	}
	for i := 0; i < 5; i++ {
		postConfirm(t, srv, wrong)
	}
	if counting.confirms != spent {
		t.Fatalf("a locked-out device reached the registry %d more times", counting.confirms-spent)
	}
}

type countingConfirmer struct {
	inner    Confirmer
	confirms int
}

func (c *countingConfirmer) Confirm(p PairingConfirmation) (string, Device, error) {
	c.confirms++
	return c.inner.Confirm(p)
}

func (c *countingConfirmer) Revoke(id string) (Device, bool, error) { return c.inner.Revoke(id) }

// ---------------------------------------------------------------------------
// The slot and the floor
// ---------------------------------------------------------------------------

// TestPairingConfirmServesOneAtATime proves the route's own single slot. It is
// separate from the kernel's budget on purpose: a confirmation takes the
// registry write lock, and neither side should be able to shut the other out.
func TestPairingConfirmServesOneAtATime(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	c := pendingPairing(t, reg, "phone")

	// Hold the slot from inside a stub confirmer, then drive a second request.
	held := make(chan struct{})
	entered := make(chan struct{})
	srv.confirms = &blockingConfirmer{inner: reg, entered: entered, hold: held}

	var first *httptest.ResponseRecorder
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		first = postConfirm(t, srv, c)
	}()
	<-entered

	second := postConfirm(t, srv, c)
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("the second concurrent confirmation = %d, want 503\n%s", second.Code, second.Body.String())
	}
	if got := second.Header().Get("Retry-After"); got == "" {
		t.Fatal("the busy refusal carries no Retry-After")
	}
	close(held)
	wg.Wait()
	if first.Code != http.StatusOK {
		t.Fatalf("the held confirmation = %d, want 200\n%s", first.Code, first.Body.String())
	}

	// And a read route was never blocked by any of it.
	today := httptest.NewRecorder()
	srv.Handler().ServeHTTP(today, request(http.MethodGet, RouteToday, decodeGrant(t, first).Token, nil))
	if today.Code != http.StatusOK {
		t.Fatalf("today = %d after a pairing held the pairing slot", today.Code)
	}
}

type blockingConfirmer struct {
	inner   Confirmer
	entered chan struct{}
	hold    chan struct{}
}

func (b *blockingConfirmer) Confirm(p PairingConfirmation) (string, Device, error) {
	b.entered <- struct{}{}
	<-b.hold
	return b.inner.Confirm(p)
}

func (b *blockingConfirmer) Revoke(id string) (Device, bool, error) { return b.inner.Revoke(id) }

// TestPairingConfirmPadsEveryAnswerToTheFloor is the timing test.
//
// The three outcomes cost wildly different amounts of work — an unknown device
// returns before any comparison, a wrong code runs the constant-time compare,
// and a success writes the record file and a receipt — and the floor is what
// stops that difference from being readable with a stopwatch. It is asserted as
// a MINIMUM, which is what a floor is: the test proves the padding happens, not
// that the paths are identical, because they are not and this file does not
// claim they are.
func TestPairingConfirmPadsEveryAnswerToTheFloor(t *testing.T) {
	const floor = 120 * time.Millisecond
	reg, _, _, _ := testRegistry(t)
	srv, err := NewServer(ServerOptions{
		Addr: "127.0.0.1:7778", Devices: reg, Pairings: reg,
		Reader: newStubReader(), Writer: newStubWriter(),
		Captures:     NewReservationStore(t.TempDir()),
		Now:          func() time.Time { return testNow },
		PairingFloor: floor,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	c := pendingPairing(t, reg, "phone")
	unknown := c
	unknown.DeviceID = "dev_20260903_120000_deadbeef"
	wrong := c
	wrong.PairingCode = "wrong-code"

	for _, tc := range []struct {
		name string
		body PairingConfirmation
		want int
	}{
		// The cheapest refusal in Registry.Confirm: it returns before any
		// comparison runs.
		{"an unknown device", unknown, http.StatusUnauthorized},
		{"a wrong code", wrong, http.StatusUnauthorized},
		// The expensive path: it writes the record file and a receipt.
		{"a successful confirmation", c, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			rec := postConfirm(t, srv, tc.body)
			elapsed := time.Since(start)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d\n%s", rec.Code, tc.want, rec.Body.String())
			}
			if elapsed < floor {
				t.Fatalf("answered in %v, under the %v floor — the cheap and expensive paths are separable", elapsed, floor)
			}
		})
	}
}

// TestPairingFloorDefaultsToTheConstant keeps the production default honest: the
// tests above all switch the floor off or shorten it, so nothing else in this
// package would notice if the default became zero.
func TestPairingFloorDefaultsToTheConstant(t *testing.T) {
	reg, _, _, _ := testRegistry(t)
	srv, err := NewServer(ServerOptions{
		Addr: "127.0.0.1:7778", Devices: reg, Pairings: reg,
		Reader: newStubReader(), Writer: newStubWriter(),
		Captures: NewReservationStore(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.pairingMinimum != PairingFloor {
		t.Fatalf("a listener built without a PairingFloor has %v, want the %v default", srv.pairingMinimum, PairingFloor)
	}
}

// ---------------------------------------------------------------------------
// The exemption
// ---------------------------------------------------------------------------

// TestExactlyOneRouteIsUnauthenticated is the exemption witness.
//
// The auth guard reads its exemption out of routeDefs, so this walks the same
// table and drives the real handler: every other route with no credential must
// still be a 401, and the one public route must not be.
func TestExactlyOneRouteIsUnauthenticated(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	handler := srv.Handler()

	public := 0
	for _, route := range srv.Routes() {
		if route.Public {
			public++
			if route.Pattern != RouteConfirm {
				t.Fatalf("%s %s is public; only %s may be", route.Method, route.Pattern, RouteConfirm)
			}
			continue
		}
		// No Authorization header at all.
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request(route.Method, route.Pattern, "", strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a token = %d, want 401\n%s", route.Method, route.Pattern, rec.Code, rec.Body.String())
		}
	}
	if public != 1 {
		t.Fatalf("the allowlist declares %d unauthenticated routes, want exactly 1", public)
	}

	// The public route is reached with no credential, and a credential does not
	// change what it does: a phone retrying a confirmation while holding a stale
	// token must get the same answer as one holding none.
	c := pendingPairing(t, reg, "phone")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request(http.MethodPost, RouteConfirm, "a-token-that-is-not-a-device-token", bytes.NewReader(marshalConfirmation(t, c))))
	if rec.Code != http.StatusOK {
		t.Fatalf("the pairing route refused a caller carrying a junk token: %d\n%s", rec.Code, rec.Body.String())
	}
}

// TestTheAuthExemptionAgreesWithTheMuxOnEncodedPaths pins the one place where
// "which path is this?" is answered twice — once by the auth guard's exemption
// map and once by ServeMux — and proves the two cannot disagree in the
// direction that would matter.
//
// The dangerous shape is a request the mux routes to a PROTECTED handler while
// the guard has already called it public. It cannot happen: a mux match means
// the unescaped segments equal the pattern, and r.URL.Path is exactly that
// unescaped form. This drives the encodings that make the question non-obvious
// so a future change to either side shows up here rather than as a route served
// without a credential.
func TestTheAuthExemptionAgreesWithTheMuxOnEncodedPaths(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	handler := srv.Handler()

	// An encoded spelling of a PROTECTED route is never exempted.
	for _, path := range []string{
		"/v1/companion/%74oday",
		"/v1/companion/%68ealth",
		"/v1/companion/%63ontext",
		"/v1/companion/%63aptures",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request(http.MethodGet, path, "", strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a token = %d, want 401 — an encoded protected route escaped the guard\n%s",
				path, rec.Code, rec.Body.String())
		}
	}

	// %2F is one segment, so the mux mounts nothing there. The exemption is
	// granted on the unescaped path and then no handler runs, which is why the
	// asymmetry is harmless: a 404, never a served route.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request(http.MethodPost, "/v1/companion/pairing%2Fconfirm", "",
		bytes.NewReader(marshalConfirmation(t, pendingPairing(t, reg, "encoded")))))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("an encoded separator = %d, want 404\n%s", rec.Code, rec.Body.String())
	}

	// And an alternate spelling the MUX considers the same route is served as
	// that route. Go decides this, not the exemption map; the point of pinning
	// it is that the two agree.
	c := pendingPairing(t, reg, "alternate spelling")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request(http.MethodPost, "/v1/companion/pairing/%63onfirm", "",
		bytes.NewReader(marshalConfirmation(t, c))))
	if rec.Code != http.StatusOK {
		t.Fatalf("an alternate spelling of the public route = %d, want 200 — the guard and the mux disagree\n%s",
			rec.Code, rec.Body.String())
	}
}

// TestPairingRouteIsStillBehindTheHostGuard proves the exemption is from the
// CREDENTIAL check and nothing else. The DNS-rebinding guard runs first and does
// not care that the route is public: a browser on the phone's network pointed at
// a name that resolves to loopback must not be able to spend a pairing code.
func TestPairingRouteIsStillBehindTheHostGuard(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	c := pendingPairing(t, reg, "phone")

	r := request(http.MethodPost, RouteConfirm, "", bytes.NewReader(marshalConfirmation(t, c)))
	r.Host = "companion.example"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a rebound Host reached the pairing route: %d\n%s", rec.Code, rec.Body.String())
	}
	// And the code was not spent.
	if got := postConfirm(t, srv, c); got.Code != http.StatusOK {
		t.Fatalf("the refused request spent the pairing code: %d", got.Code)
	}
}

// TestNewServerRefusesAListenerWithNoConfirmer pins the assembly rule: the
// allowlist declares the route unconditionally, so a listener that cannot serve
// it must not start. A conditional route table would make the security boundary
// depend on how the caller wired it.
func TestNewServerRefusesAListenerWithNoConfirmer(t *testing.T) {
	reg, _, _, _ := testRegistry(t)
	_, err := NewServer(ServerOptions{
		Addr: "127.0.0.1:7778", Devices: reg,
		Reader: newStubReader(), Writer: newStubWriter(),
		Captures: NewReservationStore(t.TempDir()),
	})
	if err == nil {
		t.Fatal("NewServer built a listener that declares a route it cannot serve")
	}
	if !strings.Contains(err.Error(), "pairing confirmer") {
		t.Fatalf("the refusal does not name what is missing: %v", err)
	}
}
