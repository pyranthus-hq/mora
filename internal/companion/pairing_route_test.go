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
	"errors"
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
	srv.confirms = &brokenConfirmer{Confirmer: reg, token: "a-token", dev: Device{
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
//
// It embeds Confirmer so the budget half of the seam comes from the real
// registry: a stub that answered the budget itself would be proving a property
// of the stub.
type brokenConfirmer struct {
	Confirmer
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

// TestPairingConfirmRefusesAnExpiredCodeWhoseReceiptAlsoFailed is the
// receipt-warning boundary, and it is a refusal that used to escape it.
//
// Registry.Confirm BURNS a matching-but-late code — deliberately, so a clock
// rolled back cannot revive yesterday's photographed QR code — and that burn is
// a durable write with a receipt. When the receipt fails, Confirm reports
// errors.Join(ErrPairingExpired, ErrReceiptNotWritten): a refusal with a warning
// attached.
//
// The handler used to tolerate ANY receipt warning as success. On this path that
// meant building a grant out of an empty token, failing the outbound contract
// check, and answering 500 — a refusal an attacker can tell apart from every
// other one by status code alone, and a 500 that blames the Mac rather than the
// code. A receipt warning is tolerated ONLY alongside a credential that was
// actually minted.
//
// The registry here is real and its audit writer is failed at the seam the
// ordering rules exist for, so the joined error is the one production produces
// rather than one a stub invented.
func TestPairingConfirmRefusesAnExpiredCodeWhoseReceiptAlsoFailed(t *testing.T) {
	reg, clock, _, _ := testRegistry(t)
	srv, err := NewServer(ServerOptions{
		Addr: "127.0.0.1:7778", Devices: reg, Pairings: reg,
		Reader: newStubReader(), Writer: newStubWriter(),
		Captures:     NewReservationStore(t.TempDir()),
		Now:          func() time.Time { return *clock },
		PairingFloor: -1,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Both pairings are registered BEFORE the audit writer is failed, so the
	// failure the test injects is the one on the confirmation path and not one
	// on `pair`. The second exists because the control below burns the first.
	c := pendingPairing(t, reg, "phone")
	second := pendingPairing(t, reg, "second phone")
	*clock = clock.Add(PairingTTL + time.Second)
	reg.writeAudit = func(receipt, func() error) error {
		return errors.New("simulated audit failure after commit")
	}

	// The joined error really is what Confirm produces on this path; if it ever
	// stops being, this test is measuring nothing and says so.
	if _, _, cerr := reg.Confirm(c); !errors.Is(cerr, ErrPairingExpired) || !errors.Is(cerr, ErrReceiptNotWritten) {
		t.Fatalf("Confirm returned %v; this test needs the joined expired+unaudited error", cerr)
	}

	rec := postConfirm(t, srv, second)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an expired code whose receipt failed = %d, want the opaque 401\n%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != unauthorizedBody {
		t.Fatalf("it answers a body no other refusal answers:\n%s", rec.Body.String())
	}
	// No grant, and above all no token: the body must not be a document at all.
	if strings.Contains(rec.Body.String(), SchemaPairingGrant) || strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("a refusal carried grant material:\n%s", rec.Body.String())
	}
	// And nothing was issued behind the scenes either.
	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		if d.State == DeviceActive || d.TokenFingerprint != "" {
			t.Fatalf("%s was activated by a refused confirmation (%s, %s)", d.DeviceID, d.State, d.TokenFingerprint)
		}
	}
}

// TestPairingConfirmStillIssuesWhenOnlyTheSuccessReceiptFailed is the other side
// of the same boundary, and it is why the tolerance exists at all.
//
// On the SUCCESS path the device is active and the returned token is the only
// copy of its credential. Throwing it away because an audit row failed would
// strand exactly the credential nobody holds that the record-first ordering
// exists to prevent, so the grant is served and the warning is the registry's to
// surface.
func TestPairingConfirmStillIssuesWhenOnlyTheSuccessReceiptFailed(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	c := pendingPairing(t, reg, "phone")
	reg.writeAudit = func(receipt, func() error) error {
		return errors.New("simulated audit failure after commit")
	}

	rec := postConfirm(t, srv, c)
	if rec.Code != http.StatusOK {
		t.Fatalf("a successful confirmation whose receipt failed = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	grant := decodeGrant(t, rec)
	if _, err := reg.Authenticate(grant.Token); err != nil {
		t.Fatalf("the issued token does not authenticate: %v", err)
	}
}

// TestPairingGrantFingerprintMustCoverItsToken pins the outbound contract check.
//
// The fingerprint's whole job is to be compared: a person holds the phone next
// to the Mac and checks the value the phone stored against `mora companion
// list`. A grant carrying a well-formed digest of some OTHER string passes every
// format check and sends that person to compare a value that describes nothing
// they hold, so the two halves have to be checked against each other through the
// same derivation N11 stores.
func TestPairingGrantFingerprintMustCoverItsToken(t *testing.T) {
	good := PairingGrantFixture()
	if err := good.Validate(); err != nil {
		t.Fatalf("the shipped fixture must validate: %v", err)
	}

	for _, tc := range []struct {
		name        string
		fingerprint string
	}{
		// A digest of a DIFFERENT token: well-formed, sha256, lowercase hex, and
		// wrong. This is the shape a format-only check accepts.
		{"a digest of another token", Fingerprint("some-other-token")},
		{"a digest of the empty string", Fingerprint("")},
		{"all zeroes", "sha256:" + strings.Repeat("0", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := *PairingGrantFixture()
			g.TokenFingerprint = tc.fingerprint
			err := g.Validate()
			if err == nil {
				t.Fatal("a grant whose fingerprint does not cover its token validated")
			}
			var schemaErr *Error
			if !errors.As(err, &schemaErr) || schemaErr.Field != "token_fingerprint" {
				t.Fatalf("the refusal is not about token_fingerprint: %v", err)
			}
			// And it cannot be marshalled, which is what makes this a wire
			// guarantee rather than a convention: Marshal validates.
			if _, err := Marshal(&g); err == nil {
				t.Fatal("a grant with a mismatched fingerprint marshalled")
			}
		})
	}

	// The live handler's grant satisfies it, so the check is not merely
	// unreachable in production.
	srv, reg, _ := confirmServer(t)
	rec := postConfirm(t, srv, pendingPairing(t, reg, "phone"))
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d, want 200", rec.Code)
	}
	grant := decodeGrant(t, rec)
	if grant.TokenFingerprint != Fingerprint(grant.Token) {
		t.Fatal("the served grant's fingerprint does not cover its own token")
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

// TestPairingBudgetSurvivesARestart is the durability rule.
//
// The budget used to be a map in the listener's memory. That is not a budget: an
// attacker who can make the process exit — or who simply waits for the operator
// to restart it — gets a fresh MaxPairingAttempts every time, and five guesses
// per restart against a code is a very different thing from five guesses, full
// stop.
//
// The restart is simulated the only honest way: a SECOND registry value and a
// SECOND listener over the same directories, sharing nothing but the files. If
// the count lived in memory the new listener would start at zero.
func TestPairingBudgetSurvivesARestart(t *testing.T) {
	reg, _, configDir, stateDir := testRegistry(t)
	first := listenerOver(t, reg)
	c := pendingPairing(t, reg, "phone")
	wrong := c
	wrong.PairingCode = "wrong-code"

	// Four wrong codes, then the process "restarts" before the fifth.
	for i := 0; i < MaxPairingAttempts-1; i++ {
		if rec := postConfirm(t, first, wrong); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	if n, _, err := reg.PairingBudget(c.DeviceID); err != nil || n != MaxPairingAttempts-1 {
		t.Fatalf("the record holds %d attempts (err %v), want %d", n, err, MaxPairingAttempts-1)
	}

	restarted := NewRegistry(configDir, stateDir, WithClock(func() time.Time { return testNow }))
	second := listenerOver(t, restarted)

	if rec := postConfirm(t, second, wrong); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the fifth attempt after a restart = %d, want 401", rec.Code)
	}
	devices, err := restarted.List()
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].State != DeviceRevoked {
		t.Fatalf("state after five attempts across a restart = %s, want revoked", devices[0].State)
	}
	// The sixth attempt, with the RIGHT code, buys nothing.
	if rec := postConfirm(t, second, c); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the correct code worked after the budget was spent across a restart: %d", rec.Code)
	}
}

// listenerOver builds a listener over an existing registry. It is what makes
// "restart" mean something above: a second Server and a second Registry over the
// same files, sharing nothing else.
func listenerOver(t *testing.T, reg *Registry) *Server {
	t.Helper()
	srv, err := NewServer(ServerOptions{
		Addr: "127.0.0.1:7778", Devices: reg, Pairings: reg,
		Reader: newStubReader(), Writer: newStubWriter(),
		Captures:     NewReservationStore(t.TempDir(), WithReservationClock(func() time.Time { return testNow })),
		Now:          func() time.Time { return testNow },
		PairingFloor: -1,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// TestPairingCounterWriteFailureExhaustsThePairing is the fail-closed rule for
// the budget's WRITE.
//
// A durable budget is only durable while it can be written. This file used to
// discard a failed counter write: the count did not move, and the next guess
// reached Confirm again — so an attacker who could make that write fail, or who
// was simply unlucky with the registry lock, had no budget at all. The budget
// looked durable and was not, and no test covered it.
//
// The rule: an unwritable failure makes the pairing exhausted for this process
// until the write lands, the write is retried on the next attempt, and that
// attempt is refused too.
func TestPairingCounterWriteFailureExhaustsThePairing(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	c := pendingPairing(t, reg, "phone")
	wrong := c
	wrong.PairingCode = "wrong-code"

	// Two write failures: the one on the guess itself, and the one on the first
	// retry. The second is what makes this a test of the RETRY rather than of a
	// single unlucky write.
	counter := &failingCounter{Confirmer: reg, failures: 2}
	srv.confirms = counter

	// The first guess is tried, is wrong, and its counter write FAILS.
	if rec := postConfirm(t, srv, wrong); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the first wrong code = %d, want 401", rec.Code)
	}
	if counter.confirms != 1 {
		t.Fatalf("Confirm ran %d times on the first attempt, want 1", counter.confirms)
	}
	// The count really did not move — this is the state the latch exists for.
	if n, _, err := reg.PairingBudget(c.DeviceID); err != nil || n != 0 {
		t.Fatalf("the record holds %d attempts (err %v); the write was supposed to fail", n, err)
	}

	// The next attempt must NOT reach Confirm. This is the whole property: the
	// guess it carries is never tried, so an unwritable budget cannot be
	// out-guessed.
	confirmsBefore := counter.confirms
	if rec := postConfirm(t, srv, c); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an attempt against an unrecordable budget = %d, want 401", rec.Code)
	}
	if counter.confirms != confirmsBefore {
		t.Fatalf("Confirm was reached %d times while the budget was unrecordable", counter.confirms-confirmsBefore)
	}
	// It carried the RIGHT code and was still refused, which is what "fail
	// closed" means here.
	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].State != DevicePending {
		t.Fatalf("state = %s, want pending — the right code was served while the budget was unrecordable", devices[0].State)
	}
	// That attempt retried the write and it failed again, so the count is still
	// where it was and the latch still holds.
	if n, _, err := reg.PairingBudget(c.DeviceID); err != nil || n != 0 {
		t.Fatalf("the record holds %d attempts (err %v) after a failed retry, want 0", n, err)
	}

	// The writer heals. The retry lands on the next attempt, which is STILL
	// refused, and the owed failure is now on the record: the count is the
	// number of guesses actually made, not the number of writes that happened
	// to succeed.
	counter.failures = 0
	confirmsBefore = counter.confirms
	if rec := postConfirm(t, srv, c); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the flushing attempt = %d, want 401", rec.Code)
	}
	if counter.confirms != confirmsBefore {
		t.Fatalf("the flushing attempt reached Confirm %d times", counter.confirms-confirmsBefore)
	}
	if n, _, err := reg.PairingBudget(c.DeviceID); err != nil || n != 1 {
		t.Fatalf("after the retry the record holds %d attempts (err %v), want 1", n, err)
	}

	// And with nothing owed, the route serves again: the latch is a hold, not a
	// permanent lockout, so a transient write failure does not brick a pairing.
	if rec := postConfirm(t, srv, c); rec.Code != http.StatusOK {
		t.Fatalf("a settled latch did not release: %d", rec.Code)
	}
}

// TestPairingCounterWriteFailureStillEndsAtTheBudget proves the drained retries
// land the pairing in the same place a healthy run would.
//
// Five guesses are five guesses whether or not their writes succeeded at the
// time. If the flush wrote once and forgot the rest, a run of failures would
// leave the record under the budget forever and the pairing would never be
// revoked.
func TestPairingCounterWriteFailureStillEndsAtTheBudget(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	c := pendingPairing(t, reg, "phone")
	wrong := c
	wrong.PairingCode = "wrong-code"

	// Every write fails while the guesses are made. Only the FIRST guess
	// reaches Confirm; after that the latch refuses without trying, so the
	// owed count is one — the rest were never tried.
	counter := &failingCounter{Confirmer: reg, failures: 1}
	srv.confirms = counter
	for i := 0; i < MaxPairingAttempts; i++ {
		if rec := postConfirm(t, srv, wrong); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, rec.Code)
		}
	}

	// Heal the writer, then drive EXACTLY the guesses the budget still has left
	// if the owed one counted. One attempt retries the owed write and is
	// refused; MaxPairingAttempts-1 more are tried and counted. If the owed
	// failure were lost, the count would end one short and the pairing would
	// still be live — which is the slack this loop is sized to remove.
	counter.failures = 0
	for i := 0; i < MaxPairingAttempts; i++ {
		if rec := postConfirm(t, srv, wrong); rec.Code != http.StatusUnauthorized {
			t.Fatalf("post-heal attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].State != DeviceRevoked {
		t.Fatalf("state after the budget was spent = %s, want revoked", devices[0].State)
	}
	if rec := postConfirm(t, srv, c); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the right code worked after the budget was spent: %d", rec.Code)
	}
}

// failingCounter fails RecordPairingFailure a fixed number of times, and counts
// how often Confirm was reached — which is the observation the fail-closed rule
// is about.
type failingCounter struct {
	Confirmer
	failures int
	confirms int
}

func (f *failingCounter) Confirm(p PairingConfirmation) (string, Device, error) {
	f.confirms++
	return f.Confirmer.Confirm(p)
}

func (f *failingCounter) RecordPairingFailure(id string) (int, error) {
	if f.failures > 0 {
		f.failures--
		return 0, errors.New("companion: simulated counter write failure")
	}
	return f.Confirmer.RecordPairingFailure(id)
}

// TestRecordPairingFailureOnlyCountsALivePairing drives the registry guard
// DIRECTLY, and it is here because the handler's own guard hides it.
//
// The handler only records a failure for ErrPairingCode, which a settled device
// can never produce — so a route-level test proves the two guards TOGETHER hold
// and cannot say which one is doing the work. Remove the registry's and every
// route test still passes. It is the second line of defense on the property that
// matters most here (a device id is not a secret, so a counter that can grow
// against an ACTIVE device is a way to have the listener revoke a working
// phone), and a second line nothing tests is not a line.
func TestRecordPairingFailureOnlyCountsALivePairing(t *testing.T) {
	reg, _, _, _ := testRegistry(t)

	// An ACTIVE device: nothing to brute force, so nothing to count.
	_, active := pairAndConfirm(t, reg, "paired phone")
	for i := 0; i < MaxPairingAttempts*2; i++ {
		if n, err := reg.RecordPairingFailure(active.DeviceID); err != nil || n != 0 {
			t.Fatalf("an active device counted a failure: n=%d err=%v", n, err)
		}
	}
	if n, pending, err := reg.PairingBudget(active.DeviceID); err != nil || n != 0 || pending {
		t.Fatalf("active device budget = (%d, %v, %v), want (0, false, nil)", n, pending, err)
	}

	// A REVOKED device: same.
	revoked := pendingPairing(t, reg, "revoked phone")
	if _, _, err := reg.Revoke(revoked.DeviceID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxPairingAttempts*2; i++ {
		if n, err := reg.RecordPairingFailure(revoked.DeviceID); err != nil || n != 0 {
			t.Fatalf("a revoked device counted a failure: n=%d err=%v", n, err)
		}
	}

	// A device that never existed.
	if n, err := reg.RecordPairingFailure("dev_20260904_000000_deadbeef"); err != nil || n != 0 {
		t.Fatalf("an unknown device counted a failure: n=%d err=%v", n, err)
	}

	// A PENDING device with a live code is the ONE case that counts, so the
	// test above is not passing because the function never counts anything.
	live := pendingPairing(t, reg, "pairing phone")
	for i := 1; i <= MaxPairingAttempts; i++ {
		n, err := reg.RecordPairingFailure(live.DeviceID)
		if err != nil || n != i {
			t.Fatalf("failure %d recorded as n=%d err=%v", i, n, err)
		}
	}
	n, pending, err := reg.PairingBudget(live.DeviceID)
	if err != nil || n != MaxPairingAttempts || !pending {
		t.Fatalf("live budget = (%d, %v, %v), want (%d, true, nil)", n, pending, err, MaxPairingAttempts)
	}

	// And a pending device whose code has been burned is no longer live, so it
	// stops counting: there is nothing left to guess at.
	if _, _, err := reg.Revoke(live.DeviceID); err != nil {
		t.Fatal(err)
	}
	if n, err := reg.RecordPairingFailure(live.DeviceID); err != nil || n != 0 {
		t.Fatalf("a settled pairing kept counting: n=%d err=%v", n, err)
	}
}

// TestPairingRevokeFailureStillRefusesAndIsRetried is the "the revoke result is
// CHECKED" rule.
//
// The budget's whole meaning is the revocation it ends in. A revoke whose result
// is discarded leaves a pairing that has spent its budget still LIVE while the
// listener believes it dealt with it — and a DURABLE count then makes that state
// permanent rather than self-healing. So a failure must fail CLOSED (the attempt
// is still refused, even with the right code) and the revocation must be retried
// on the next attempt until it lands, with the receipt written when it does.
func TestPairingRevokeFailureStillRefusesAndIsRetried(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	c := pendingPairing(t, reg, "phone")
	// Fail the first two revocations: the one the budget triggers, and the
	// first retry.
	failing := &failingRevoker{Confirmer: reg, failures: 2}
	srv.confirms = failing

	wrong := c
	wrong.PairingCode = "wrong-code"
	for i := 0; i < MaxPairingAttempts; i++ {
		if rec := postConfirm(t, srv, wrong); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	// The revocation failed, so the record is still pending — and the RIGHT
	// code must still be refused. This is the fail-closed half.
	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].State != DevicePending {
		t.Fatalf("state after a failed revocation = %s, want pending (the revoke really did fail)", devices[0].State)
	}
	if rec := postConfirm(t, srv, c); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the correct code was served while the budget was spent: %d", rec.Code)
	}
	if got := receiptFiles(t, reg.stateDir, "revoked"); len(got) != 0 {
		t.Fatalf("a failed revocation wrote %d revocation receipts", len(got))
	}

	// The retry half. The next attempt goes through the same branch, and this
	// time the revocation lands.
	if rec := postConfirm(t, srv, c); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the retry attempt = %d, want 401", rec.Code)
	}
	devices, err = reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].State != DeviceRevoked {
		t.Fatalf("state after the retried revocation = %s, want revoked", devices[0].State)
	}
	revocations := receiptFiles(t, reg.stateDir, "revoked")
	if len(revocations) != 1 {
		t.Fatalf("the retried revocation wrote %d receipts, want 1", len(revocations))
	}
	if !strings.Contains(revocations[0], c.DeviceID) {
		t.Fatalf("the revocation receipt names the wrong device: %s", revocations[0])
	}
	if failing.attempts < 3 {
		t.Fatalf("Revoke was called %d times; the failures were not retried", failing.attempts)
	}
}

