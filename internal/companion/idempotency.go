package companion

// idempotency.go is the durable reservation store behind governed capture
// (graph node N21).
//
// A phone on a train retries. It retries because the socket died, because iOS
// suspended the app mid-request, because the user tapped twice. Every one of
// those retries carries the SAME idempotency key, and the only acceptable
// outcome is one memory in the vault and one receipt — byte for byte the bytes
// the first attempt returned.
//
// # The ordering, and what makes it exactly-once
//
//	reserve (durable)  ->  vault write at a PINNED id  ->  settle (durable)
//
// The reservation is fsynced and renamed into place before the kernel is asked
// to write anything, and it carries the vault id the write will use. That id is
// DERIVED — from the device, the idempotency key and the payload — so it is the
// same id on every attempt, and the kernel's create-exclusive publish is what
// decides the race: whoever gets the link wins, and every later attempt is told
// the memory already exists and settles `applied` without writing again.
//
// That is the difference from round one, which reserved first and then hoped.
// Reserving first alone only closes the window BEFORE the write; a process
// killed between the vault publication and the settle write still left a pending
// reservation over a memory that existed, and the retry duplicated it. Pinning
// the id closes the window on the other side too, because the second write
// cannot land.
//
// # Bounded, because a phone can talk
//
// Every reservation is a file a device caused to exist, so the store is bounded
// in four directions at once:
//
//   - MaxPendingReservations caps how many captures may be IN FLIGHT. Past it a
//     new key is refused with ErrTooManyPending and no file is created, so a
//     device that can make the kernel fail cannot turn that into unbounded disk.
//   - Pending records expire after PendingSweepAfter and are swept on open and on
//     insert. An expired pending record is a crashed attempt nobody came back
//     for, and the pinned id means sweeping it loses nothing: the retry derives
//     the same id and the create-exclusive publish still refuses to write twice.
//     The sweep walks the in-memory pending SET, so it costs nothing per capture.
//   - MaxReservations caps the total. Over it the oldest SETTLED records go,
//     because a settled record is idempotency memory while a pending one is a
//     claim somebody may still be acting on.
//   - maxReservationBytes caps one record on the read path, so a hostile file is
//     refused rather than allocated.
//
// # The key space is per device
//
// Reservations live under <state>/companion/captures/<device_id>/. Two devices
// that happen to choose the same key never collide, and — the reason it is not
// merely tidy — no device can fetch another device's receipt by guessing its
// idempotency key. A receipt names a memory; a receipt lookup that crossed
// devices would be a read oracle over somebody else's vault writes.
//
// Like registry.go, this file writes out its own atomic write, its own lock and
// its own directory hardening rather than importing internal/atomicio, because
// the package is a leaf (TestPackageIsALeaf) and must compile with the standard
// library alone.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Bounds on the reservation store. None of them is a capacity target; each is
// the point past which a device is costing the Mac more than a note is worth.
const (
	// MaxReservations caps how many reservations exist across every device.
	MaxReservations = 512
	// MaxPendingReservations caps how many captures may be in flight at once.
	//
	// It is much smaller than MaxReservations because a pending record is a
	// claim, not a memory: the listener runs one kernel call at a time, so more
	// than a handful of live claims means attempts are failing rather than
	// finishing. Refusing at this line is what makes the store's bound HARD —
	// without it, a kernel that fails every request turns each fresh key into a
	// file that nothing ever settles or evicts.
	MaxPendingReservations = 64
	// ReservationTTL is how long a settled receipt stays answerable. A retry
	// after this gets a fresh receipt, which is why it is days rather than
	// minutes: a phone that was offline over a weekend is a normal phone.
	ReservationTTL = 7 * 24 * time.Hour
	// ReservationTakeover is how long a pending reservation is treated as
	// somebody else's live work rather than as a crash to recover from. Past it
	// the record is sweepable and the key is reclaimable.
	//
	// It is longer than the listener's KernelTimeout on purpose. Shorter, and a
	// second caller could reclaim a key whose holder is still inside its vault
	// write. The pinned id makes that safe rather than catastrophic, but "safe"
	// is not a reason to make it likely.
	ReservationTakeover = 3 * KernelTimeout
	// PendingSweepAfter is when a crashed pending record is COLLECTED, and it is
	// deliberately later than ReservationTakeover.
	//
	// The two thresholds do different jobs and collapsing them breaks the first.
	// Between them a retry RECLAIMS the record, reads the vault id the crashed
	// attempt pinned, and asks the kernel whether that memory is already
	// published — which is how a capture killed between its write and its receipt
	// finishes without writing again. Sweep at the takeover line instead and the
	// record is gone before any retry can read it, so the recovery path is
	// unreachable and every crashed capture pays for a second write attempt.
	//
	// Past this line the record goes, and correctness does not depend on it: the
	// memory id is DERIVED, so a retry re-derives the same id and the kernel's
	// create-exclusive publish still refuses the second write. What is lost is
	// only the shortcut.
	PendingSweepAfter = 4 * ReservationTakeover
	// maxReservationBytes bounds ONE reservation file on the read path. A
	// receipt and its response bytes are under 4 KiB together; this leaves room
	// and refuses anything that is not a reservation.
	maxReservationBytes = 64 << 10
	// reservationWait bounds how long a concurrent duplicate waits for the
	// holder of the same key to settle. Past it the caller is told the capture
	// is in flight and retries, which is a smaller lie than a socket held open
	// indefinitely.
	reservationWait = 10 * time.Second
	// reservationIDEntropyBytes sizes the random tail of a receipt or request
	// identifier.
	reservationIDEntropyBytes = 4
)

