package companion

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Store helpers
// ---------------------------------------------------------------------------

// storeRootOf is the directory a test server's reservations live in, so a
// restart test can build a second store over the same state.
func storeRootOf(srv *Server) string { return srv.captures.root }

// reopenStore builds a second store over an EXISTING store's directory, which is
// what a restarted `mora companion serve` does. It sets the root directly
// because NewReservationStore derives one from a StateDir, and handing it the
// derived path again would nest a second tree inside the first.
func reopenStore(root string, now func() time.Time) *ReservationStore {
	s := NewReservationStore("", WithReservationClock(now))
	s.root = root
	// Opening takes a census, and the constructor took it over the wrong (empty)
	// root before the real one was set. Take it again so a reopened store behaves
	// exactly as one constructed over this directory would.
	s.census(s.now())
	return s
}

// reservationRecordOn reads one stored reservation straight off the disk.
// Asserting on the FILE rather than on an API is the point in the crash tests:
// what survives a process is the file.
func reservationRecordOn(t *testing.T, root, deviceID, key string) reservationRecord {
	t.Helper()
	store := &ReservationStore{root: root, now: time.Now}
	record, err := readReservation(store.path(deviceID, key))
	if err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	return record
}

func reservationStateOn(t *testing.T, root string, c Capture) reservationState {
	t.Helper()
	return reservationRecordOn(t, root, c.DeviceID, c.IdempotencyKey).State
}

// storeTotal reads the store's in-memory census.
func storeTotal(s *ReservationStore) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// storePending reads the store's in-memory count of live claims.
func storePending(s *ReservationStore) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// reservationExists reports whether a key has a file at all.
func reservationExists(root, deviceID, key string) bool {
	store := &ReservationStore{root: root, now: time.Now}
	_, err := os.Stat(store.path(deviceID, key))
	return err == nil
}

// testStore returns a reservation store over a temporary directory with a
// movable clock, so a test can drive the takeover window and the TTL without
// waiting for them.
func testStore(t *testing.T) (*ReservationStore, *time.Time, string) {
	t.Helper()
	root := t.TempDir()
	now := testNow
	clock := &now
	return NewReservationStore(root, WithReservationClock(func() time.Time { return *clock })), &now, filepath.Join(root, "companion", "captures")
}

const testStoreDevice = "dev_20260903_120000_a1b2c3d4"

// storeIdentity builds a CaptureIdentity for a key and a payload. Identity and
// fingerprint are derived from different strings so a test that changes one
// without the other is visibly doing so.
func storeIdentity(key, payload string) CaptureIdentity {
	return CaptureIdentity{
		DeviceID:    testStoreDevice,
		Key:         key,
		Identity:    Fingerprint("identity:" + payload),
		Fingerprint: Fingerprint(payload),
		MemoryID:    PrefixMemory + "20260903_120000_" + strings.TrimPrefix(Fingerprint(key+payload), "sha256:")[:8],
	}
}

// storeReceipt builds a terminal receipt for a reserved key, and the bytes that
// answered it. The store validates what it is asked to settle, so a helper that
// produced an invalid one would fail at Settle rather than at the assertion
// under test.
func storeReceipt(t *testing.T, store *ReservationStore, id CaptureIdentity, state ReceiptState) (Receipt, []byte) {
	t.Helper()
	receiptID, err := store.NewID(PrefixReceipt)
	if err != nil {
		t.Fatalf("mint receipt id: %v", err)
	}
	requestID, err := store.NewID(PrefixRequest)
	if err != nil {
		t.Fatalf("mint request id: %v", err)
	}
	r := NewReceipt()
	r.ReceiptID = receiptID
	r.RequestID = requestID
	r.IdempotencyKey = id.Key
	r.DeviceID = id.DeviceID
	r.State = state
	r.PayloadFingerprint = id.Fingerprint
	r.Policy = PolicyOpen
	r.MemoryID = wireMemoryID(id.MemoryID)
	r.ReceivedAt = reservationStamp(store.now())
	r.SettledAt = reservationStamp(store.now())
	if err := r.Validate(); err != nil {
		t.Fatalf("the helper built an invalid receipt: %v", err)
	}
	body, err := Marshal(&r)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	return r, body
}

// ---------------------------------------------------------------------------
// The reservation lifecycle
// ---------------------------------------------------------------------------