// TestPairingConfirmFailsClosedWhenTheBudgetCannotBeRead is the other direction:
// a budget that cannot be READ is a budget that cannot be enforced, and a route
// that cannot enforce its own rate limit must not keep answering guesses.
func TestPairingConfirmFailsClosedWhenTheBudgetCannotBeRead(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	c := pendingPairing(t, reg, "phone")
	srv.confirms = &unreadableBudget{Confirmer: reg}

	rec := postConfirm(t, srv, c)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unreadable budget = %d, want the opaque 401\n%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != unauthorizedBody {
		t.Fatalf("an unreadable budget answers a different body:\n%s", rec.Body.String())
	}
	// And the code was NOT spent, so the pairing survives the outage.
	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].State != DevicePending {
		t.Fatalf("state = %s, want pending — a read failure must not settle a pairing", devices[0].State)
	}
}

type unreadableBudget struct{ Confirmer }

func (u *unreadableBudget) PairingBudget(string) (int, bool, error) {
	return 0, false, errors.New("companion: simulated registry read failure")
}

// TestPairingConfirmSpendsNoWriteLockOnALockedOutCaller proves a caller whose
// budget is gone costs a bounded READ and nothing more.
//
// It matters because this route is unauthenticated: if a spent budget still took
// the registry's cross-process write lock once per request, a stranger on the
// network could stall `mora companion pair` and `mora companion revoke` on the
// Mac just by repeating a guess.
func TestPairingConfirmSpendsNoWriteLockOnALockedOutCaller(t *testing.T) {
	srv, reg, _ := confirmServer(t)
	c := pendingPairing(t, reg, "phone")
	counting := &countingConfirmer{Confirmer: reg}
	srv.confirms = counting

	wrong := c
	wrong.PairingCode = "wrong-code"
	for i := 0; i < MaxPairingAttempts; i++ {
		postConfirm(t, srv, wrong)
	}
	confirms, revokes := counting.confirms, counting.revokes
	if confirms != MaxPairingAttempts {
		t.Fatalf("the budget allowed %d confirmations, want %d", confirms, MaxPairingAttempts)
	}

	for i := 0; i < 5; i++ {
		postConfirm(t, srv, wrong)
	}
	if counting.confirms != confirms {
		t.Fatalf("a locked-out device reached Confirm %d more times", counting.confirms-confirms)
	}
	// The revocation landed on the fifth attempt, so the record is no longer
	// pending and the retry branch does not fire again either.
	if counting.revokes != revokes {
		t.Fatalf("a settled pairing took the write lock %d more times", counting.revokes-revokes)
	}
	if counting.budgets == 0 {
		t.Fatal("the durable budget was never read")
	}
}