// schemaCaptureReservation names the stored reservation. It is deliberately NOT
// one of the published wire schemas — no device ever receives one — and it is
// versioned only so a later reader can tell what it is looking at.
const schemaCaptureReservation = "mora.companion.capture.reservation"

// Reservation errors. They are values so the capture path can branch on the
// condition and map it onto the published reject_reason vocabulary without
// matching prose.
var (
	// ErrIdempotencyConflict reports that the key was already used for a
	// DIFFERENT capture. It is never a silent overwrite and never a silent
	// replay: the first capture keeps the key.
	ErrIdempotencyConflict = errors.New("companion: that idempotency key already covers a different capture")
	// ErrCaptureInFlight reports that another caller holds this key right now.
	// It is transient: the retry that follows finds the settled receipt.
	ErrCaptureInFlight = errors.New("companion: that idempotency key is being processed")
	// ErrTooManyPending reports that too many captures are already in flight. It
	// is the store's HARD bound, and it is refused before any file is created.
	ErrTooManyPending = errors.New("companion: too many captures are already in flight")
	// ErrPublishedIntegrity reports that the published store's own bookkeeping
	// contradicts itself — a pointer naming one memory and the record behind it
	// naming another. It is a vault-integrity failure, not a client condition,
	// and it settles as a rejection rather than being retried forever.
	ErrPublishedIntegrity = errors.New("companion: the published store's bookkeeping disagrees with itself")
	// ErrNoClaim reports a Settle against a claim that was already released.
	ErrNoClaim = errors.New("companion: this reservation is no longer held")
)

// reservationState is the two-state lifecycle of a stored reservation.
type reservationState string

const (
	// reservationPending means the key is claimed and the capture has not
	// settled. A pending record older than ReservationTakeover is a crashed
	// attempt: reclaimable, and sweepable.
	reservationPending reservationState = "pending"
	// reservationSettled means the capture reached a terminal receipt, which is
	// stored with the exact bytes that answered the first attempt.
	reservationSettled reservationState = "settled"
)

// CaptureIdentity is everything the store needs to answer "is this the same
// capture?" and "where does it publish?".
//
// Identity and Fingerprint are deliberately two fields. Fingerprint is the wire
// `payload_fingerprint`, which N02 defines as SHA-256 over the capture TEXT and
// which the receipt carries; Identity covers every field that changes what gets
// written, so the same key with a different scope is a conflict rather than a
// replay of somebody else's placement decision.
type CaptureIdentity struct {
	DeviceID    string
	Key         string
	Identity    string
	Fingerprint string
	// MemoryID is the vault id this capture publishes under. It is derived
	// before the reservation is written, and it is what makes the kernel's
	// create-exclusive publish an exactly-once primitive.
	MemoryID string
}

func (id CaptureIdentity) validate() error {
	if err := validateID("device_id", PrefixDevice, id.DeviceID); err != nil {
		return err
	}
	if err := validateIdempotencyKey("idempotency_key", id.Key); err != nil {
		return err
	}
	if err := validateFingerprint("capture_identity", id.Identity); err != nil {
		return err
	}
	if err := validateFingerprint("payload_fingerprint", id.Fingerprint); err != nil {
		return err
	}
	return validateID("memory_id", PrefixMemory, id.MemoryID)
}