// TestReservationIsDurableBeforeTheWrite is the ordering claim, asserted against
// the filesystem.
//
// The claim is handed back only after the reservation is on disk, and it carries
// the vault id the write will pin. Reserving AFTER the write would mean a crash
// in between leaves a memory in the vault that no key points at.
func TestReservationIsDurableBeforeTheWrite(t *testing.T) {
	store, _, root := testStore(t)
	id := storeIdentity("key.one", "a note")

	_, claim, err := store.Reserve(id)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if claim == nil {
		t.Fatal("a fresh key returned no claim")
	}
	if claim.TakenOver {
		t.Fatal("a fresh key reported a takeover")
	}
	if claim.MemoryID() != id.MemoryID {
		t.Fatalf("the claim publishes under %q, want the pinned %q", claim.MemoryID(), id.MemoryID)
	}
	record, err := readReservation(store.path(testStoreDevice, "key.one"))
	if err != nil {
		t.Fatalf("the reservation is not on disk: %v", err)
	}
	if record.State != reservationPending {
		t.Fatalf("state = %q, want pending", record.State)
	}
	// The pinned id is on disk BEFORE the write, which is what a crashed attempt
	// leaves behind for the retry to aim at.
	if record.MemoryID != id.MemoryID {
		t.Fatalf("the record pins %q, want %q", record.MemoryID, id.MemoryID)
	}
	if record.CaptureIdentity != id.Identity || record.PayloadFingerprint != id.Fingerprint {
		t.Fatalf("the record does not carry both digests: %+v", record)
	}
	if record.Receipt != nil || record.Response != "" {
		t.Fatal("a pending reservation carries an answer")
	}
	if !strings.HasPrefix(store.path(testStoreDevice, "key.one"), filepath.Join(root, testStoreDevice)) {
		t.Fatalf("the reservation is not under the device's own directory")
	}
}

// TestReservationReplayReturnsTheStoredBytes proves the replay answer is the
// BYTES the first attempt returned, not a re-marshalling of the same fields.
//
// A rebuilt receipt would carry a new receipt id and a new settled_at; even a
// perfectly rebuilt one is only equal by luck of the encoder, and a client that
// hashes or caches a response body is entitled to better than luck.
func TestReservationReplayReturnsTheStoredBytes(t *testing.T) {
	store, _, _ := testStore(t)
	id := storeIdentity("key.one", "a note")

	_, claim, err := store.Reserve(id)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	receipt, body := storeReceipt(t, store, id, ReceiptApplied)
	if err := claim.Settle(receipt, body); err != nil {
		t.Fatalf("settle: %v", err)
	}

	replay, again, err := store.Reserve(id)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if again != nil {
		t.Fatal("a settled key handed out a second claim")
	}
	if !bytes.Equal(replay, body) {
		t.Fatalf("the replay is not the stored bytes\nwant:\n%s\ngot:\n%s", body, replay)
	}
}

// TestReservationSettleRequiresTheAnsweringBytes. A settled record with no
// response is a record that cannot answer a replay, so the store refuses to
// create one rather than discovering the hole on the retry.
func TestReservationSettleRequiresTheAnsweringBytes(t *testing.T) {
	store, _, _ := testStore(t)
	id := storeIdentity("key.one", "a note")
	_, claim, err := store.Reserve(id)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	receipt, _ := storeReceipt(t, store, id, ReceiptApplied)
	if err := claim.Settle(receipt, nil); err == nil {
		t.Fatal("Settle stored a receipt with no bytes to answer with")
	}
}

// TestReservationSameKeyDifferentCaptureIsAConflict. The first capture keeps the
// key whether it settled or is still pending: an identity check that ran only
// against settled records would let a second capture reclaim a crashed
// reservation belonging to the first.
func TestReservationSameKeyDifferentCaptureIsAConflict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		settle bool
	}{
		{"settled", true},
		{"still pending", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _, _ := testStore(t)
			first := storeIdentity("key.one", "the original")

			_, claim, err := store.Reserve(first)
			if err != nil {
				t.Fatalf("reserve: %v", err)
			}
			if tc.settle {
				receipt, body := storeReceipt(t, store, first, ReceiptApplied)
				if err := claim.Settle(receipt, body); err != nil {
					t.Fatalf("settle: %v", err)
				}
			} else {
				claim.Abandon()
			}

			second := storeIdentity("key.one", "something else")
			if _, _, err := store.Reserve(second); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("err = %v, want ErrIdempotencyConflict", err)
			}
		})
	}
}

