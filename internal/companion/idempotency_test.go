package companion

import (
	"errors"
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
	return s
}

// reservationStateOn reads the stored state of one key straight off the disk.
// Asserting on the FILE rather than on an API is the point in the crash tests:
// what survives a process is the file.
func reservationStateOn(t *testing.T, root string, c Capture) reservationState {
	t.Helper()
	store := &ReservationStore{root: root, now: time.Now}
	record, err := readReservation(store.path(c.DeviceID, c.IdempotencyKey))
	if err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	return record.State
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

// storeReceipt builds a terminal receipt for a reserved key. The store validates
// what it is asked to settle, so a helper that produced an invalid one would
// fail at Settle rather than at the assertion under test.
func storeReceipt(t *testing.T, store *ReservationStore, deviceID, key, fingerprint string, state ReceiptState) Receipt {
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
	r.IdempotencyKey = key
	r.DeviceID = deviceID
	r.State = state
	r.PayloadFingerprint = fingerprint
	r.Policy = PolicyOpen
	r.MemoryID = "mem_00000001"
	r.ReceivedAt = reservationStamp(store.now())
	r.SettledAt = reservationStamp(store.now())
	if err := r.Validate(); err != nil {
		t.Fatalf("the helper built an invalid receipt: %v", err)
	}
	return r
}

// ---------------------------------------------------------------------------
// The reservation lifecycle
// ---------------------------------------------------------------------------

// TestReservationIsDurableBeforeTheWrite is the ordering claim, asserted against
// the filesystem.
//
// The claim is handed back only after the reservation is on disk, so a caller
// holding a claim is holding something that survives the process. Reserving
// AFTER the write would mean a crash in between leaves a memory in the vault
// that no key points at, and the retry writes a second one.
func TestReservationIsDurableBeforeTheWrite(t *testing.T) {
	store, _, root := testStore(t)
	fingerprint := Fingerprint("a note")

	_, claim, err := store.Reserve(testStoreDevice, "key.one", fingerprint)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if claim == nil {
		t.Fatal("a fresh key returned no claim")
	}
	// Nothing has been settled and no vault write has happened, and the record
	// is already there.
	record, err := readReservation(store.path(testStoreDevice, "key.one"))
	if err != nil {
		t.Fatalf("the reservation is not on disk: %v", err)
	}
	if record.State != reservationPending {
		t.Fatalf("state = %q, want pending", record.State)
	}
	if record.PayloadFingerprint != fingerprint {
		t.Fatalf("the record does not carry the payload fingerprint")
	}
	if record.Receipt != nil {
		t.Fatal("a pending reservation carries a receipt")
	}
	if !strings.HasPrefix(store.path(testStoreDevice, "key.one"), filepath.Join(root, testStoreDevice)) {
		t.Fatalf("the reservation is not under the device's own directory")
	}
}

// TestReservationReplayReturnsTheStoredReceipt proves the replay answer comes
// out of storage rather than being rebuilt. A rebuilt receipt would carry a new
// receipt id and a new settled_at, which is a different document for the same
// event.
func TestReservationReplayReturnsTheStoredReceipt(t *testing.T) {
	store, _, _ := testStore(t)
	fingerprint := Fingerprint("a note")

	_, claim, err := store.Reserve(testStoreDevice, "key.one", fingerprint)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	want := storeReceipt(t, store, testStoreDevice, "key.one", fingerprint, ReceiptApplied)
	if err := claim.Settle(want); err != nil {
		t.Fatalf("settle: %v", err)
	}

	got, again, err := store.Reserve(testStoreDevice, "key.one", fingerprint)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if again != nil {
		t.Fatal("a settled key handed out a second claim")
	}
	if got != want {
		t.Fatalf("replay returned %+v, want %+v", got, want)
	}
	first, err := Marshal(&want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := Marshal(&got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("the replay is not byte-identical\n%s\n%s", first, second)
	}
}

// TestReservationSameKeyDifferentPayloadIsAConflict. The first payload keeps the
// key whether it settled or is still pending: a fingerprint check that ran only
// against settled records would let a second payload take over a crashed
// reservation belonging to the first.
func TestReservationSameKeyDifferentPayloadIsAConflict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		settle bool
	}{
		{"settled", true},
		{"still pending", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _, _ := testStore(t)
			first := Fingerprint("the original")

			_, claim, err := store.Reserve(testStoreDevice, "key.one", first)
			if err != nil {
				t.Fatalf("reserve: %v", err)
			}
			if tc.settle {
				if err := claim.Settle(storeReceipt(t, store, testStoreDevice, "key.one", first, ReceiptApplied)); err != nil {
					t.Fatalf("settle: %v", err)
				}
			} else {
				claim.Abandon()
			}

			_, _, err = store.Reserve(testStoreDevice, "key.one", Fingerprint("something else"))
			if !errors.Is(err, ErrIdempotencyConflict) {
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

	mine := Fingerprint("my note")
	_, claim, err := store.Reserve(testStoreDevice, "shared.key", mine)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := claim.Settle(storeReceipt(t, store, testStoreDevice, "shared.key", mine, ReceiptApplied)); err != nil {
		t.Fatalf("settle: %v", err)
	}

	// The same key, from another device, with a DIFFERENT payload. If the key
	// space were shared this would be a conflict; scoped per device it is simply
	// a fresh reservation.
	stored, second, err := store.Reserve(other, "shared.key", Fingerprint("their note"))
	if err != nil {
		t.Fatalf("the second device's reservation failed: %v", err)
	}
	if second == nil {
		t.Fatalf("the second device was handed the first device's receipt: %+v", stored)
	}
	// And the same key with the SAME payload does not return the other device's
	// receipt either.
	stored, third, err := store.Reserve(other, "shared.key", mine)
	if !errors.Is(err, ErrIdempotencyConflict) && third == nil && stored.DeviceID == testStoreDevice {
		t.Fatalf("device %s read device %s's receipt", other, testStoreDevice)
	}
}

// TestReservationConcurrentDuplicatesElectOneWinner drives N goroutines at one
// key against the store directly, without the listener's one-at-a-time work
// budget in the way.
//
// Exactly one wins the claim. Every other caller either waits for it and reads
// the settled receipt, or is told the capture is in flight. None of them gets a
// second claim, because a second claim is a second vault write.
func TestReservationConcurrentDuplicatesElectOneWinner(t *testing.T) {
	store, _, _ := testStore(t)
	fingerprint := Fingerprint("concurrent")

	const callers = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claims  int
		replays []Receipt
		busy    int
	)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			stored, claim, err := store.Reserve(testStoreDevice, "key.one", fingerprint)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrCaptureInFlight):
				busy++
			case err != nil:
				t.Errorf("reserve: %v", err)
			case claim != nil:
				claims++
				// The winner settles, which is what wakes everybody waiting.
				if serr := claim.Settle(storeReceipt(t, store, testStoreDevice, "key.one", fingerprint, ReceiptApplied)); serr != nil {
					t.Errorf("settle: %v", serr)
				}
			default:
				replays = append(replays, stored)
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
	// Every caller that got an answer got the SAME answer.
	for i, r := range replays {
		if i > 0 && r != replays[0] {
			t.Fatalf("two waiters got different receipts:\n%+v\n%+v", replays[0], r)
		}
	}
}

// TestReservationSurvivesTheStoreValue reopens the directory with a second
// store, which is what a restarted process does. The in-flight set is memory;
// the reservation is a file.
func TestReservationSurvivesTheStoreValue(t *testing.T) {
	store, clock, root := testStore(t)
	fingerprint := Fingerprint("durable")

	_, claim, err := store.Reserve(testStoreDevice, "key.one", fingerprint)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	want := storeReceipt(t, store, testStoreDevice, "key.one", fingerprint, ReceiptApplied)
	if err := claim.Settle(want); err != nil {
		t.Fatalf("settle: %v", err)
	}

	reopened := NewReservationStore(filepath.Dir(filepath.Dir(root)), WithReservationClock(func() time.Time { return *clock }))
	got, again, err := reopened.Reserve(testStoreDevice, "key.one", fingerprint)
	if err != nil {
		t.Fatalf("reserve after reopen: %v", err)
	}
	if again != nil {
		t.Fatal("a reopened store handed out a second claim for a settled key")
	}
	if got != want {
		t.Fatalf("a reopened store returned %+v, want %+v", got, want)
	}
}

// TestReservationTakeoverWaitsForTheWindow is the crash-recovery rule, both
// ways round.
//
// Inside the window a pending reservation belongs to a caller that may still be
// inside its vault write, and taking it would MAKE the duplicate. Past the
// window it is a crash, and refusing forever would wedge the key.
func TestReservationTakeoverWaitsForTheWindow(t *testing.T) {
	store, clock, root := testStore(t)
	fingerprint := Fingerprint("crashed")

	_, claim, err := store.Reserve(testStoreDevice, "key.one", fingerprint)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Abandon models the crash: the in-process hold is gone and the PENDING
	// record stays on disk, which is exactly the state a killed process leaves.
	claim.Abandon()

	*clock = testNow.Add(ReservationTakeover - time.Second)
	if _, _, err := store.Reserve(testStoreDevice, "key.one", fingerprint); !errors.Is(err, ErrCaptureInFlight) {
		t.Fatalf("inside the window err = %v, want ErrCaptureInFlight", err)
	}

	*clock = testNow.Add(ReservationTakeover)
	_, recovered, err := store.Reserve(testStoreDevice, "key.one", fingerprint)
	if err != nil {
		t.Fatalf("past the window: %v", err)
	}
	if recovered == nil {
		t.Fatal("past the takeover window a crashed reservation was not recoverable")
	}
	// The recovered claim rewrote the record, so its stamp is the new one and the
	// key is not permanently in the past.
	record, err := readReservation(filepath.Join(root, testStoreDevice, strings.TrimPrefix(Fingerprint("key.one"), "sha256:")+".json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if record.ReservedAt != reservationStamp(*clock) {
		t.Fatalf("the recovered reservation is stamped %q, want %q", record.ReservedAt, reservationStamp(*clock))
	}
}

// TestReservationSettleRefusesAForeignReceipt. The store answers future replays
// with what it settled, so a receipt for a different key, device or payload
// would make it hand somebody else's outcome to the next caller.
func TestReservationSettleRefusesAForeignReceipt(t *testing.T) {
	store, _, _ := testStore(t)
	fingerprint := Fingerprint("mine")

	_, claim, err := store.Reserve(testStoreDevice, "key.one", fingerprint)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	foreign := storeReceipt(t, store, testStoreDevice, "key.two", fingerprint, ReceiptApplied)
	if err := claim.Settle(foreign); err == nil {
		t.Fatal("Settle accepted a receipt for another key")
	}
	// And the record is untouched: a refused settle must not half-write.
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
	fingerprint := Fingerprint("mine")

	_, claim, err := store.Reserve(testStoreDevice, "key.one", fingerprint)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	broken := storeReceipt(t, store, testStoreDevice, "key.one", fingerprint, ReceiptApplied)
	broken.MemoryID = "" // an applied receipt that names no memory
	if err := claim.Settle(broken); err == nil {
		t.Fatal("Settle stored a receipt the contract forbids")
	}
}

// ---------------------------------------------------------------------------
// Bounds and durability
// ---------------------------------------------------------------------------

// TestReservationStoreIsBounded proves the store cannot be grown without limit
// by a device that keeps talking. Expired entries go first; past the cap, the
// oldest SETTLED entries go, because a settled entry is pure idempotency memory
// while a pending one is what stops a duplicate right now.
func TestReservationStoreIsBounded(t *testing.T) {
	store, clock, root := testStore(t)

	// One past the cap, each settled, each a second apart so "oldest" is real.
	for i := 0; i <= MaxReservations; i++ {
		*clock = testNow.Add(time.Duration(i) * time.Second)
		key := "key." + strings.TrimPrefix(Fingerprint(string(rune(i))), "sha256:")[:20]
		fingerprint := Fingerprint(key)
		_, claim, err := store.Reserve(testStoreDevice, key, fingerprint)
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		if err := claim.Settle(storeReceipt(t, store, testStoreDevice, key, fingerprint, ReceiptApplied)); err != nil {
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

// TestReservationExpires proves the TTL is real. A key answerable forever is a
// directory that grows forever.
func TestReservationExpires(t *testing.T) {
	store, clock, _ := testStore(t)
	fingerprint := Fingerprint("old")

	_, claim, err := store.Reserve(testStoreDevice, "key.old", fingerprint)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := claim.Settle(storeReceipt(t, store, testStoreDevice, "key.old", fingerprint, ReceiptApplied)); err != nil {
		t.Fatalf("settle: %v", err)
	}

	// Past the TTL, a reservation for ANOTHER key prunes the expired one.
	*clock = testNow.Add(ReservationTTL + time.Hour)
	if _, _, err := store.Reserve(testStoreDevice, "key.new", Fingerprint("new")); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := readReservation(store.path(testStoreDevice, "key.old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the expired reservation is still readable: %v", err)
	}
}

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
	if _, _, err := store.Reserve(testStoreDevice, "key.one", Fingerprint("x")); err != nil {
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
	if _, _, err := store.Reserve(testStoreDevice, "key.one", Fingerprint("x")); err != nil {
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

// TestReservationValidatesWhatItIsAsked refuses a malformed device id, key or
// fingerprint at the boundary rather than deriving a path from it.
func TestReservationValidatesWhatItIsAsked(t *testing.T) {
	store, _, _ := testStore(t)
	for _, tc := range []struct {
		name                     string
		device, key, fingerprint string
	}{
		{"no device", "", "key.one", Fingerprint("x")},
		{"device is not a device id", "phone", "key.one", Fingerprint("x")},
		{"no key", testStoreDevice, "", Fingerprint("x")},
		{"key carries prose", testStoreDevice, "remember the wifi code", Fingerprint("x")},
		{"key is unbounded", testStoreDevice, strings.Repeat("k", MaxIdempotencyKeyBytes+1), Fingerprint("x")},
		{"fingerprint is not a digest", testStoreDevice, "key.one", "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := store.Reserve(tc.device, tc.key, tc.fingerprint); err == nil {
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
	// Two mints inside the same second differ, or two captures in one second
	// would share a receipt id.
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