// reservationRecord is the stored form of one reservation.
//
// It carries digests rather than the text: the store answers "same key, same
// capture?" and has no business holding a word the user wrote. Response holds
// the exact bytes the first attempt returned, so a replay is byte-identical on
// the wire rather than re-marshalled and merely equal in structure.
type reservationRecord struct {
	Header
	IdempotencyKey     string           `json:"idempotency_key"`
	DeviceID           string           `json:"device_id"`
	CaptureIdentity    string           `json:"capture_identity"`
	PayloadFingerprint string           `json:"payload_fingerprint"`
	MemoryID           string           `json:"memory_id"`
	State              reservationState `json:"state"`
	ReservedAt         string           `json:"reserved_at"`
	ExpiresAt          string           `json:"expires_at"`
	Receipt            *Receipt         `json:"receipt,omitempty"`
	Response           string           `json:"response,omitempty"`
}

// ReservationStore is the durable idempotency store rooted at a StateDir.
//
// It holds no cached view of the directory: every operation takes the
// cross-process lock, reads the record, acts, and writes it back, so two stores
// over one directory and two processes over one directory behave the same way.
// The only in-memory state is the in-flight set, which exists so concurrent
// duplicates inside ONE process wait for each other instead of racing the
// filesystem.
type ReservationStore struct {
	root    string
	now     func() time.Time
	entropy io.Reader

	mu       sync.Mutex
	inflight map[string]chan struct{}

	// counted, total and pending are the store's census, held in memory so the
	// hot path does no directory walk.
	//
	// A fresh reservation used to scan the whole store twice — once to count the
	// pending records against the bound, once more to trim — which made every
	// capture pay for every capture that came before it. The census is seeded by
	// ONE scan when the store opens and moved by insert, settle and sweep after
	// that. pending maps a reservation's path to when it was reserved, which is
	// all the sweep needs, so sweeping walks the pending SET rather than the
	// directory.
	//
	// It is per process, and that is the honest bound rather than a global one:
	// a second process opens its own store and takes its own census. One
	// `mora companion serve` is the writer in practice, and a second one
	// admitting up to its own limit is a bound of 2N, not of infinity.
	total   int
	pending map[string]time.Time

	// writeRecord publishes a reservation file. It is a field rather than a
	// direct call so a test can fail the write at the exact instant the ordering
	// rules exist for — the moment between "the vault holds the memory" and "the
	// reservation says so". There is no exported way to replace it and
	// production never does. The same seam, for the same reason, as
	// Registry.writeRecord.
	writeRecord func(path string, body []byte, beforeRename func() error) error
	// readDir is the directory walk, a field so a test can COUNT it. The claim
	// it exists for is a cost claim — that a fresh reservation walks nothing —
	// and a cost claim needs something that can be counted.
	readDir func(name string) ([]os.DirEntry, error)
}

// ReservationOption injects a seam. Both exist for tests; production passes
// neither and gets time.Now and crypto/rand.
type ReservationOption func(*ReservationStore)

// WithReservationClock pins the time source.
func WithReservationClock(now func() time.Time) ReservationOption {
	return func(s *ReservationStore) {
		if now != nil {
			s.now = now
		}
	}
}

// WithReservationEntropy pins the randomness source, so a test sees
// reproducible receipt and request identifiers.
func WithReservationEntropy(src io.Reader) ReservationOption {
	return func(s *ReservationStore) {
		if src != nil {
			s.entropy = src
		}
	}
}