// TestReservationIsScopedPerDevice is the isolation gate.
//
// Two devices that choose the same key must not collide, and — the reason this
// is a security property and not a tidiness one — no device may reach another
// device's receipt by guessing its idempotency key. A receipt names a memory, so
// a cross-device lookup would be a read oracle over somebody else's writes.
func TestReservationIsScopedPerDevice(t *testing.T) {
	store, _, _ := testStore(t)
	const other = "dev_20260903_120000_99999999"

	mine := storeIdentity("shared.key", "my note")
	_, claim, err := store.Reserve(mine)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	receipt, body := storeReceipt(t, store, mine, ReceiptApplied)
	if err := claim.Settle(receipt, body); err != nil {
		t.Fatalf("settle: %v", err)
	}

	// The same key from another device with a DIFFERENT capture. If the key space
	// were shared this would be a conflict; scoped per device it is a fresh
	// reservation.
	theirs := storeIdentity("shared.key", "their note")
	theirs.DeviceID = other
	replay, second, err := store.Reserve(theirs)
	if err != nil {
		t.Fatalf("the second device's reservation failed: %v", err)
	}
	if second == nil {
		t.Fatalf("the second device was handed the first device's answer: %s", replay)
	}
	// And the same key with the SAME capture does not reach the other device's
	// stored bytes either.
	sameCapture := mine
	sameCapture.DeviceID = other
	replay, third, err := store.Reserve(sameCapture)
	if err == nil && third == nil && bytes.Equal(replay, body) {
		t.Fatalf("device %s read device %s's stored response", other, testStoreDevice)
	}
}

// TestReservationConcurrentDuplicatesElectOneWinner drives N goroutines at one
// key against the store directly, without the listener's one-at-a-time work
// budget in the way.
//
// Exactly one wins the claim. Every other caller either waits for it and reads
// the settled bytes, or is told the capture is in flight.
func TestReservationConcurrentDuplicatesElectOneWinner(t *testing.T) {
	store, _, _ := testStore(t)
	id := storeIdentity("key.one", "concurrent")

	const callers = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claims  int
		replays [][]byte
		busy    int
	)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			replay, claim, err := store.Reserve(id)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrCaptureInFlight):
				busy++
			case err != nil:
				t.Errorf("reserve: %v", err)
			case claim != nil:
				claims++
				receipt, body := storeReceipt(t, store, id, ReceiptApplied)
				if serr := claim.Settle(receipt, body); serr != nil {
					t.Errorf("settle: %v", serr)
				}
			default:
				replays = append(replays, replay)
			}
		}()
	}
	close(start)
	wg.Wait()

	if claims != 1 {
		t.Fatalf("%d callers won a claim, want exactly 1", claims)
	}
	if claims+len(replays)+busy != callers {
		t.Fatalf("%d claims + %d replays + %d busy != %d callers", claims, len(replays), busy, callers)
	}
	for i, r := range replays {
		if i > 0 && !bytes.Equal(r, replays[0]) {
			t.Fatalf("two waiters got different bytes:\n%s\n%s", replays[0], r)
		}
	}
}

// TestReservationSurvivesTheStoreValue reopens the directory with a second
// store, which is what a restarted process does. The in-flight set is memory;
// the reservation is a file.
func TestReservationSurvivesTheStoreValue(t *testing.T) {
	store, clock, root := testStore(t)
	id := storeIdentity("key.one", "durable")

	_, claim, err := store.Reserve(id)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	receipt, body := storeReceipt(t, store, id, ReceiptApplied)
	if err := claim.Settle(receipt, body); err != nil {
		t.Fatalf("settle: %v", err)
	}

	reopened := reopenStore(root, func() time.Time { return *clock })
	replay, again, err := reopened.Reserve(id)
	if err != nil {
		t.Fatalf("reserve after reopen: %v", err)
	}
	if again != nil {
		t.Fatal("a reopened store handed out a second claim for a settled key")
	}
	if !bytes.Equal(replay, body) {
		t.Fatalf("a reopened store returned different bytes")
	}
}

