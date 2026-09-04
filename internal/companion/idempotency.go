package companion

// idempotency.go is the durable reservation store behind governed capture
// (graph node N21).
//
// A phone on a train retries. It retries because the socket died, because iOS
// suspended the app mid-request, because the user tapped twice. Every one of
// those retries carries the SAME idempotency key, and the only acceptable
// outcome is one memory in the vault and one receipt — byte for byte the
// receipt the first attempt would have produced.
//
// # The ordering, and why it is that way round
//
//	reserve (durable)  ->  vault write  ->  settle (durable)
//
// The reservation is fsynced and renamed into place BEFORE the kernel is asked
// to write anything. That ordering is the whole design. Reserving after the
// write would mean a crash in between leaves a memory in the vault that no key
// points at, so the phone's retry writes a second one and the user has the same
// note twice with no way to tell which is which. Reserving first means a crash
// in between leaves a key pointing at a write that never happened, and the
// retry completes it — the vault gains one memory, not two.
//
// # What this store does NOT promise
//
// It is not a distributed transaction. There is a window between the kernel's
// vault publication and this store's settle write: a process killed inside it
// leaves a `pending` reservation over a memory that DOES exist, and a retry
// after the takeover window writes a second one. Closing that would mean the
// vault write itself carrying the idempotency key, which is a change to the
// kernel's write path rather than to this file. The window is one fsync wide
// and it is stated here rather than papered over.
//
// # Bounded, because a phone can talk
//
// Every reservation is a file a device caused to exist, so the store is a DoS
// surface unless it is bounded in three directions at once: MaxReservations
// caps how many exist, ReservationTTL caps how long one lives, and
// maxReservationBytes caps how big one can be on the read path. Pruning is
// oldest-settled-first, so the entries that survive pressure are the ones a
// retry is most likely to ask about. Losing an old settled entry risks a
// duplicate on a week-old retry; keeping an unbounded directory risks the Mac.
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
	// A capture writes one file, so without a cap a phone in a retry loop with
	// a fresh key each time is an unbounded directory.
	MaxReservations = 512
	// ReservationTTL is how long a settled receipt stays answerable. A retry
	// after this gets a fresh receipt for a second memory, which is why it is
	// days rather than minutes: a phone that was offline over a weekend is a
	// normal phone.
	ReservationTTL = 7 * 24 * time.Hour
	// ReservationTakeover is how long a pending reservation is treated as
	// somebody else's live work rather than as a crash to recover from.
	//
	// It is longer than the listener's KernelTimeout on purpose. Shorter, and a
	// second process could take over a reservation whose holder is still inside
	// its vault write — which is the duplicate this file exists to prevent,
	// arrived at from the other direction.
	ReservationTakeover = 3 * KernelTimeout
	// maxReservationBytes bounds ONE reservation file on the read path. A
	// receipt is under 2 KiB; this leaves room for the envelope and refuses
	// anything that is not a reservation.
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
	// DIFFERENT payload. It is never a silent overwrite and never a silent
	// replay: the first payload keeps the key.
	ErrIdempotencyConflict = errors.New("companion: that idempotency key already covers a different payload")
	// ErrCaptureInFlight reports that another caller holds this key right now.
	// It is transient: the retry that follows finds the settled receipt.
	ErrCaptureInFlight = errors.New("companion: that idempotency key is being processed")
	// ErrNoClaim reports a Settle against a claim that was already released.
	ErrNoClaim = errors.New("companion: this reservation is no longer held")
)

// reservationState is the two-state lifecycle of a stored reservation.
type reservationState string

const (
	// reservationPending means the key is claimed and the vault write has not
	// been confirmed. A pending record older than ReservationTakeover is a
	// crashed attempt and may be taken over.
	reservationPending reservationState = "pending"
	// reservationSettled means the capture reached a terminal receipt, which is
	// stored verbatim so a replay returns the same bytes.
	reservationSettled reservationState = "settled"
)