// NewReservationStore returns a store under stateDir. The directory does not
// have to exist yet; it is created, hardened and re-hardened on use.
//
// Opening SWEEPS: expired records and crashed pending claims are collected here,
// so a listener that restarts after a bad night does not begin life at its own
// bound. The sweep is best effort and silent — a store that cannot tidy itself
// must still serve.
func NewReservationStore(stateDir string, opts ...ReservationOption) *ReservationStore {
	s := &ReservationStore{
		root:        filepath.Join(stateDir, "companion", "captures"),
		now:         time.Now,
		entropy:     rand.Reader,
		inflight:    map[string]chan struct{}{},
		pending:     map[string]time.Time{},
		writeRecord: writeSecretFile,
		readDir:     os.ReadDir,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.census(s.now())
	return s
}

// census is the ONE directory walk the store performs in the ordinary course of
// its life.
//
// It runs when the store opens: it collects what may not be kept — expired
// records of any state, and crashed pending claims past PendingSweepAfter — and
// seeds the in-memory counts from what survives. Everything after this is
// arithmetic.
//
// It is best effort and silent. A store that cannot tidy itself must still
// serve, and a listener that refused to start because one reservation file was
// unreadable would be trading a small problem for a total one.
func (s *ReservationStore) census(now time.Time) {
	total := 0
	pending := map[string]time.Time{}
	for _, row := range s.list() {
		if s.collectable(row.record, now) {
			_ = os.Remove(row.path)
			continue
		}
		total++
		if row.record.State == reservationPending {
			if reserved, err := time.Parse(time.RFC3339, row.record.ReservedAt); err == nil {
				pending[row.path] = reserved
			} else {
				pending[row.path] = time.Time{}
			}
		}
	}
	s.mu.Lock()
	s.total, s.pending = total, pending
	s.mu.Unlock()
}

// collectable reports whether a record may be deleted outright: expired at any
// state, or a pending claim nobody came back for.
func (s *ReservationStore) collectable(record reservationRecord, now time.Time) bool {
	if expiry, err := time.Parse(time.RFC3339, record.ExpiresAt); err == nil && !now.Before(expiry) {
		return true
	}
	if record.State != reservationPending {
		return false
	}
	reserved, err := time.Parse(time.RFC3339, record.ReservedAt)
	return err != nil || !now.Before(reserved.Add(PendingSweepAfter))
}

// Claim is a held reservation: the key is durably claimed and the holder is the
// one caller allowed to run the vault write for it.
//
// It is released exactly once, by Settle or by Abandon. A claim that is neither
// settled nor abandoned leaves the in-flight hold standing for the life of the
// process, which is why the capture path abandons it from a defer.
type Claim struct {
	store    *ReservationStore
	inflight string
	done     chan struct{}
	path     string
	record   reservationRecord
	release  sync.Once

	// TakenOver reports that this claim RECLAIMED a crashed attempt rather than
	// starting fresh. The caller uses it to ask the kernel whether the pinned
	// memory id is already published before it writes, which is the difference
	// between finishing somebody else's work and repeating it.
	TakenOver bool
}

// MemoryID is the vault id this claim publishes under.
func (c *Claim) MemoryID() string { return c.record.MemoryID }

// Identity is what was RESERVED, not what the caller recomputed.
//
// The difference matters on a takeover: the reclaimed record carries the id the
// crashed attempt pinned, and the kernel has to be asked about that id rather
// than about one derived a second time.
func (c *Claim) Identity() CaptureIdentity {
	return CaptureIdentity{
		DeviceID:    c.record.DeviceID,
		Key:         c.record.IdempotencyKey,
		Identity:    c.record.CaptureIdentity,
		Fingerprint: c.record.PayloadFingerprint,
		MemoryID:    c.record.MemoryID,
	}
}

// ---------------------------------------------------------------------------
// Reserve
// ---------------------------------------------------------------------------

// Reserve claims id.Key for id.DeviceID.
//
// It returns exactly one of three things:
//
//   - the exact response BYTES of the first attempt, when this key has already
//     settled for this capture. This is the replay answer, and it is the same
//     bytes rather than a re-marshalling of the same fields.
//   - a live *Claim, when the caller is the one who may now run the write.
//   - an error: ErrIdempotencyConflict for the same key over a different
//     capture, ErrCaptureInFlight while somebody else holds it,
//     ErrTooManyPending when the store is at its in-flight bound.
//
// The durable write happens before the claim is handed back, so a caller that
// holds a Claim is holding a promise that survives a crash.
func (s *ReservationStore) Reserve(id CaptureIdentity) ([]byte, *Claim, error) {
	if err := id.validate(); err != nil {
		return nil, nil, err
	}

	inflight := id.DeviceID + "\x00" + id.Key
	// The wait is bounded by a real timer rather than by the injected clock. A
	// pinned test clock does not advance on its own, and a wait that depended on
	// it would hang rather than fail.
	timer := time.NewTimer(reservationWait)
	defer timer.Stop()

	for {
		s.mu.Lock()
		waitOn, busy := s.inflight[inflight]
		if !busy {
			// The in-process hold is taken BEFORE the disk is touched, so two
			// goroutines cannot both read "no record" and both write one.
			done := make(chan struct{})
			s.inflight[inflight] = done
			s.mu.Unlock()

			replay, claim, err := s.reserveOnDisk(id, inflight, done)
			if claim == nil {
				// A replay, a conflict or a refusal holds nothing.
				s.finish(inflight, done)
			}
			return replay, claim, err
		}
		s.mu.Unlock()

		select {
		case <-waitOn:
			// The holder finished. Go round again: the record is settled now,
			// so the next pass returns its bytes rather than a second claim.
		case <-timer.C:
			return nil, nil, ErrCaptureInFlight
		}
	}
}

// reserveOnDisk is the durable half of Reserve, under the cross-process lock.
func (s *ReservationStore) reserveOnDisk(id CaptureIdentity, inflight string, done chan struct{}) ([]byte, *Claim, error) {
	if err := s.ensureDir(id.DeviceID); err != nil {
		return nil, nil, err
	}
	held, err := s.lock()
	if err != nil {
		return nil, nil, err
	}
	defer held.release()

	path := s.path(id.DeviceID, id.Key)
	now := s.now()
	takenOver := false

	fresh := true
	existing, err := readReservation(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// A fresh key.
	case err != nil:
		return nil, nil, err
	case s.collectable(existing, now):
		// Expired, or a crashed claim past the sweep window. Collecting it HERE
		// rather than on a timer is what keeps the hot path free of directory
		// walks: the one record this request touches is the one record that gets
		// checked, and the census moves by one.
		_ = os.Remove(path)
		s.forget(path)
	default:
		// The identity is checked FIRST, before the state, so a key reused for a
		// different capture is a conflict whether the first attempt settled or is
		// still running. The other order would let a second capture reclaim a
		// crashed reservation belonging to the first.
		//
		// captured_at is inside the identity, so a retry that re-stamps its clock
		// lands here rather than deriving a second vault id.
		if existing.CaptureIdentity != id.Identity {
			return nil, nil, ErrIdempotencyConflict
		}
		if existing.State == reservationSettled && existing.Response != "" {
			return []byte(existing.Response), nil, nil
		}
		if !s.takeoverDue(existing.ReservedAt, now) {
			// Another caller is inside its vault write right now. Taking the key
			// from it is safe — the pinned id cannot be written twice — but it
			// would mean two callers doing the same work, and the second would
			// answer before the first had settled.
			return nil, nil, ErrCaptureInFlight
		}
		// A pending record past the takeover window is a crashed attempt. Its
		// write may or may not have landed, so the caller checks the pinned id
		// against the vault before it writes.
		takenOver = true
		fresh = false
		// The reclaimed record keeps the id the crashed attempt pinned, which is
		// the whole point: the retry must aim at the same vault path.
		id.MemoryID = existing.MemoryID
	}

	// The store's HARD bound, answered from the census rather than from a walk.
	// It is enforced BEFORE the record is written, so a refusal creates nothing —
	// a bound that admitted the request and then tidied up afterwards would still
	// be a file per request while the pressure lasted.
	if fresh {
		if s.sweep(now) >= MaxPendingReservations {
			return nil, nil, ErrTooManyPending
		}
	}

	record := reservationRecord{
		Header:             newHeader(schemaCaptureReservation),
		IdempotencyKey:     id.Key,
		DeviceID:           id.DeviceID,
		CaptureIdentity:    id.Identity,
		PayloadFingerprint: id.Fingerprint,
		MemoryID:           id.MemoryID,
		State:              reservationPending,
		ReservedAt:         reservationStamp(now),
		ExpiresAt:          reservationStamp(now.Add(ReservationTTL)),
	}
	if err := s.write(path, record, held); err != nil {
		return nil, nil, err
	}
	s.remember(path, now, fresh)
	// Trimming runs after the write and never touches the record just written, so
	// a store at its total cap can still accept the capture that pushed it there.
	// It is the one operation that still walks, and it runs only when the census
	// says the store is over its total cap — which is the moment a walk is what
	// the caller is asking for.
	s.trim(path)
	return nil, &Claim{
		store: s, inflight: inflight, done: done, path: path, record: record, TakenOver: takenOver,
	}, nil
}

// remember moves the census for a reservation that was just written.
func (s *ReservationStore) remember(path string, now time.Time, fresh bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fresh {
		s.total++
	}
	s.pending[path] = now
}

// forget moves the census for a reservation that was just deleted.
func (s *ReservationStore) forget(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, path)
	if s.total > 0 {
		s.total--
	}
}