// TestReservationTakeoverWaitsForTheWindow is the crash-recovery rule, both ways
// round.
//
// Inside the window a pending reservation belongs to a caller that may still be
// inside its vault write, and reclaiming it would mean two callers doing the
// same work. Past the window it is a crash, and refusing forever would wedge the
// key.
func TestReservationTakeoverWaitsForTheWindow(t *testing.T) {
	store, clock, _ := testStore(t)
	id := storeIdentity("key.one", "crashed")

	_, claim, err := store.Reserve(id)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Abandon models the crash: the in-process hold is gone and the PENDING
	// record stays on disk, which is exactly the state a killed process leaves.
	claim.Abandon()

	*clock = testNow.Add(ReservationTakeover - time.Second)
	if _, _, err := store.Reserve(id); !errors.Is(err, ErrCaptureInFlight) {
		t.Fatalf("inside the window err = %v, want ErrCaptureInFlight", err)
	}

	// The retry asks under a DIFFERENT derived id, which is what a client whose
	// captured_at drifted between attempts produces. The reclaimed claim must
	// still aim at the id the crashed attempt PINNED: that id is where the
	// memory either is or is not, and re-deriving one here would send the retry
	// somewhere the create-exclusive publish has nothing to refuse.
	*clock = testNow.Add(ReservationTakeover)
	drifted := id
	drifted.MemoryID = PrefixMemory + "20260904_090000_ffffffff"
	_, recovered, err := store.Reserve(drifted)
	if err != nil {
		t.Fatalf("past the window: %v", err)
	}
	if recovered == nil {
		t.Fatal("past the takeover window a crashed reservation was not recoverable")
	}
	if !recovered.TakenOver {
		t.Fatal("a reclaimed reservation did not report the takeover")
	}
	if recovered.MemoryID() != id.MemoryID {
		t.Fatalf("the reclaimed claim publishes under %q, want the pinned %q", recovered.MemoryID(), id.MemoryID)
	}
	// And the identity the kernel is ASKED about is the one that was reserved,
	// not the one the caller just recomputed. They differ here precisely because
	// the stamp drifted, which is the case a recomputed identity would get wrong.
	asked := recovered.Identity()
	if asked.MemoryID != id.MemoryID || asked.Identity != id.Identity ||
		asked.Key != id.Key || asked.DeviceID != id.DeviceID {
		t.Fatalf("the reclaimed claim reports identity %+v, want what was reserved %+v", asked, id)
	}
	if asked.MemoryID == drifted.MemoryID {
		t.Fatal("the reclaimed claim reports the drifted id; a retry would aim at a path nothing holds")
	}
	// And the record on disk still pins it, so a THIRD attempt agrees too.
	record, err := readReservation(store.path(testStoreDevice, id.Key))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if record.MemoryID != id.MemoryID {
		t.Fatalf("the reclaimed record pins %q, want %q", record.MemoryID, id.MemoryID)
	}
}