// countingConfirmer counts the calls that take the registry's WRITE lock.
type countingConfirmer struct {
	Confirmer
	confirms int
	revokes  int
	budgets  int
}

func (c *countingConfirmer) PairingBudget(id string) (int, bool, error) {
	c.budgets++
	return c.Confirmer.PairingBudget(id)
}

func (c *countingConfirmer) Confirm(p PairingConfirmation) (string, Device, error) {
	c.confirms++
	return c.Confirmer.Confirm(p)
}

func (c *countingConfirmer) Revoke(id string) (Device, bool, error) {
	c.revokes++
	return c.Confirmer.Revoke(id)
}

// failingRevoker is a registry whose Revoke fails a fixed number of times
// before working. It is how the "the revoke result is CHECKED" rule is driven:
// a discarded result leaves a pairing that spent its budget still live.
type failingRevoker struct {
	Confirmer
	failures int
	attempts int
}

func (f *failingRevoker) Revoke(id string) (Device, bool, error) {
	f.attempts++
	if f.attempts <= f.failures {
		return Device{}, false, errors.New("companion: simulated revoke failure")
	}
	return f.Confirmer.Revoke(id)
}

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
	srv.confirms = &blockingConfirmer{Confirmer: reg, entered: entered, hold: held}

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
	Confirmer
	entered chan struct{}
	hold    chan struct{}
}