// settled moves the census for a reservation that just reached its receipt. The
// record survives; it is no longer a live claim.
func (s *ReservationStore) settled(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, path)
}

// takeoverDue reports whether a pending reservation stamped at reservedAt is old
// enough to be treated as a crashed attempt.
//
// An unparseable stamp is treated as due. A reservation whose age cannot be read
// is a corrupt record, and leaving it undecidable would wedge that key forever;
// the identity check above still protects the capture identity, and the pinned
// id still protects the vault.
func (s *ReservationStore) takeoverDue(reservedAt string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, reservedAt)
	if err != nil {
		return true
	}
	return !now.Before(t.Add(ReservationTakeover))
}

// ---------------------------------------------------------------------------
// Settle and Abandon
// ---------------------------------------------------------------------------

// Settle stores the terminal receipt WITH the exact bytes that answered it, and
// releases the claim.
//
// The response bytes are stored rather than rebuilt because a replay has to be
// the same answer on the wire, not merely the same fields: a client that hashes
// or caches a response body is entitled to have the retry match it byte for
// byte.
//
// The receipt is validated and cross-checked against what was reserved before
// anything is written: a receipt for a different key, device or payload would
// make the store answer a future replay with somebody else's outcome.
func (c *Claim) Settle(r Receipt, response []byte) error {
	if c == nil || c.store == nil {
		return ErrNoClaim
	}
	defer c.finish()
	if err := r.Validate(); err != nil {
		return err
	}
	if len(response) == 0 {
		return fmt.Errorf("companion: a settled reservation carries the bytes it answered with")
	}
	if r.IdempotencyKey != c.record.IdempotencyKey ||
		r.DeviceID != c.record.DeviceID ||
		r.PayloadFingerprint != c.record.PayloadFingerprint {
		return fmt.Errorf("companion: this receipt does not describe the reserved capture")
	}
	held, err := c.store.lock()
	if err != nil {
		return err
	}
	defer held.release()

	record := c.record
	record.State = reservationSettled
	record.Receipt = &r
	record.Response = string(response)
	if err := c.store.write(c.path, record, held); err != nil {
		return err
	}
	// The record survives; it is no longer a live claim, so it leaves the pending
	// set and stops counting against the in-flight bound.
	c.store.settled(c.path)
	return nil
}