// TestReservationSettleRefusesAForeignReceipt. The store answers future replays
// with what it settled, so a receipt for a different key, device or payload
// would make it hand somebody else's outcome to the next caller.
func TestReservationSettleRefusesAForeignReceipt(t *testing.T) {
	store, _, _ := testStore(t)
	id := storeIdentity("key.one", "mine")

	_, claim, err := store.Reserve(id)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	other := storeIdentity("key.two", "mine")
	foreign, body := storeReceipt(t, store, other, ReceiptApplied)
	if err := claim.Settle(foreign, body); err == nil {
		t.Fatal("Settle accepted a receipt for another key")
	}
	record, err := readReservation(store.path(testStoreDevice, "key.one"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if record.State != reservationPending || record.Receipt != nil {
		t.Fatalf("a refused settle changed the record: %+v", record)
	}
}

// TestReservationRefusesAnInvalidReceipt. A receipt that does not validate must
// never reach storage, because storage is where a replay reads it FROM.
func TestReservationRefusesAnInvalidReceipt(t *testing.T) {
	store, _, _ := testStore(t)
	id := storeIdentity("key.one", "mine")

	_, claim, err := store.Reserve(id)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	broken, body := storeReceipt(t, store, id, ReceiptApplied)
	broken.MemoryID = "" // an applied receipt that names no memory
	if err := claim.Settle(broken, body); err == nil {
		t.Fatal("Settle stored a receipt the contract forbids")
	}
}

// ---------------------------------------------------------------------------
// Bounds
// ---------------------------------------------------------------------------

// TestReservationRefusesPastThePendingBound is the HARD bound.
//
// Round one's bound was not hard: nothing evicted an unexpired pending record,
// so a kernel that failed every request turned each fresh key into a file that
// was never settled and never collected. The refusal has to happen BEFORE the
// write, or the bound is a promise to tidy up rather than a limit.
func TestReservationRefusesPastThePendingBound(t *testing.T) {
	store, _, root := testStore(t)

	for i := 0; i < MaxPendingReservations; i++ {
		id := storeIdentity(fmt.Sprintf("key.%03d", i), "payload")
		_, claim, err := store.Reserve(id)
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		// Abandoned, not settled: every one of these is a live claim.
		claim.Abandon()
	}

	over := storeIdentity("key.over", "payload")
	_, claim, err := store.Reserve(over)
	if !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("at the bound err = %v, want ErrTooManyPending", err)
	}
	if claim != nil {
		t.Fatal("a refused reservation handed out a claim")
	}
	// And NO file was created. A bound that admitted the request and tidied up
	// afterwards would still be a file per request while the pressure lasted.
	if reservationExists(root, testStoreDevice, "key.over") {
		t.Fatal("the refused key created a reservation file")
	}
	entries, err := os.ReadDir(filepath.Join(root, testStoreDevice))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != MaxPendingReservations {
		t.Fatalf("the store holds %d reservations, want %d", len(entries), MaxPendingReservations)
	}
}

// TestReservationSweepsCrashedPendingRecords proves the sweep, on both the
// occasions it has to run.
//
// Sweeping a crashed pending record is only safe because the memory id is
// DERIVED: the retry re-derives the same id, the vault refuses the second write,
// and the record was never the thing holding the guarantee.
func TestReservationSweepsCrashedPendingRecords(t *testing.T) {
	t.Run("on insert", func(t *testing.T) {
		store, clock, root := testStore(t)
		crashed := storeIdentity("key.crashed", "payload")
		_, claim, err := store.Reserve(crashed)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		claim.Abandon()

		*clock = testNow.Add(PendingSweepAfter + time.Second)
		fresh := storeIdentity("key.fresh", "payload")
		if _, _, err := store.Reserve(fresh); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if reservationExists(root, testStoreDevice, "key.crashed") {
			t.Fatal("the crashed pending record survived an insert past the takeover window")
		}
	})

	t.Run("on open", func(t *testing.T) {
		store, clock, root := testStore(t)
		crashed := storeIdentity("key.crashed", "payload")
		_, claim, err := store.Reserve(crashed)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		claim.Abandon()

		*clock = testNow.Add(PendingSweepAfter + time.Second)
		reopenStore(root, func() time.Time { return *clock })
		if reservationExists(root, testStoreDevice, "key.crashed") {
			t.Fatal("opening the store did not sweep a crashed pending record")
		}
	})

	t.Run("a reclaimable pending record survives the takeover window", func(t *testing.T) {
		store, clock, root := testStore(t)
		live := storeIdentity("key.live", "payload")
		_, claim, err := store.Reserve(live)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		claim.Abandon()

		// Past the takeover window but inside the sweep window: this is the
		// interval a crashed capture is RECOVERED in, so the record has to
		// survive it or the recovery path is unreachable.
		*clock = testNow.Add(ReservationTakeover + time.Second)
		reopenStore(root, func() time.Time { return *clock })
		if !reservationExists(root, testStoreDevice, "key.live") {
			t.Fatal("a reclaimable pending record was swept before anything could reclaim it")
		}
	})
}

// TestReservationExpires proves the TTL is real, on both the occasions it is
// enforced.
//
// Expiry is checked where it costs nothing: when the record itself is touched,
// and when the store takes its census on open. It is deliberately NOT a periodic
// walk — a fresh reservation must not pay for every reservation before it, which
// is what the round two review measured.
func TestReservationExpires(t *testing.T) {
	t.Run("a touched record is collected and not answered", func(t *testing.T) {
		store, clock, root := testStore(t)
		id := storeIdentity("key.old", "old")

		_, claim, err := store.Reserve(id)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		receipt, body := storeReceipt(t, store, id, ReceiptApplied)
		if err := claim.Settle(receipt, body); err != nil {
			t.Fatalf("settle: %v", err)
		}

		*clock = testNow.Add(ReservationTTL + time.Hour)
		replay, again, err := store.Reserve(id)
		if err != nil {
			t.Fatalf("reserve past the TTL: %v", err)
		}
		if again == nil {
			t.Fatalf("an expired reservation still answered a replay: %s", replay)
		}
		// The stale record was replaced rather than left to answer, and the store
		// counts one reservation rather than two.
		record := reservationRecordOn(t, root, testStoreDevice, "key.old")
		if record.State != reservationPending {
			t.Fatalf("state = %q, want the fresh pending record", record.State)
		}
		if got := storeTotal(store); got != 1 {
			t.Fatalf("the census counts %d reservations, want 1", got)
		}
	})

	t.Run("opening the store collects them", func(t *testing.T) {
		store, clock, root := testStore(t)
		id := storeIdentity("key.old", "old")

		_, claim, err := store.Reserve(id)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		receipt, body := storeReceipt(t, store, id, ReceiptApplied)
		if err := claim.Settle(receipt, body); err != nil {
			t.Fatalf("settle: %v", err)
		}

		*clock = testNow.Add(ReservationTTL + time.Hour)
		reopened := reopenStore(root, func() time.Time { return *clock })
		if reservationExists(root, testStoreDevice, "key.old") {
			t.Fatal("opening the store did not collect an expired reservation")
		}
		if got := storeTotal(reopened); got != 0 {
			t.Fatalf("the reopened census counts %d reservations, want 0", got)
		}
	})
}

// TestReservationTrimsTheOldestSettledPastTheTotalCap. The total cap is the
// second bound, and it drops idempotency memory rather than live claims.
func TestReservationTrimsTheOldestSettledPastTheTotalCap(t *testing.T) {
	store, clock, root := testStore(t)

	for i := 0; i <= MaxReservations; i++ {
		*clock = testNow.Add(time.Duration(i) * time.Second)
		id := storeIdentity(fmt.Sprintf("key.%04d", i), "payload")
		_, claim, err := store.Reserve(id)
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		receipt, body := storeReceipt(t, store, id, ReceiptApplied)
		if err := claim.Settle(receipt, body); err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, testStoreDevice))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) > MaxReservations {
		t.Fatalf("the store holds %d reservations, over the %d cap", len(entries), MaxReservations)
	}
}