func (b *blockingConfirmer) Confirm(p PairingConfirmation) (string, Device, error) {
	b.entered <- struct{}{}
	<-b.hold
	return b.Confirmer.Confirm(p)
}

// TestPairingFloorPadsToBucketBoundaries pins the quantizer's arithmetic.
//
// It is a unit test on padToBucket rather than a wall-clock measurement,
// because the cases where a rounding bug lives — just under a boundary, exactly
// on one, just over — are the cases a measurement cannot separate from jitter.
func TestPairingFloorPadsToBucketBoundaries(t *testing.T) {
	const width = 250 * time.Millisecond
	for _, tc := range []struct{ elapsed, want time.Duration }{
		{0, width},
		{time.Nanosecond, width},
		{width - time.Nanosecond, width},
		{width, width},
		{width + time.Nanosecond, 2 * width},
		{width + 150*time.Millisecond, 2 * width},
		{2 * width, 2 * width},
		{2*width + time.Nanosecond, 3 * width},
	} {
		if got := padToBucket(tc.elapsed, width); got != tc.want {
			t.Errorf("padToBucket(%v, %v) = %v, want %v", tc.elapsed, width, got, tc.want)
		}
	}
	// A zero or negative width is "no pad", which is how a test switches the
	// quantizer off; it must not divide by zero on the way.
	if got := padToBucket(5*time.Millisecond, 0); got != 5*time.Millisecond {
		t.Errorf("padToBucket with no width = %v, want the elapsed time unchanged", got)
	}
}