// reservationRecord is the stored form of one reservation.
//
// It carries the fingerprint rather than the text: the store answers "same key,
// same payload?" and has no business holding a word the user wrote. The receipt
// it settles with carries none either.
type reservationRecord struct {
	Header
	IdempotencyKey     string           `json:"idempotency_key"`
	DeviceID           string           `json:"device_id"`
	PayloadFingerprint string           `json:"payload_fingerprint"`
	State              reservationState `json:"state"`
	ReservedAt         string           `json:"reserved_at"`
	ExpiresAt          string           `json:"expires_at"`
	Receipt            *Receipt         `json:"receipt,omitempty"`
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
func NewReservationStore(stateDir string, opts ...ReservationOption) *ReservationStore {
	s := &ReservationStore{
		root:     filepath.Join(stateDir, "companion", "captures"),
		now:      time.Now,
		entropy:  rand.Reader,
		inflight: map[string]chan struct{}{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
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
}

// ---------------------------------------------------------------------------
// Reserve
// ---------------------------------------------------------------------------

// Reserve claims key for device against fingerprint.
//
// It returns exactly one of three things:
//
//   - a stored terminal Receipt, when this key has already settled for this
//     payload. This is the replay answer and it is byte-identical, because the
//     receipt is returned from storage rather than rebuilt.
//   - a live *Claim, when the caller is the one who may now run the write.
//   - an error: ErrIdempotencyConflict for the same key over a different
//     payload, ErrCaptureInFlight while somebody else holds it.
//
// The durable write happens before the claim is handed back, so a caller that
// holds a Claim is holding a promise that survives a crash.
func (s *ReservationStore) Reserve(deviceID, key, fingerprint string) (Receipt, *Claim, error) {
	if err := validateID("device_id", PrefixDevice, deviceID); err != nil {
		return Receipt{}, nil, err
	}
	if err := validateIdempotencyKey("idempotency_key", key); err != nil {
		return Receipt{}, nil, err
	}
	if err := validateFingerprint("payload_fingerprint", fingerprint); err != nil {
		return Receipt{}, nil, err
	}

	inflight := deviceID + "\x00" + key
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

			rcpt, claim, err := s.reserveOnDisk(deviceID, key, fingerprint, inflight, done)
			if claim == nil {
				// A replay, a conflict or a failure holds nothing.
				s.finish(inflight, done)
			}
			return rcpt, claim, err
		}
		s.mu.Unlock()

		select {
		case <-waitOn:
			// The holder finished. Go round again: the record is settled now,
			// so the next pass returns its receipt rather than a second claim.
		case <-timer.C:
			return Receipt{}, nil, ErrCaptureInFlight
		}
	}
}

// reserveOnDisk is the durable half of Reserve, under the cross-process lock.
func (s *ReservationStore) reserveOnDisk(deviceID, key, fingerprint, inflight string, done chan struct{}) (Receipt, *Claim, error) {
	if err := s.ensureDir(deviceID); err != nil {
		return Receipt{}, nil, err
	}
	held, err := s.lock()
	if err != nil {
		return Receipt{}, nil, err
	}
	defer held.release()

	path := s.path(deviceID, key)
	now := s.now()

	existing, err := readReservation(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// A fresh key.
	case err != nil:
		return Receipt{}, nil, err
	default:
		// The fingerprint is checked FIRST, before the state, so a key reused
		// for different text is a conflict whether the first attempt settled or
		// is still running. The other order would let a second payload take over
		// a crashed reservation belonging to the first.
		if existing.PayloadFingerprint != fingerprint {
			return Receipt{}, nil, ErrIdempotencyConflict
		}
		if existing.State == reservationSettled && existing.Receipt != nil {
			return *existing.Receipt, nil, nil
		}
		if !s.takeoverDue(existing.ReservedAt, now) {
			// Another process is inside its vault write right now. Taking the
			// key from it would produce the duplicate this file exists to
			// prevent, so the caller is told to come back.
			return Receipt{}, nil, ErrCaptureInFlight
		}
		// A pending record older than the takeover window is a crashed attempt.
		// The write it reserved never confirmed, so this caller completes it.
	}

	record := reservationRecord{
		Header:             newHeader(schemaCaptureReservation),
		IdempotencyKey:     key,
		DeviceID:           deviceID,
		PayloadFingerprint: fingerprint,
		State:              reservationPending,
		ReservedAt:         reservationStamp(now),
		ExpiresAt:          reservationStamp(now.Add(ReservationTTL)),
	}
	if err := s.write(path, record, held); err != nil {
		return Receipt{}, nil, err
	}
	// Pruning runs after the write and never touches the record just written, so
	// a store at its cap can still accept the capture that pushed it there.
	s.prune(now, path)
	return Receipt{}, &Claim{store: s, inflight: inflight, done: done, path: path, record: record}, nil
}

// takeoverDue reports whether a pending reservation stamped at reservedAt is old
// enough to be treated as a crashed attempt.
//
// An unparseable stamp is treated as due. A reservation whose age cannot be read
// is a corrupt record, and leaving it undecidable would wedge that key forever;
// the fingerprint check above still protects the payload identity.
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

// Settle stores the terminal receipt and releases the claim.
//
// The receipt is validated and cross-checked against what was reserved before
// anything is written: a receipt for a different key, device or payload would
// make the store answer a future replay with somebody else's outcome.
func (c *Claim) Settle(r Receipt) error {
	if c == nil || c.store == nil {
		return ErrNoClaim
	}
	defer c.finish()
	if err := r.Validate(); err != nil {
		return err
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
	return c.store.write(c.path, record, held)
}

// Abandon releases the in-process hold WITHOUT settling.
//
// The pending record stays on disk deliberately. It is the durable evidence
// that this key was claimed, and the reason a crash between the reservation and
// the write cannot become two memories: the retry finds the pending record and
// completes it once the takeover window has passed.
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
	return writeSecretFile(path, body, func() error {
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
	// is about to be handed to a device as the answer to a replay, and a record
	// edited on disk must not become a projection that never passed Validate.
	if record.Receipt != nil {
		if err := record.Receipt.Validate(); err != nil {
			return reservationRecord{}, fmt.Errorf("companion: %s holds a receipt that does not validate: %w", path, err)
		}
	}
	return record, nil
}

// ---------------------------------------------------------------------------
// Pruning
// ---------------------------------------------------------------------------

// prune keeps the store inside its bounds. It is best effort: a capture must not
// fail because housekeeping did.
//
// Expired entries go first. If the store is still over MaxReservations, the
// OLDEST SETTLED entries go next — settled entries are pure idempotency memory,
// so dropping one risks a duplicate on a very old retry, while dropping a
// pending one would drop the record that stops a duplicate right now. A pending
// entry inside its takeover window is never pruned.
func (s *ReservationStore) prune(now time.Time, keep string) {
	rows := s.list()
	live := make([]reservationFile, 0, len(rows))
	for _, row := range rows {
		if row.path == keep {
			live = append(live, row)
			continue
		}
		if expiry, err := time.Parse(time.RFC3339, row.record.ExpiresAt); err == nil && !now.Before(expiry) {
			_ = os.Remove(row.path)
			continue
		}
		live = append(live, row)
	}
	if len(live) <= MaxReservations {
		return
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].record.ReservedAt < live[j].record.ReservedAt })
	over := len(live) - MaxReservations
	for _, row := range live {
		if over == 0 {
			break
		}
		if row.path == keep || row.record.State != reservationSettled {
			continue
		}
		if os.Remove(row.path) == nil {
			over--
		}
	}
}

type reservationFile struct {
	path   string
	record reservationRecord
}

// list walks the store. An unreadable entry is skipped rather than fatal: prune
// is housekeeping, and one corrupt file must not stop the rest being bounded.
func (s *ReservationStore) list() []reservationFile {
	devices, err := os.ReadDir(s.root)
	if err != nil {
		return nil
	}
	out := []reservationFile{}
	for _, device := range devices {
		if !device.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.root, device.Name()))
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