// ---------------------------------------------------------------------------
// Durability and hostile files
// ---------------------------------------------------------------------------

// TestReservationFilesAreNotWorldReadable holds the store to the same
// filesystem discipline as the device registry: 0700 directories, 0600 files.
//
// The records carry no bearer secret, but they name every capture a phone made
// and every memory those captures created, and that is not a list another local
// account is entitled to read.
func TestReservationFilesAreNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes are not enforced on Windows; exclusion there comes from the profile directory's ACL")
	}
	store, _, root := testStore(t)
	if _, _, err := store.Reserve(storeIdentity("key.one", "x")); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	for _, tc := range []struct {
		path string
		mode os.FileMode
	}{
		{root, secretDirMode},
		{filepath.Join(root, testStoreDevice), secretDirMode},
		{store.path(testStoreDevice, "key.one"), secretFileMode},
	} {
		info, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.path, err)
		}
		if info.Mode().Perm() != tc.mode {
			t.Fatalf("%s is mode %04o, want %04o", tc.path, info.Mode().Perm(), tc.mode)
		}
	}
}

// TestReservationRefusesAnOversizeRecord proves the read-path bound. A capture
// reads a reservation on every request, so an unbounded file is a per-request
// memory amplifier before it is anything else.
func TestReservationRefusesAnOversizeRecord(t *testing.T) {
	store, _, _ := testStore(t)
	if _, _, err := store.Reserve(storeIdentity("key.one", "x")); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	path := store.path(testStoreDevice, "key.one")
	if err := os.WriteFile(path, make([]byte, maxReservationBytes+1), secretFileMode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readReservation(path); err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("an oversize reservation was read: %v", err)
	}
}

