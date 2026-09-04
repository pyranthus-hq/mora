package companion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// testNow is the listener's pinned clock. It is a package-level constant rather
// than a literal in testServer so a reservation store, a registry and a server
// in one test agree on what "now" is.
var testNow = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// The stub kernel writer
// ---------------------------------------------------------------------------

// stubWriter answers the capture route the way a kernel would. Every field is
// settable so a test can pin a policy, fail a publish, or count how many times
// the vault was actually asked to write.
type stubWriter struct {
	mu sync.Mutex

	policy     WritePolicy
	policyErr  error
	publishErr error

	// publishes counts vault-write attempts. It is the duplicate detector: the
	// whole point of the reservation is that N retries produce ONE of these.
	publishes int
	// memoryIDs is handed out one per applied publish, so two applied writes
	// are visibly two memories rather than one id returned twice.
	nextMemory int
	// entered is signalled once per publish, and hold (when non-nil) is waited
	// on before the publish returns, so a test can pin one capture inside the
	// kernel while it drives a second.
	entered chan struct{}
	hold    chan struct{}
	// captured records every capture the kernel was handed.
	captured []Capture
}

func newStubWriter() *stubWriter { return &stubWriter{policy: PolicyOpen} }

func (w *stubWriter) Policy(context.Context) (WritePolicy, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.policy, w.policyErr
}

func (w *stubWriter) Publish(ctx context.Context, c Capture) (WriteOutcome, error) {
	w.mu.Lock()
	w.publishes++
	w.captured = append(w.captured, c)
	policy, err := w.policy, w.publishErr
	entered, hold := w.entered, w.hold
	w.nextMemory++
	n := w.nextMemory
	w.mu.Unlock()

	if entered != nil {
		entered <- struct{}{}
	}
	if hold != nil {
		<-hold
	}
	if err != nil {
		return WriteOutcome{}, err
	}
	if cerr := ctx.Err(); cerr != nil {
		return WriteOutcome{}, cerr
	}
	out := WriteOutcome{Policy: policy}
	switch policy {
	case PolicyReadonly:
		out.State, out.Reason = ReceiptRejected, ReasonPolicy
	case PolicyPropose:
		out.State = ReceiptAccepted
	default:
		out.State = ReceiptApplied
		// One id per applied write, so two writes are visibly two memories
		// rather than one id handed back twice.
		out.MemoryID = fmt.Sprintf("%s%08d", PrefixMemory, n)
	}
	return out, nil
}

func (w *stubWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.publishes
}

func (w *stubWriter) setPolicy(p WritePolicy) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.policy = p
}

// writerOf returns the stub the test server was built with.
func writerOf(t *testing.T, srv *Server) *stubWriter {
	t.Helper()
	w, ok := srv.writer.(*stubWriter)
	if !ok {
		t.Fatalf("the test server is not holding a stubWriter")
	}
	return w
}

// ---------------------------------------------------------------------------
// Capture helpers
// ---------------------------------------------------------------------------

// testDeviceID returns the one device testServer paired. The capture schema
// carries a device id and the listener compares it against the authenticated
// one, so a test body has to name the real device.
func testDeviceID(t *testing.T, srv *Server) string {
	t.Helper()
	reg, ok := srv.devices.(*Registry)
	if !ok {
		t.Fatalf("the test server is not holding a *Registry")
	}
	devices, err := reg.List()
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("the test registry holds %d devices, want exactly 1", len(devices))
	}
	return devices[0].DeviceID
}

// captureFor builds a valid capture whose idempotency key is derived from the
// text, so two calls with the same text are a RETRY and two calls with different
// text are two captures. A test that wants the same key over different text
// overrides the key itself.
func captureFor(t *testing.T, srv *Server, text string) Capture {
	t.Helper()
	c := NewCapture()
	c.IdempotencyKey = "idem." + strings.TrimPrefix(Fingerprint(text), "sha256:")[:16]
	c.DeviceID = testDeviceID(t, srv)
	c.CapturedAt = reservationStamp(testNow)
	c.RequestedLane = LaneMemory
	c.Intent = IntentRemember
	c.Scope = "personal"
	c.Text = text
	c.PayloadFingerprint = Fingerprint(text)
	return c
}