// Abandon releases the in-process hold WITHOUT settling.
//
// The pending record stays on disk deliberately. It is the durable evidence that
// this key was claimed, and it is what a retry inside the takeover window sees
// instead of racing. Past that window the record is sweepable, and losing it
// costs nothing: the pinned memory id, not the record, is what stops a second
// write.
func (c *Claim) Abandon() {
	if c == nil {
		return
	}
	c.finish()
}

func (c *Claim) finish() {
	c.release.Do(func() { c.store.finish(c.inflight, c.done) })
}

// finish drops the in-flight hold and wakes every waiter for that key.
func (s *ReservationStore) finish(inflight string, done chan struct{}) {
	s.mu.Lock()
	if current, ok := s.inflight[inflight]; ok && current == done {
		delete(s.inflight, inflight)
	}
	s.mu.Unlock()
	close(done)
}

// SettleFromReplay settles a PENDING reservation with bytes that are already
// durable somewhere else.
//
// It closes the last window in the ordering: the receipt reaches the kernel's
// canonical record before the reservation settles, so a crash or a failed settle
// in between leaves a publication that IS complete and a reservation that says
// pending. A retry then answers from the canonical bytes without ever coming
// back for the row, which sits there occupying the in-flight bound until the
// sweep collects it.
//
// It is a no-op when there is nothing to finish — no record, or one that already
// settled — so a replay may call it unconditionally. The receipt is decoded from
// the bytes and validated before anything is written, because these bytes are
// about to become the stored answer to every future replay.
func (s *ReservationStore) SettleFromReplay(id CaptureIdentity, response []byte) error {
	if err := id.validate(); err != nil {
		return err
	}
	if len(response) == 0 {
		return fmt.Errorf("companion: a replay settles with the bytes it answered with")
	}
	var receipt Receipt
	if err := json.Unmarshal(response, &receipt); err != nil {
		return fmt.Errorf("companion: the replay bytes are not a receipt: %w", err)
	}
	if err := receipt.Validate(); err != nil {
		return err
	}
	if receipt.IdempotencyKey != id.Key || receipt.DeviceID != id.DeviceID ||
		receipt.PayloadFingerprint != id.Fingerprint {
		return fmt.Errorf("companion: those replay bytes do not describe this capture")
	}

	if err := s.ensureDir(id.DeviceID); err != nil {
		return err
	}
	held, err := s.lock()
	if err != nil {
		return err
	}
	defer held.release()

	path := s.path(id.DeviceID, id.Key)
	record, err := readReservation(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.State == reservationSettled || record.CaptureIdentity != id.Identity {
		return nil
	}
	record.State = reservationSettled
	record.Receipt = &receipt
	record.Response = string(response)
	if err := s.write(path, record, held); err != nil {
		return err
	}
	s.settled(path)
	return nil
}

// ---------------------------------------------------------------------------
// Identifiers
// ---------------------------------------------------------------------------

// NewID mints an identifier in the shape the rest of Mora generates: a kind
// prefix, a date, a time, and a random hex tail (`rcp_20260904_120000_a1b2c3d4`).
//
// It lives on the store because the store is the only thing on the capture path
// that holds an entropy source, and putting one on the listener would give the
// HTTP surface a seam it has no other use for. Nothing may be parsed out of the
// opaque half.
func (s *ReservationStore) NewID(prefix string) (string, error) {
	tail := make([]byte, reservationIDEntropyBytes)
	if _, err := io.ReadFull(s.entropy, tail); err != nil {
		return "", fmt.Errorf("companion: read entropy: %w", err)
	}
	return fmt.Sprintf("%s%s_%s", prefix, s.now().UTC().Format("20060102_150405"), hex.EncodeToString(tail)), nil
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func (s *ReservationStore) lockPath() string { return filepath.Join(s.root, ".lock") }

func (s *ReservationStore) deviceDir(deviceID string) string {
	return filepath.Join(s.root, deviceID)
}

// path is the reservation file for one key.
//
// The name is a digest of the key rather than the key itself. A key is
// device-chosen text within an opaque character set, and while that set holds no
// path separator today, a filename derived from attacker-chosen bytes is a
// traversal waiting for the character set to widen. A digest is fixed width,
// case-stable on a case-insensitive filesystem, and reveals nothing.
func (s *ReservationStore) path(deviceID, key string) string {
	digest := strings.TrimPrefix(Fingerprint(key), "sha256:")
	return filepath.Join(s.deviceDir(deviceID), digest+".json")
}

// ensureDir creates the store's directories and asserts their modes on every
// use, for the same reason registry.go does: a mode set only at creation trusts
// an umask, a restore-from-backup and a chmod it has no reason to trust.
func (s *ReservationStore) ensureDir(deviceID string) error {
	for _, dir := range []string{s.root, s.deviceDir(deviceID)} {
		if err := os.MkdirAll(dir, secretDirMode); err != nil {
			return err
		}
		if err := hardenPath(dir, secretDirMode); err != nil {
			return err
		}
	}
	return nil
}

func (s *ReservationStore) lock() (*lockFile, error) {
	if err := os.MkdirAll(s.root, secretDirMode); err != nil {
		return nil, err
	}
	return acquireLock(s.lockPath(), secretFileMode, lockTimeout, lockPoll)
}

// write publishes a reservation atomically at 0600, re-asserting that the lock
// is still held in the instant before the rename.
func (s *ReservationStore) write(path string, record reservationRecord, held *lockFile) error {
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if len(body) > maxReservationBytes {
		return fmt.Errorf("companion: a reservation of %d bytes is over the %d-byte limit", len(body), maxReservationBytes)
	}
	return s.writeRecord(path, body, func() error {
		if !held.stillOwns() {
			return ErrLocked
		}
		return nil
	})
}

// readReservation reads and validates one reservation file.
//
// The size bound is enforced BY the read rather than by a stat beside it, for
// the reason registry.go's load explains: a stat answers a question about one
// moment and the read happens in another.
func readReservation(path string) (reservationRecord, error) {
	if err := hardenPath(path, secretFileMode); err != nil {
		return reservationRecord{}, err
	}
	body, err := readBounded(path, maxReservationBytes)
	if errors.Is(err, errTooLarge) {
		return reservationRecord{}, fmt.Errorf("companion: %s is over the %d-byte limit; this is a corrupt or hostile file, not a large one", path, maxReservationBytes)
	}
	if err != nil {
		return reservationRecord{}, err
	}
	var record reservationRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return reservationRecord{}, fmt.Errorf("companion: %s is not readable as a capture reservation (%w)", path, err)
	}
	if err := record.validate(schemaCaptureReservation); err != nil {
		return reservationRecord{}, fmt.Errorf("companion: %s is not a capture reservation: %w", path, err)
	}
	// A stored receipt is validated on the way OUT as well as on the way in. It
	// is about to answer a replay, and a record edited on disk must not become a
	// projection that never passed Validate.
	if record.Receipt != nil {
		if err := record.Receipt.Validate(); err != nil {
			return reservationRecord{}, fmt.Errorf("companion: %s holds a receipt that does not validate: %w", path, err)
		}
	}
	return record, nil
}

// ---------------------------------------------------------------------------
// Sweeping and trimming
// ---------------------------------------------------------------------------

// sweep collects crashed pending claims and reports how many live ones remain.
//
// It walks the PENDING SET, not the directory. That is the difference the round
// two review named: the old shape read every record in the store on every fresh
// reservation, so a capture cost a walk over every capture before it. The set is
// the only thing a sweep needs — a pending record is identified by its path and
// judged by when it was reserved — and both are in memory.
//
// Sweeping a crashed pending record is safe because the memory id is DERIVED
// rather than stored: the retry re-derives the same id, and the kernel's
// create-exclusive publish refuses the second write. The record was never the
// thing holding the guarantee.
//
// It is best effort: a capture must not fail because housekeeping did. The count
// it returns is what the caller compares against MaxPendingReservations, so the
// bound is measured AFTER the sweep rather than against claims that no longer
// deserve to exist.
func (s *ReservationStore) sweep(now time.Time) int {
	s.mu.Lock()
	stale := make([]string, 0, len(s.pending))
	for path, reserved := range s.pending {
		if reserved.IsZero() || !now.Before(reserved.Add(PendingSweepAfter)) {
			stale = append(stale, path)
		}
	}
	for _, path := range stale {
		delete(s.pending, path)
		if s.total > 0 {
			s.total--
		}
	}
	live := len(s.pending)
	s.mu.Unlock()

	for _, path := range stale {
		_ = os.Remove(path)
	}
	return live
}

// trim enforces the TOTAL cap by dropping the oldest settled records.
//
// Settled records are pure idempotency memory, so dropping one risks a duplicate
// on a very old retry — and even that is now bounded by the pinned id, which the
// vault still refuses to write twice. A pending record is never trimmed: it is a
// claim somebody may still be acting on.
func (s *ReservationStore) trim(keep string) {
	s.mu.Lock()
	over := s.total > MaxReservations
	s.mu.Unlock()
	if !over {
		return
	}
	rows := s.list()
	if len(rows) <= MaxReservations {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].record.ReservedAt < rows[j].record.ReservedAt })
	excess := len(rows) - MaxReservations
	for _, row := range rows {
		if excess == 0 {
			break
		}
		if row.path == keep || row.record.State != reservationSettled {
			continue
		}
		if os.Remove(row.path) == nil {
			s.forget(row.path)
			excess--
		}
	}
}

type reservationFile struct {
	path   string
	record reservationRecord
}

// list walks the store. An unreadable entry is skipped rather than fatal:
// sweeping is housekeeping, and one corrupt file must not stop the rest being
// bounded.
func (s *ReservationStore) list() []reservationFile {
	devices, err := s.readDir(s.root)
	if err != nil {
		return nil
	}
	out := []reservationFile{}
	for _, device := range devices {
		if !device.IsDir() {
			continue
		}
		entries, err := s.readDir(filepath.Join(s.root, device.Name()))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(s.root, device.Name(), entry.Name())
			record, err := readReservation(path)
			if err != nil {
				continue
			}
			out = append(out, reservationFile{path: path, record: record})
		}
	}
	return out
}

// reservationStamp renders the one timestamp format this package publishes:
// RFC3339, UTC, second precision.
func reservationStamp(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}