// TestReservationRefusesAForeignDocument. The store's directory is on disk and
// another program can put a file in it; a document that is not a reservation
// must not be read as one.
func TestReservationRefusesAForeignDocument(t *testing.T) {
	store, _, root := testStore(t)
	if err := os.MkdirAll(filepath.Join(root, testStoreDevice), secretDirMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := store.path(testStoreDevice, "key.one")
	if err := os.WriteFile(path, []byte(`{"schema":"mora.companion.receipt","schema_version":1}`), secretFileMode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readReservation(path); err == nil {
		t.Fatal("a document with another schema was read as a reservation")
	}
}

// TestReservationKeyIsNotAPath. Reservation filenames are digests of the key, so
// a key can never steer the write out of the store's own directory even if the
// contract's character set widens later.
func TestReservationKeyIsNotAPath(t *testing.T) {
	store, _, root := testStore(t)
	for _, key := range []string{"a", "a.b:c-d_e", strings.Repeat("k", MaxIdempotencyKeyBytes)} {
		path := store.path(testStoreDevice, key)
		if filepath.Dir(path) != filepath.Join(root, testStoreDevice) {
			t.Fatalf("key %q produced %q, outside the device directory", key, path)
		}
		if strings.Contains(filepath.Base(path), key) && len(key) > 4 {
			t.Fatalf("the filename carries the key itself: %q", filepath.Base(path))
		}
	}
}

// TestReservationValidatesWhatItIsAsked refuses a malformed device id, key,
// digest or memory id at the boundary rather than deriving a path from it.
func TestReservationValidatesWhatItIsAsked(t *testing.T) {
	store, _, _ := testStore(t)
	valid := storeIdentity("key.one", "x")
	for _, tc := range []struct {
		name    string
		breakIt func(*CaptureIdentity)
	}{
		{"no device", func(id *CaptureIdentity) { id.DeviceID = "" }},
		{"device is not a device id", func(id *CaptureIdentity) { id.DeviceID = "phone" }},
		{"no key", func(id *CaptureIdentity) { id.Key = "" }},
		{"key carries prose", func(id *CaptureIdentity) { id.Key = "remember the wifi code" }},
		{"key is unbounded", func(id *CaptureIdentity) { id.Key = strings.Repeat("k", MaxIdempotencyKeyBytes+1) }},
		{"identity is not a digest", func(id *CaptureIdentity) { id.Identity = "x" }},
		{"fingerprint is not a digest", func(id *CaptureIdentity) { id.Fingerprint = "x" }},
		{"no memory id", func(id *CaptureIdentity) { id.MemoryID = "" }},
		{"memory id is not a memory id", func(id *CaptureIdentity) { id.MemoryID = "rcp_20260903_120000_aaaaaaaa" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broken := valid
			tc.breakIt(&broken)
			if _, _, err := store.Reserve(broken); err == nil {
				t.Fatal("the store accepted it")
			}
		})
	}
}

// TestReservationIDsAreOpaqueAndBounded. A receipt id is a published identifier,
// so it has to satisfy the contract's own rule wherever it is minted.
func TestReservationIDsAreOpaqueAndBounded(t *testing.T) {
	store, _, _ := testStore(t)
	for _, prefix := range []string{PrefixReceipt, PrefixRequest} {
		id, err := store.NewID(prefix)
		if err != nil {
			t.Fatalf("mint %s: %v", prefix, err)
		}
		if err := validateID("id", prefix, id); err != nil {
			t.Fatalf("%q: %v", id, err)
		}
	}
	first, _ := store.NewID(PrefixReceipt)
	second, _ := store.NewID(PrefixReceipt)
	if first == second {
		t.Fatalf("two identifiers minted in the same second are identical: %q", first)
	}
}

// TestReservationIDMintingSurfacesAnEntropyFailure. A silent fallback would mint
// a predictable receipt id, and the ids end up in an audit trail.
func TestReservationIDMintingSurfacesAnEntropyFailure(t *testing.T) {
	store := NewReservationStore(t.TempDir(),
		WithReservationClock(func() time.Time { return testNow }),
		WithReservationEntropy(failingReader{}))
	if _, err := store.NewID(PrefixReceipt); err == nil {
		t.Fatal("a dead entropy source still minted an identifier")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// ---------------------------------------------------------------------------
// Scan cost
// ---------------------------------------------------------------------------

// countingReadDir wraps os.ReadDir and counts the walks.
func countingReadDir(n *int) func(string) ([]os.DirEntry, error) {
	return func(name string) ([]os.DirEntry, error) {
		*n++
		return os.ReadDir(name)
	}
}

// TestReservationFreshPathWalksNoDirectory is the cost gate.
//
// Round two answered the in-flight bound by reading every record in the store,
// and then trimmed by reading them all again — so the Nth capture of a session
// paid for the N-1 before it, and a store near its cap turned one phone request
// into a thousand file reads. The census makes the bound arithmetic: one walk
// when the store opens, and none per capture.
//
// The assertion is on the WALK rather than on a stopwatch, because a timing
// assertion is a flake and a walk count is a fact.
func TestReservationFreshPathWalksNoDirectory(t *testing.T) {
	root := t.TempDir()
	now := testNow
	walks := 0
	store := NewReservationStore(root, WithReservationClock(func() time.Time { return now }))
	// The census has already run against an empty directory. Count from here.
	store.readDir = countingReadDir(&walks)

	for i := 0; i < 32; i++ {
		id := storeIdentity(fmt.Sprintf("key.%03d", i), "payload")
		_, claim, err := store.Reserve(id)
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		receipt, body := storeReceipt(t, store, id, ReceiptApplied)
		if err := claim.Settle(receipt, body); err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
	}
	if walks != 0 {
		t.Fatalf("32 captures walked the store %d times, want 0", walks)
	}
	// A replay is the same: it reads ONE record, the one it is about.
	if _, _, err := store.Reserve(storeIdentity("key.000", "payload")); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if walks != 0 {
		t.Fatalf("a replay walked the store %d times, want 0", walks)
	}
	// And the census it kept instead is the truth: 32 records, none of them a
	// live claim.
	if got := storeTotal(store); got != 32 {
		t.Fatalf("the census counts %d reservations, want 32", got)
	}
	if got := storePending(store); got != 0 {
		t.Fatalf("the census counts %d live claims, want 0", got)
	}
}

// TestReservationOpeningWalksOnce pins the other half: the census is seeded by
// exactly one pass over the store, not by one per device or one per record.
func TestReservationOpeningWalksOnce(t *testing.T) {
	store, _, root := testStore(t)
	for i := 0; i < 4; i++ {
		id := storeIdentity(fmt.Sprintf("key.%03d", i), "payload")
		_, claim, err := store.Reserve(id)
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		receipt, body := storeReceipt(t, store, id, ReceiptApplied)
		if err := claim.Settle(receipt, body); err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
	}

	walks := 0
	reopened := NewReservationStore("", WithReservationClock(func() time.Time { return testNow }))
	reopened.root = root
	reopened.readDir = countingReadDir(&walks)
	reopened.census(testNow)

	// One walk for the store's root, one for the single device directory inside
	// it. What matters is that it does not grow with the number of RECORDS.
	if walks != 2 {
		t.Fatalf("the opening census walked %d directories, want 2 (the root and one device)", walks)
	}
	if got := storeTotal(reopened); got != 4 {
		t.Fatalf("the census counts %d reservations, want 4", got)
	}
}

// TestReservationSweepWalksThePendingSetNotTheStore. The sweep has to be able to
// run on the hot path, so it reads the in-memory claim set rather than the
// directory; the only files it touches are the ones it deletes.
func TestReservationSweepWalksThePendingSetNotTheStore(t *testing.T) {
	store, clock, root := testStore(t)

	// One crashed claim, and a settled record beside it that the sweep must not
	// need to read in order to leave alone.
	settledID := storeIdentity("key.settled", "payload")
	_, settledClaim, err := store.Reserve(settledID)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	receipt, body := storeReceipt(t, store, settledID, ReceiptApplied)
	if err := settledClaim.Settle(receipt, body); err != nil {
		t.Fatalf("settle: %v", err)
	}
	crashed := storeIdentity("key.crashed", "payload")
	_, crashedClaim, err := store.Reserve(crashed)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	crashedClaim.Abandon()

	walks := 0
	store.readDir = countingReadDir(&walks)
	*clock = testNow.Add(PendingSweepAfter + time.Second)
	if live := store.sweep(*clock); live != 0 {
		t.Fatalf("the sweep reports %d live claims, want 0", live)
	}
	if walks != 0 {
		t.Fatalf("the sweep walked the store %d times, want 0", walks)
	}
	if reservationExists(root, testStoreDevice, "key.crashed") {
		t.Fatal("the sweep left the crashed claim on disk")
	}
	if !reservationExists(root, testStoreDevice, "key.settled") {
		t.Fatal("the sweep collected a settled record")
	}
	if got := storeTotal(store); got != 1 {
		t.Fatalf("the census counts %d reservations after the sweep, want 1", got)
	}
}

// TestReservationCensusTracksSettleAndSweep. The census is the bound, so a count
// that drifted from the directory would be a bound that refused good captures or
// admitted bad ones. It is checked against the files at each step.
func TestReservationCensusTracksSettleAndSweep(t *testing.T) {
	store, clock, root := testStore(t)

	id := storeIdentity("key.one", "payload")
	_, claim, err := store.Reserve(id)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if got, want := storePending(store), 1; got != want {
		t.Fatalf("after reserve: %d live claims, want %d", got, want)
	}
	if got, want := storeTotal(store), 1; got != want {
		t.Fatalf("after reserve: %d reservations, want %d", got, want)
	}

	receipt, body := storeReceipt(t, store, id, ReceiptApplied)
	if err := claim.Settle(receipt, body); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := storePending(store); got != 0 {
		t.Fatalf("a settled record is still counted as a live claim: %d", got)
	}
	if got := storeTotal(store); got != 1 {
		t.Fatalf("a settled record left the census: %d", got)
	}

	// A crashed claim, swept.
	crashed := storeIdentity("key.two", "payload")
	_, crashedClaim, err := store.Reserve(crashed)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	crashedClaim.Abandon()
	*clock = testNow.Add(PendingSweepAfter + time.Second)
	store.sweep(*clock)
	if got := storeTotal(store); got != 1 {
		t.Fatalf("after the sweep the census counts %d, want 1", got)
	}

	// And the census agrees with the disk.
	entries, err := os.ReadDir(filepath.Join(root, testStoreDevice))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != storeTotal(store) {
		t.Fatalf("the census counts %d reservations, the directory holds %d", storeTotal(store), len(entries))
	}
}