func captureBytes(t *testing.T, c Capture) []byte {
	t.Helper()
	body, err := Marshal(&c)
	if err != nil {
		t.Fatalf("marshal capture: %v", err)
	}
	return body
}

func captureBody(t *testing.T, srv *Server, text string) *strings.Reader {
	t.Helper()
	return strings.NewReader(string(captureBytes(t, captureFor(t, srv, text))))
}

// postCapture drives one capture through the REAL guard chain and returns the
// response. Everything in this file goes through the handler rather than calling
// the capture path directly: the guards, the limiter and the 2xx-only last-seen
// stamp are part of what is under test.
func postCapture(t *testing.T, srv *Server, token string, c Capture) *httptest.ResponseRecorder {
	t.Helper()
	return postRaw(srv, token, captureBytes(t, c))
}

// postRaw is postCapture without a *testing.T, so a concurrent test can drive it
// from a goroutine. Nothing in it can fail a test from the wrong goroutine.
func postRaw(srv *Server, token string, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, request(http.MethodPost, RouteCapture, token, strings.NewReader(string(body))))
	return rec
}

// decodeReceipt decodes a response body as a receipt and validates it, so no
// assertion in this file is made against a shape the contract would refuse.
func decodeReceipt(t *testing.T, rec *httptest.ResponseRecorder) Receipt {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("capture answered %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	var out Receipt
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode receipt: %v\n%s", err, rec.Body.String())
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("the listener returned a receipt that does not validate: %v\n%s", err, rec.Body.String())
	}
	return out
}

// ---------------------------------------------------------------------------
// The policy gate
// ---------------------------------------------------------------------------

// TestCaptureStateFollowsTheWritePolicy is the gate: N02's published table,
// driven end to end through the real handler under each of the three policies.
//
// The publish COUNT is asserted beside the state because the states alone would
// pass for a listener that wrote the vault and then labelled the receipt
// `rejected`. readonly must not reach the write path at all.
func TestCaptureStateFollowsTheWritePolicy(t *testing.T) {
	for _, tc := range []struct {
		policy   WritePolicy
		state    ReceiptState
		reason   RejectReason
		memory   bool
		settled  bool
		attempts int
	}{
		{PolicyReadonly, ReceiptRejected, ReasonPolicy, false, true, 1},
		{PolicyPropose, ReceiptAccepted, "", false, false, 1},
		{PolicyOpen, ReceiptApplied, "", true, true, 1},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			srv, _, _, token, _ := testServer(t)
			writer := writerOf(t, srv)
			writer.setPolicy(tc.policy)

			receipt := decodeReceipt(t, postCapture(t, srv, token, captureFor(t, srv, "the wifi code is on the fridge")))
			if receipt.State != tc.state {
				t.Fatalf("policy %s produced state %q, want %q", tc.policy, receipt.State, tc.state)
			}
			if receipt.Reason != tc.reason {
				t.Fatalf("policy %s produced reason %q, want %q", tc.policy, receipt.Reason, tc.reason)
			}
			if receipt.Policy != tc.policy {
				t.Fatalf("receipt names policy %q, want %q", receipt.Policy, tc.policy)
			}
			if (receipt.MemoryID != "") != tc.memory {
				t.Fatalf("policy %s produced memory_id %q", tc.policy, receipt.MemoryID)
			}
			if (receipt.SettledAt != "") != tc.settled {
				t.Fatalf("policy %s produced settled_at %q, want present=%t", tc.policy, receipt.SettledAt, tc.settled)
			}
		})
	}
}

// TestCaptureUnderReadonlyNeverReachesTheWritePath is the half of the table a
// state assertion cannot prove. `rejected: policy` has to mean the vault was
// never asked, not that it was asked and the answer was relabelled.
func TestCaptureUnderReadonlyNeverReachesTheWritePath(t *testing.T) {
	srv, _, _, token, _ := testServer(t)
	writer := writerOf(t, srv)
	writer.setPolicy(PolicyReadonly)

	receipt := decodeReceipt(t, postCapture(t, srv, token, captureFor(t, srv, "do not write this")))
	if receipt.State != ReceiptRejected || receipt.Reason != ReasonPolicy {
		t.Fatalf("readonly produced %s/%s, want rejected/policy", receipt.State, receipt.Reason)
	}
	// The kernel WAS consulted — the policy is read from the vault, not guessed
	// — but Publish returned the refusal without writing, which is what the stub
	// records as a publish that produced no memory.
	if receipt.MemoryID != "" {
		t.Fatalf("a readonly capture named memory %q", receipt.MemoryID)
	}
}