// TestPairingRefusalsLandInTheSameTimingBucket is the timing test, and it
// asserts BUCKET EQUALITY rather than a minimum.
//
// A minimum was the previous assertion and it was too weak: the four paths do
// wildly different amounts of work — an unknown id returns before any
// comparison, a wrong code takes the registry's write lock and publishes the
// failure counter, an expired code burns the code and writes a record and a
// receipt, and a latched budget retries a write — and a minimum passes while one
// of them overruns by any amount at all. The overrun is exactly what an observer
// with a stopwatch reads.
//
// The quantizer makes the answer a STEP: every path that finishes its work
// inside one bucket leaves at one bucket, and a path that overruns leaves at
// two. So the assertion is that all four land in the same bucket, which on any
// machine that can do a few file writes in 250ms is bucket one.
func TestPairingRefusalsLandInTheSameTimingBucket(t *testing.T) {
	const width = 250 * time.Millisecond
	reg, clock, _, _ := testRegistry(t)
	srv, err := NewServer(ServerOptions{
		Addr: "127.0.0.1:7778", Devices: reg, Pairings: reg,
		Reader: newStubReader(), Writer: newStubWriter(),
		Captures:     NewReservationStore(t.TempDir()),
		Now:          func() time.Time { return *clock },
		PairingFloor: width,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Four devices, so each path drives its own pairing and none of them
	// disturbs another's state.
	live := pendingPairing(t, reg, "wrong code")
	stale := pendingPairing(t, reg, "expired code")
	latched := pendingPairing(t, reg, "unrecordable counter")
	unknown := live
	unknown.DeviceID = "dev_20260903_120000_deadbeef"
	wrong := live
	wrong.PairingCode = "wrong-code"

	// The expired path needs a code that MATCHES and is late, which is the
	// refusal that writes the most: it burns the code and writes a receipt.
	*clock = clock.Add(PairingTTL + time.Second)

	// The latched path needs a device whose counter write already failed.
	srv.confirms = &failingCounter{Confirmer: reg, failures: 1}
	latchedWrong := latched
	latchedWrong.PairingCode = "wrong-code"
	postConfirm(t, srv, latchedWrong)
	srv.confirms = reg
	// Put the latch back by hand: the line above spent the stub's one failure,
	// and what this case measures is the branch that retries an owed write.
	srv.unrecorded.latch(latched.DeviceID)

	buckets := map[string]int{}
	for _, tc := range []struct {
		name string
		body PairingConfirmation
	}{
		// Cheapest: Confirm returns before any comparison runs.
		{"an unknown device id", unknown},
		// Takes the write lock and publishes the failure counter.
		{"a wrong code against a live pairing", wrong},
		// Burns the code: a record write and a receipt write.
		{"a matching but expired code", stale},
		// Retries an owed counter write before refusing.
		{"a budget that could not be recorded", latchedWrong},
	} {
		start := time.Now()
		rec := postConfirm(t, srv, tc.body)
		elapsed := time.Since(start)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s = %d, want 401\n%s", tc.name, rec.Code, rec.Body.String())
		}
		// The bucket a duration fell in. Integer division, so anything in
		// [width, 2*width) is bucket 1 — the pad guarantees at least width, and
		// only a path that did more than a full bucket of WORK can reach 2.
		buckets[tc.name] = int(elapsed / width)
	}

	var first string
	for name, bucket := range buckets {
		if first == "" {
			first = name
		}
		if bucket != buckets[first] {
			t.Fatalf("%q landed in bucket %d and %q in bucket %d — the refusal paths are separable by a stopwatch\n%v",
				name, bucket, first, buckets[first], buckets)
		}
		if bucket < 1 {
			t.Fatalf("%q answered inside the first bucket boundary (%v); the pad did not run\n%v", name, width, buckets)
		}
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