// TestCaptureAppliedOnlyAfterThePublicationCompletes proves the ordering claim
// in the issue: the state flips to applied AFTER the vault write, never before.
//
// The stub holds the publish open. While it is held, the request has not
// answered at all — so there is no window in which a receipt saying `applied`
// exists beside a write that has not landed.
func TestCaptureAppliedOnlyAfterThePublicationCompletes(t *testing.T) {
	srv, _, _, token, _ := testServer(t)
	writer := writerOf(t, srv)
	writer.entered = make(chan struct{})
	writer.hold = make(chan struct{})

	body := captureBytes(t, captureFor(t, srv, "held open"))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postRaw(srv, token, body) }()

	<-writer.entered
	select {
	case rec := <-done:
		t.Fatalf("the capture answered %d while the vault write was still running\n%s", rec.Code, rec.Body.String())
	case <-time.After(50 * time.Millisecond):
	}
	close(writer.hold)

	receipt := decodeReceipt(t, <-done)
	if receipt.State != ReceiptApplied {
		t.Fatalf("state = %q after the publication completed, want applied", receipt.State)
	}
	if receipt.MemoryID == "" || receipt.SettledAt == "" {
		t.Fatalf("an applied receipt must name its memory and be settled: %+v", receipt)
	}
}

// TestCaptureFailedPublishIsNotATerminalRejection pins the distinction between
// "this will never be applied" and "this attempt failed".
//
// A rejected receipt is terminal and would stop the phone retrying, so a kernel
// that could not decide must produce no receipt at all.
func TestCaptureFailedPublishIsNotATerminalRejection(t *testing.T) {
	srv, _, _, token, _ := testServer(t)
	writer := writerOf(t, srv)
	writer.publishErr = errors.New("the vault is on a disconnected volume")

	rec := postCapture(t, srv, token, captureFor(t, srv, "unlucky"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a failed publish answered %d, want 503\n%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "disconnected volume") {
		t.Fatalf("the kernel's error text reached the device: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Idempotency, through the handler
// ---------------------------------------------------------------------------

// TestCaptureRetryReturnsTheSameReceiptBytes is the retry contract. Same key,
// same payload: one write, and a response that is the same BYTES — not merely
// the same state, and not a second receipt id.
func TestCaptureRetryReturnsTheSameReceiptBytes(t *testing.T) {
	srv, _, _, token, _ := testServer(t)
	writer := writerOf(t, srv)
	capture := captureFor(t, srv, "sam owes me the deck by friday")

	first := postCapture(t, srv, token, capture)
	second := postCapture(t, srv, token, capture)

	if first.Body.String() != second.Body.String() {
		t.Fatalf("a retry returned different bytes\nfirst:\n%s\nsecond:\n%s", first.Body.String(), second.Body.String())
	}
	if got := writer.count(); got != 1 {
		t.Fatalf("the vault was written %d times for one idempotency key, want 1", got)
	}
	receipt := decodeReceipt(t, second)
	if receipt.State != ReceiptApplied {
		t.Fatalf("state = %q, want applied", receipt.State)
	}
}

// TestCaptureSameKeyDifferentPayloadIsAConflict is the other half. The first
// payload keeps the key; the second is refused rather than silently overwriting
// it or silently inheriting its receipt.
func TestCaptureSameKeyDifferentPayloadIsAConflict(t *testing.T) {
	srv, _, _, token, _ := testServer(t)
	writer := writerOf(t, srv)

	first := captureFor(t, srv, "the original note")
	applied := decodeReceipt(t, postCapture(t, srv, token, first))

	second := captureFor(t, srv, "a completely different note")
	second.IdempotencyKey = first.IdempotencyKey

	receipt := decodeReceipt(t, postCapture(t, srv, token, second))
	if receipt.State != ReceiptRejected || receipt.Reason != ReasonIdempotencyConflict {
		t.Fatalf("a reused key over new text produced %s/%s, want rejected/idempotency_conflict", receipt.State, receipt.Reason)
	}
	if got := writer.count(); got != 1 {
		t.Fatalf("the conflicting capture reached the vault: %d writes, want 1", got)
	}
	// The first receipt is untouched: a conflict must not consume the key it was
	// refused, or the second payload would win by asking twice.
	replay := decodeReceipt(t, postCapture(t, srv, token, first))
	if replay.ReceiptID != applied.ReceiptID {
		t.Fatalf("the conflict took the key: replay receipt %q, want %q", replay.ReceiptID, applied.ReceiptID)
	}
}

// TestCaptureConcurrentDuplicatesWriteOnce drives N goroutines at one key.
//
// One wins the reservation; the rest either wait for it and get the same
// receipt, or are told the capture is in flight. What none of them may do is
// produce a second vault write or a second receipt id.
func TestCaptureConcurrentDuplicatesWriteOnce(t *testing.T) {
	srv, _, _, token, _ := testServer(t)
	writer := writerOf(t, srv)
	body := captureBytes(t, captureFor(t, srv, "concurrent tap"))

	const callers = 8
	var wg sync.WaitGroup
	bodies := make([]string, callers)
	codes := make([]int, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := postRaw(srv, token, body)
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		}(i)
	}
	close(start)
	wg.Wait()

	if got := writer.count(); got != 1 {
		t.Fatalf("%d concurrent duplicates produced %d vault writes, want 1", callers, got)
	}
	// The listener's work budget is one kernel call at a time, so some callers
	// legitimately get a 503. Every caller that got an ANSWER must have got the
	// same one.
	answered := ""
	for i := range bodies {
		switch codes[i] {
		case http.StatusOK:
			if answered == "" {
				answered = bodies[i]
				continue
			}
			if bodies[i] != answered {
				t.Fatalf("two concurrent duplicates got different receipts\n%s\n%s", answered, bodies[i])
			}
		case http.StatusServiceUnavailable:
		default:
			t.Fatalf("concurrent duplicate %d answered %d: %s", i, codes[i], bodies[i])
		}
	}
	if answered == "" {
		t.Fatal("no concurrent duplicate got a receipt")
	}
}

// TestCaptureReservationSurvivesProcessRestart reopens the store over the same
// directory, which is what a restarted `mora companion serve` does.
//
// The reservation is a file, not a memory, so the answer after the restart is
// the same receipt rather than a second write.
func TestCaptureReservationSurvivesProcessRestart(t *testing.T) {
	srv, _, reg, token, _ := testServer(t)
	writer := writerOf(t, srv)
	capture := captureFor(t, srv, "survive the restart")
	first := decodeReceipt(t, postCapture(t, srv, token, capture))

	// A second listener over the SAME registry and the SAME reservation
	// directory, with its own in-memory state. This is a restart.
	restarted, err := NewServer(ServerOptions{
		Addr:     "127.0.0.1:7778",
		Devices:  reg,
		Reader:   newStubReader(),
		Writer:   writer,
		Captures: reopenStore(storeRootOf(srv), func() time.Time { return testNow }),
		Now:      func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	second := decodeReceipt(t, postCapture(t, restarted, token, capture))
	if second.ReceiptID != first.ReceiptID {
		t.Fatalf("after a restart the same key produced receipt %q, want %q", second.ReceiptID, first.ReceiptID)
	}
	if got := writer.count(); got != 1 {
		t.Fatalf("the restart wrote the vault again: %d writes, want 1", got)
	}
}

// TestCaptureCrashBetweenReservationAndWriteAppliesExactlyOnce is the crash
// gate.
//
// The first attempt reserves durably and is then killed before the vault write
// confirms — modelled by a publish that panics, which is as close to a lost
// process as an in-process test gets while still leaving the on-disk state a
// crash would leave. A restarted listener retries the same key and must produce
// exactly one applied receipt and exactly one vault artefact.
func TestCaptureCrashBetweenReservationAndWriteAppliesExactlyOnce(t *testing.T) {
	srv, _, reg, token, _ := testServer(t)
	root := storeRootOf(srv)
	capture := captureFor(t, srv, "killed mid-write")

	crashing := newStubWriter()
	crashing.publishErr = errors.New("process died")
	crashed, err := NewServer(ServerOptions{
		Addr:     "127.0.0.1:7778",
		Devices:  reg,
		Reader:   newStubReader(),
		Writer:   crashing,
		Captures: reopenStore(root, func() time.Time { return testNow }),
		Now:      func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("first listener: %v", err)
	}
	if rec := postCapture(t, crashed, token, capture); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("the crashing attempt answered %d, want 503", rec.Code)
	}
	// The reservation is on disk and PENDING: the key was claimed before the
	// write was attempted, which is the ordering the whole design turns on.
	if state := reservationStateOn(t, root, capture); state != reservationPending {
		t.Fatalf("after the crash the reservation is %q, want pending", state)
	}

	// The restart is past the takeover window, which is how a live process tells
	// a crashed reservation from a peer that is still inside its write.
	after := testNow.Add(ReservationTakeover + time.Second)
	survivor := newStubWriter()
	restarted, err := NewServer(ServerOptions{
		Addr:     "127.0.0.1:7778",
		Devices:  reg,
		Reader:   newStubReader(),
		Writer:   survivor,
		Captures: reopenStore(root, func() time.Time { return after }),
		Now:      func() time.Time { return after },
	})
	if err != nil {
		t.Fatalf("restarted listener: %v", err)
	}
	receipt := decodeReceipt(t, postCapture(t, restarted, token, capture))
	if receipt.State != ReceiptApplied {
		t.Fatalf("the retry produced state %q, want applied", receipt.State)
	}
	if got := survivor.count(); got != 1 {
		t.Fatalf("the retry wrote the vault %d times, want exactly 1", got)
	}
	if got := crashing.count(); got != 1 {
		t.Fatalf("the crashed attempt wrote the vault %d times, want 1 attempt", got)
	}
	// And the retry is now the settled answer, so a THIRD attempt is a replay
	// rather than a second write.
	replay := decodeReceipt(t, postCapture(t, restarted, token, capture))
	if replay.ReceiptID != receipt.ReceiptID {
		t.Fatalf("a third attempt minted receipt %q, want the settled %q", replay.ReceiptID, receipt.ReceiptID)
	}
	if got := survivor.count(); got != 1 {
		t.Fatalf("a third attempt wrote the vault again: %d writes, want 1", got)
	}
}

// TestCapturePendingReservationIsNotTakenOverEarly is the other side of the
// takeover window. A peer still inside its vault write must not have its key
// taken, or the recovery rule would itself be the duplicate.
func TestCapturePendingReservationIsNotTakenOverEarly(t *testing.T) {
	srv, _, reg, token, _ := testServer(t)
	root := storeRootOf(srv)
	capture := captureFor(t, srv, "still running")

	stalled := newStubWriter()
	stalled.publishErr = errors.New("stalled")
	first, err := NewServer(ServerOptions{
		Addr: "127.0.0.1:7778", Devices: reg, Reader: newStubReader(), Writer: stalled,
		Captures: reopenStore(root, func() time.Time { return testNow }),
		Now:      func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("first listener: %v", err)
	}
	postCapture(t, first, token, capture)

	// A different process, one second later — well inside the window.
	peer := newStubWriter()
	second, err := NewServer(ServerOptions{
		Addr: "127.0.0.1:7778", Devices: reg, Reader: newStubReader(), Writer: peer,
		Captures: reopenStore(root, func() time.Time { return testNow.Add(time.Second) }),
		Now:      func() time.Time { return testNow.Add(time.Second) },
	})
	if err != nil {
		t.Fatalf("second listener: %v", err)
	}
	rec := postCapture(t, second, token, capture)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a peer inside the takeover window answered %d, want 503\n%s", rec.Code, rec.Body.String())
	}
	if got := peer.count(); got != 0 {
		t.Fatalf("the peer wrote the vault %d times, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// What a capture may not do
// ---------------------------------------------------------------------------

// TestCaptureCannotClaimAnotherDevice is the impersonation gate. The body's
// device_id is compared against the authenticated one and never trusted, and the
// receipt is stamped with the authenticated id.
func TestCaptureCannotClaimAnotherDevice(t *testing.T) {
	srv, _, _, token, _ := testServer(t)
	writer := writerOf(t, srv)

	capture := captureFor(t, srv, "not mine")
	mine := capture.DeviceID
	capture.DeviceID = "dev_20260903_120000_ffffffff"

	receipt := decodeReceipt(t, postCapture(t, srv, token, capture))
	if receipt.State != ReceiptRejected || receipt.Reason != ReasonUnknownDevice {
		t.Fatalf("a capture claiming another device produced %s/%s, want rejected/unknown_device", receipt.State, receipt.Reason)
	}
	if receipt.DeviceID != mine {
		t.Fatalf("the receipt names device %q, want the authenticated %q", receipt.DeviceID, mine)
	}
	if got := writer.count(); got != 0 {
		t.Fatalf("an impersonating capture reached the vault %d times, want 0", got)
	}
}

// TestCaptureRefusesTheLanesItCannotExecute pins v1's scope. `ask` is the
// context route's job and `investigate` is the async lane, which has no worker
// yet — both are refused with the published reason rather than routed somewhere
// that does not exist.
func TestCaptureRefusesTheLanesItCannotExecute(t *testing.T) {
	for _, tc := range []struct {
		intent Intent
		lane   Lane
	}{
		{IntentAsk, LaneMemory},
		{IntentInvestigate, LaneResearch},
	} {
		t.Run(string(tc.intent), func(t *testing.T) {
			srv, _, _, token, _ := testServer(t)
			writer := writerOf(t, srv)
			capture := captureFor(t, srv, "wrong lane")
			capture.Intent = tc.intent
			capture.RequestedLane = tc.lane

			receipt := decodeReceipt(t, postCapture(t, srv, token, capture))
			if receipt.State != ReceiptRejected || receipt.Reason != ReasonUnsupportedLane {
				t.Fatalf("intent %s produced %s/%s, want rejected/unsupported_lane", tc.intent, receipt.State, receipt.Reason)
			}
			if got := writer.count(); got != 0 {
				t.Fatalf("an unsupported lane reached the vault %d times, want 0", got)
			}
		})
	}
}

// TestCaptureFromARevokedDeviceCannotBeCompleted is the revocation gate.
//
// A device revoked between its reservation and its retry gets the same opaque
// 401 every other credential failure gets, and its pending reservation stays
// pending forever — a revoked device's claimed key is never completed by anyone,
// because completing it would be a write the operator revoked the right to make.
func TestCaptureFromARevokedDeviceCannotBeCompleted(t *testing.T) {
	srv, _, reg, token, _ := testServer(t)
	root := storeRootOf(srv)
	capture := captureFor(t, srv, "revoke me mid-flight")

	stalled := writerOf(t, srv)
	stalled.publishErr = errors.New("stalled")
	if rec := postCapture(t, srv, token, capture); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("the stalled capture answered %d, want 503", rec.Code)
	}
	if state := reservationStateOn(t, root, capture); state != reservationPending {
		t.Fatalf("the reservation is %q, want pending", state)
	}

	if _, _, err := reg.Revoke(capture.DeviceID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	stalled.publishErr = nil

	rec := postCapture(t, srv, token, capture)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked device's retry answered %d, want 401\n%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != unauthorizedBody {
		t.Fatalf("a revoked device got a distinguishable refusal: %q", body)
	}
	if state := reservationStateOn(t, root, capture); state != reservationPending {
		t.Fatalf("a revoked device's reservation is %q, want it left pending", state)
	}
}

// TestCaptureReceiptNeverEchoesThePayload drives the real handler and fails on
// any word of the capture appearing in the response.
//
// A receipt is identifiers, a state, a policy, a fingerprint and two timestamps.
// It is stored, listed and shown in an Activity row, and the moment it carries a
// snippet the phone is holding vault text that nothing governs.
func TestCaptureReceiptNeverEchoesThePayload(t *testing.T) {
	srv, _, _, token, _ := testServer(t)
	// Every word is long and distinctive: a short fragment turns up inside a
	// hex identifier by coincidence, and a witness that fails on coincidence
	// stops meaning anything.
	const secret = "passphrase correcthorsebatterystaple opens grandmother's safebox"
	rec := postCapture(t, srv, token, captureFor(t, srv, secret))
	body := rec.Body.String()
	for _, word := range strings.Fields(secret) {
		if strings.Contains(body, word) {
			t.Fatalf("the receipt echoed %q from the capture:\n%s", word, body)
		}
	}
}

// TestCaptureRefusesAnOversizeBody pins the tighter of the two bounds.
//
// The guard chain caps every request at MaxRequestBytes; capture caps ITSELF at
// MaxCaptureBytes, because it is the route that turns bytes into a file. The
// body case is what proves the second bound exists: it is comfortably under the
// guard chain's limit and must still be refused by the handler, before a decode
// and before the kernel.
func TestCaptureRefusesAnOversizeBody(t *testing.T) {
	t.Run("body over MaxCaptureBytes", func(t *testing.T) {
		srv, _, _, token, _ := testServer(t)
		writer := writerOf(t, srv)

		// Valid JSON, well under MaxRequestBytes, well over MaxCaptureBytes. The
		// padding is an unknown field, so if the bound ever stopped firing the
		// request would fail the strict decode instead — a different code, which
		// is what the status assertion below distinguishes.
		valid := captureBytes(t, captureFor(t, srv, "small"))
		padding := strings.Repeat("p", MaxCaptureBytes)
		body := strings.Replace(string(valid), `"text"`, `"padding": "`+padding+`",
  "text"`, 1)
		if len(body) <= MaxCaptureBytes || len(body) >= MaxRequestBytes {
			t.Fatalf("the fixture is %d bytes; it must sit between %d and %d", len(body), MaxCaptureBytes, MaxRequestBytes)
		}

		rec := postRaw(srv, token, []byte(body))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("a %d-byte capture answered %d, want 413\n%s", len(body), rec.Code, rec.Body.String())
		}
		if got := writer.count(); got != 0 {
			t.Fatalf("an oversize capture reached the vault %d times, want 0", got)
		}
	})

	t.Run("text over MaxCaptureTextBytes", func(t *testing.T) {
		srv, _, _, token, _ := testServer(t)
		writer := writerOf(t, srv)

		// Marshal would refuse this payload, which is the point: it is built with
		// encoding/json directly so the LISTENER is the thing doing the refusing.
		oversize := captureFor(t, srv, strings.Repeat("x", MaxCaptureTextBytes+1))
		oversize.PayloadFingerprint = Fingerprint(oversize.Text)
		raw, err := json.Marshal(oversize)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rec := postRaw(srv, token, raw)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("an oversize text answered %d, want 400\n%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), CodeTooLarge) {
			t.Fatalf("the refusal does not carry the schema code: %s", rec.Body.String())
		}
		if got := writer.count(); got != 0 {
			t.Fatalf("an oversize capture reached the vault %d times, want 0", got)
		}
	})
}

// TestCaptureDecodesStrictly is the strict-inbound half for the write route. An
// unknown field, a fingerprint that does not cover the text, and a scope the
// contract does not publish are all refused before the kernel is reached, and
// the refusal carries the schema code rather than the value.
func TestCaptureDecodesStrictly(t *testing.T) {
	srv, _, _, token, _ := testServer(t)
	writer := writerOf(t, srv)
	valid := captureFor(t, srv, "strict")

	for _, tc := range []struct {
		name string
		body string
	}{
		{"unknown field", strings.Replace(string(captureBytes(t, valid)), `"text"`, `"colour": "red",
  "text"`, 1)},
		{"fingerprint does not cover the text", strings.Replace(string(captureBytes(t, valid)), valid.PayloadFingerprint, Fingerprint("something else"), 1)},
		{"scope names a source", strings.Replace(string(captureBytes(t, valid)), `"personal"`, `"gmail:work"`, 1)},
		{"no envelope", `{"idempotency_key":"k","text":"hi"}`},
		{"trailing data", string(captureBytes(t, valid)) + "}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, request(http.MethodPost, RouteCapture, token, strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("answered %d, want 400\n%s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "strict") {
				t.Fatalf("the rejection echoed the capture text: %s", rec.Body.String())
			}
			if got := writer.count(); got != 0 {
				t.Fatalf("a rejected body reached the vault %d times, want 0", got)
			}
		})
	}
}

// TestCaptureStampsLastSeenOnlyOn2xx is the N12 invariant applied to the write
// route: a last-seen stamp records that a device was SERVED.
func TestCaptureStampsLastSeenOnlyOn2xx(t *testing.T) {
	srv, _, reg, token, _ := testServer(t)
	writer := writerOf(t, srv)
	writer.publishErr = errors.New("nope")

	if rec := postCapture(t, srv, token, captureFor(t, srv, "failed")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	devices, err := reg.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if devices[0].LastSeenAt != "" {
		t.Fatalf("a 503 stamped last_seen_at = %q", devices[0].LastSeenAt)
	}

	writer.publishErr = nil
	if rec := postCapture(t, srv, token, captureFor(t, srv, "served")); rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	devices, err = reg.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if devices[0].LastSeenAt == "" {
		t.Fatal("a served capture did not stamp last_seen_at")
	}
}

// TestCaptureGoesThroughTheWorkBudget proves the write route is under the same
// one-at-a-time limiter every read is. A capture walks the vault's write path;
// it is the last route that should be able to run unbounded in parallel.
func TestCaptureGoesThroughTheWorkBudget(t *testing.T) {
	srv, _, _, token, _ := testServer(t)
	writer := writerOf(t, srv)
	writer.entered = make(chan struct{})
	writer.hold = make(chan struct{})

	holder := captureBytes(t, captureFor(t, srv, "holds the slot"))
	go func() { postRaw(srv, token, holder) }()
	<-writer.entered

	// A DIFFERENT capture, so nothing about idempotency is doing the refusing.
	rec := postCapture(t, srv, token, captureFor(t, srv, "wants the slot"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a second capture answered %d while the budget was held, want 503\n%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("the refusal carries no Retry-After")
	}
	close(writer.hold)
}

// TestCaptureIsTheOnlyRouteThatWrites is the seam witness.
//
// A device can touch exactly what the two kernel interfaces expose. Reader is
// the READ seam and must stay three read methods: a mutation named there would
// mean a read route could write. Writer is the one place a mutation may enter,
// and exactly one route reaches it.
//
// The interfaces are read out of the source with go/ast rather than asserted
// against a list in prose, so a method added later fails this test even if no
// test ever calls it.
func TestCaptureIsTheOnlyRouteThatWrites(t *testing.T) {
	srv, _, _, _, _ := testServer(t)
	writes := 0
	for _, route := range srv.Routes() {
		if route.Pattern == RouteCapture {
			writes++
		}
	}
	if writes != 1 {
		t.Fatalf("the allowlist holds %d capture routes, want exactly 1", writes)
	}

	methods := interfaceMethods(t, "Reader")
	want := []string{"Context", "Health", "Today"}
	if len(methods) != len(want) {
		t.Fatalf("companion.Reader declares %v, want exactly %v", methods, want)
	}
	for i := range want {
		if methods[i] != want[i] {
			t.Fatalf("companion.Reader declares %v, want exactly %v", methods, want)
		}
	}
}

// interfaceMethods returns the sorted method names of a package-level interface,
// read from the package's own source.
func interfaceMethods(t *testing.T, name string) []string {
	t.Helper()
	for _, file := range companionProductionFiles(t) {
		var found []string
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || spec.Name.Name != name {
				return true
			}
			iface, ok := spec.Type.(*ast.InterfaceType)
			if !ok {
				return true
			}
			for _, method := range iface.Methods.List {
				for _, ident := range method.Names {
					found = append(found, ident.Name)
				}
			}
			return false
		})
		if found != nil {
			sort.Strings(found)
			return found
		}
	}
	t.Fatalf("no interface named %s in this package", name)
	return nil
}
