package companion

// registry.go is the device registry: the durable half of pairing. schema.go
// says what a device IS on the wire; this file decides which devices exist,
// which credential authenticates one, and when that credential stops working.
//
// It stays inside the leaf rule (TestPackageIsALeaf): standard library only.
// That is why the atomic write, the lock and the directory hardening below are
// written out here instead of importing internal/atomicio — the contract has to
// stay compilable into a fixture generator and a Swift-parity harness without
// dragging the kernel along.
//
// # Where the secrets live
//
// ConfigDir holds the credential material and is hardened to 0700 with 0600
// files on every open, not only on create. A registry that only sets its modes
// at creation time trusts an umask, a restore-from-backup and a `chmod -R` it
// has no reason to trust; hardenPath re-asserts them.
//
//	<ConfigDir>/companion/devices.json   0600  device records, token hashes
//	<ConfigDir>/companion/host.key       0600  host identity seed
//	<ConfigDir>/companion/.lock          0600  cross-process write lock
//	<StateDir>/companion/receipts/*.json 0600  append-only audit of pair/revoke
//
// The receipts are in StateDir rather than ConfigDir on purpose: they are the
// audit trail, they grow, and they carry no secret — only fingerprints — so
// they are safe in the tree a user is likelier to sync or ship in a bug report.
//
// # What is never written
//
// The bearer token and the one-time pairing code the registry GENERATES exist
// in memory and in the QR payload, and nowhere else: both are persisted only as
// SHA-256 fingerprints. A stolen devices.json therefore lets an attacker
// enumerate devices and revoke them; it does not let them authenticate as one.
// TestRegistryPersistsNoSecrets enforces it byte-for-byte.
//
// The guarantee is about the generated fields, not about every byte in the
// file. A caller that passes a live pairing code as a device LABEL has written
// it to disk itself, and no boundary here can undo that — the label is
// operator-supplied text and is stored verbatim.
//
// # What is bounded
//
// The record file is bounded at MaxRegistryBytes and MaxDevices, and both are
// enforced on the read path, not only on the write path. Authenticate loads the
// file on every request the listener serves, so an unbounded file is a
// per-request memory amplifier rather than a one-off parse cost. A file past
// either bound is refused as corrupt or hostile; it is never truncated, because
// silently dropping devices is silently dropping revocations.
//
// # How the write lock works
//
// Exclusion is held by the operating system on an open handle — flock on POSIX,
// a zero share mode on Windows — and released when that handle closes,
// including when the process dies. Nothing removes the lock file. An O_EXCL
// sentinel with a staleness sweep was tried first and cannot be made correct:
// any rule that lets one process take a lock away from another is a
// check-then-use race on both sides of the handoff. See
// registry_lock_unix.go.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// PairingTTL is how long a printed pairing code stays usable. It is short
// because the code is a bearer secret displayed on a screen: the window in
// which a shoulder-surfed QR code is worth anything is the window this bounds.
const PairingTTL = 10 * time.Minute

const (
	// tokenEntropyBytes sizes the bearer token. 32 bytes is the point past
	// which the SHA-256 fingerprint stored in devices.json is not worth
	// attacking: there is no dictionary to run against it.
	tokenEntropyBytes = 32
	// pairingCodeEntropyBytes sizes the one-time code. It is smaller than the
	// token because it is transcribable by a human in a fallback flow and it
	// dies in PairingTTL; 20 bytes is 160 bits, which no ten-minute window
	// reaches.
	pairingCodeEntropyBytes = 20
	// deviceIDEntropyBytes sizes the random tail of a dev_ identifier.
	deviceIDEntropyBytes = 4
)

// Bounds on the record file. Authenticate reads it on every request N12's
// listener serves, so an unbounded file is a per-request memory amplifier, not
// just a slow parse: a 200 MB devices.json costs 200 MB of allocation on every
// inbound request until the process dies.
//
// Neither bound is a capacity target. MaxDevices is far above any real number
// of phones one person pairs, and a file that exceeds either is corrupt or
// hostile rather than large, so both refuse with that framing rather than
// truncating.
const (
	// MaxRegistryBytes bounds the encoded record file.
	MaxRegistryBytes = 1 << 20
	// MaxDevices bounds how many devices one vault registers.
	MaxDevices = 64
)

// registryFileVersion is the on-disk record-file version. It moves only when a
// stored field is removed or retyped; adding one is backward compatible because
// the decoder tolerates absent fields.
const registryFileVersion = 1

// Filesystem modes. The directory mode and the file mode are separate constants
// because they are separate promises: 0700 keeps another local account from
// LISTING the credential store, 0600 keeps it from READING one file it already
// knows the name of.
const (
	secretDirMode  os.FileMode = 0o700
	secretFileMode os.FileMode = 0o600
)

// Registry errors. They are values rather than formatted strings so a caller —
// the CLI here, the listener in N12 — can branch on the condition without
// matching prose.
var (
	// ErrNoSuchDevice is returned for a device_id that was never issued.
	ErrNoSuchDevice = errors.New("no such device")
	// ErrNotPending is returned when a confirmation arrives for a device that
	// is already active or already revoked. Replaying a confirmation must not
	// mint a second token.
	ErrNotPending = errors.New("device is not awaiting pairing")
	// ErrPairingExpired is returned when the code was right but late.
	ErrPairingExpired = errors.New("pairing code expired")
	// ErrPairingCode is returned when the code was wrong. It is deliberately
	// distinct from ErrPairingExpired for the operator and deliberately
	// identical in cost for an attacker: both are decided after the same
	// constant-time comparison.
	ErrPairingCode = errors.New("pairing code does not match")
	// ErrReceiptNotWritten means the change COMMITTED and only its audit row
	// failed. It is a warning, never a reason to undo or withhold the change:
	// a caller that discarded a minted token here would leave an active
	// credential nobody holds, which is the exact failure the ordering exists
	// to prevent. Callers surface it and carry on.
	ErrReceiptNotWritten = errors.New("the change was applied but its audit receipt could not be written")
	// ErrRegistryTooLarge is returned when a mutation would write a record
	// file that load would then refuse. It is a distinct error because the
	// caller's remedy is to revoke a device, not to retry.
	ErrRegistryTooLarge = errors.New("the device registry would exceed its size limit")
	// ErrLocked is returned when another process held the registry lock for
	// longer than lockTimeout, or when this process lost its own lock to the
	// stale sweep before it could write.
	ErrLocked = errors.New("the device registry is locked by another process")
)

// errNoChange is the internal signal a mutation returns when it decided that
// nothing needs writing. It never reaches a caller.
var errNoChange = errors.New("companion: nothing to write")

// AuthError is the typed refusal Authenticate returns. Reason is drawn from the
// frozen reject_reason vocabulary so the listener can render it and the phone
// can decode it without parsing prose.
type AuthError struct{ Reason RejectReason }

func (e *AuthError) Error() string { return "companion: " + string(e.Reason) }

// Registry is the durable device registry rooted at a ConfigDir and a StateDir.
// It holds no state between calls: every operation reads the record file, acts,
// and writes it back under the lock, so two Registry values over one directory
// and two processes over one directory behave the same way.
type Registry struct {
	configDir string
	stateDir  string
	now       func() time.Time
	entropy   io.Reader

	// writeRecord publishes the record file and writeAudit publishes an audit
	// row. They are fields rather than direct calls so a test can fail either
	// half at the exact instant the ordering rules exist for — the moment
	// between "the state is on disk" and "the row describing it is on disk".
	// There is no exported way to replace either and production never does.
	writeRecord func(path string, body []byte, beforeRename func() error) error
	writeAudit  func(rcpt receipt, beforeRename func() error) error
}

// RegistryOption injects a seam. Both seams exist for tests; production passes
// neither and gets time.Now and crypto/rand.
type RegistryOption func(*Registry)

// WithClock pins the time source.
func WithClock(now func() time.Time) RegistryOption {
	return func(r *Registry) {
		if now != nil {
			r.now = now
		}
	}
}

// WithEntropy pins the randomness source. A test that pins it gets reproducible
// device ids, codes and tokens; nothing else in the package may read entropy.
func WithEntropy(src io.Reader) RegistryOption {
	return func(r *Registry) {
		if src != nil {
			r.entropy = src
		}
	}
}

// NewRegistry returns a registry over the given directories. Neither directory
// has to exist yet; each is created, hardened and re-hardened on use.
func NewRegistry(configDir, stateDir string, opts ...RegistryOption) *Registry {
	r := &Registry{
		configDir:   configDir,
		stateDir:    stateDir,
		now:         time.Now,
		entropy:     rand.Reader,
		writeRecord: writeSecretFile,
	}
	r.writeAudit = r.writeReceipt
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ---------------------------------------------------------------------------
// On-disk records
// ---------------------------------------------------------------------------

// deviceRecord is the stored form of a device. It is a superset of the wire
// Device: everything the wire type carries plus the material that must never
// leave this file.
//
// The two hash fields are the whole security model of the registry. Neither the
// pairing code nor the bearer token is stored, so the file is a list of things
// a credential could be checked AGAINST, never a list of credentials.
type deviceRecord struct {
	DeviceID string      `json:"device_id"`
	Label    string      `json:"label"`
	Platform Platform    `json:"platform"`
	State    DeviceState `json:"state"`

	// TokenFingerprint is sha256 over the bearer token, in the published
	// "sha256:<hex>" form, so the value stored here and the value projected in
	// `mora companion list` are the same string.
	TokenFingerprint string `json:"token_fingerprint,omitempty"`
	// PairingCodeFingerprint is sha256 over the one-time code. It is cleared
	// the moment the code is spent or the device is revoked.
	PairingCodeFingerprint string `json:"pairing_code_fingerprint,omitempty"`
	PairingExpiresAt       string `json:"pairing_expires_at,omitempty"`

	// PublicKey is the device's own key material from the confirmation. The
	// registry stores it and does not yet use it: it is the hook N12's listener
	// needs if bearer auth is ever upgraded to a signature.
	PublicKey string `json:"public_key,omitempty"`

	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
	RevokedAt  string `json:"revoked_at,omitempty"`
}

// registryFile is the whole record file.
type registryFile struct {
	Version int             `json:"version"`
	Devices []*deviceRecord `json:"devices"`
}

// device projects a record onto the published wire type. It is the only place a
// Device is built from storage, so a field that must never be projected — the
// pairing-code fingerprint, the pairing expiry — cannot leak by being added to
// the record struct and forgotten here.
func (rec *deviceRecord) device() Device {
	d := NewDevice()
	d.DeviceID = rec.DeviceID
	d.Label = rec.Label
	d.Platform = rec.Platform
	d.State = rec.State
	d.TokenFingerprint = rec.TokenFingerprint
	d.CreatedAt = rec.CreatedAt
	d.LastSeenAt = rec.LastSeenAt
	d.RevokedAt = rec.RevokedAt
	return d
}

// ---------------------------------------------------------------------------
// Operations
// ---------------------------------------------------------------------------

// Pair registers a pending device and returns the payload the phone scans.
//
// The returned payload carries the ONLY copy of the one-time code: the registry
// keeps a fingerprint. A caller that loses the payload has to pair again, which
// is the correct trade — the alternative is a code sitting in a file.
func (r *Registry) Pair(label string, platform Platform, endpoint string) (PairingPayload, error) {
	if err := validateText("label", label, MaxLabelBytes, true); err != nil {
		return PairingPayload{}, err
	}
	if err := inVocabulary("platform", string(platform), "platform"); err != nil {
		return PairingPayload{}, err
	}
	if err := validateEndpoint("endpoint", endpoint); err != nil {
		return PairingPayload{}, err
	}

	host, err := r.HostFingerprint()
	if err != nil {
		return PairingPayload{}, err
	}
	deviceID, err := r.newDeviceID()
	if err != nil {
		return PairingPayload{}, err
	}
	code, err := r.newSecret(pairingCodeEntropyBytes)
	if err != nil {
		return PairingPayload{}, err
	}

	now := r.stamp()
	expires := r.stampAt(r.now().Add(PairingTTL))

	payload := NewPairingPayload()
	payload.DeviceID = deviceID
	payload.Endpoint = endpoint
	payload.PairingCode = code
	payload.ExpiresAt = expires
	payload.HostFingerprint = host
	if err := payload.Validate(); err != nil {
		return PairingPayload{}, err
	}

	rec := &deviceRecord{
		DeviceID:               deviceID,
		Label:                  label,
		Platform:               platform,
		State:                  DevicePending,
		PairingCodeFingerprint: Fingerprint(code),
		PairingExpiresAt:       expires,
		CreatedAt:              now,
	}
	// The record is validated through its own projection before it is written,
	// so a record that could never be rendered as a Device never reaches disk.
	if d := rec.device(); d.Validate() != nil {
		return PairingPayload{}, d.Validate()
	}

	err = r.mutate(func(f *registryFile) (receipt, error) {
		// Two pairings inside the same second share the identifier's date-time
		// half and are separated only by its random tail. The odds are
		// negligible and the consequence would not be — a second device
		// shadowing the first in every lookup — so it is checked rather than
		// assumed.
		if f.find(deviceID) != nil {
			return receipt{}, fmt.Errorf("companion: device id %s is already registered; run pair again", deviceID)
		}
		// The bound is enforced on the way in as well as on the way out.
		// Checking it only on load would leave the file free to grow past a
		// limit that then makes it unreadable.
		if len(f.Devices) >= MaxDevices {
			return receipt{}, fmt.Errorf("companion: %d devices are registered, the limit is %d; "+
				"revoke one with `mora companion revoke` before pairing another", len(f.Devices), MaxDevices)
		}
		f.Devices = append(f.Devices, rec)
		return receipt{Event: "paired", DeviceID: deviceID, At: now}, nil
	})
	// A receipt failure is not a pairing failure. The device is registered and
	// the code in this payload is the only copy there will ever be, so it is
	// returned alongside the warning rather than thrown away; discarding it
	// would leave a pending device nobody can complete.
	if err != nil && !errors.Is(err, ErrReceiptNotWritten) {
		return PairingPayload{}, err
	}
	return payload, err
}

// Confirm spends a pairing code and mints the bearer token.
//
// The returned token is the only copy; the registry keeps its fingerprint.
// Confirm is not idempotent by design: a second confirmation for the same
// device finds it active, not pending, and is refused with ErrNotPending. A
// replayed confirmation that minted a second live token would double the
// credentials in circulation for one device and make revocation a guess.
func (r *Registry) Confirm(c PairingConfirmation) (token string, dev Device, err error) {
	if err := c.Validate(); err != nil {
		return "", Device{}, err
	}
	token, err = r.newSecret(tokenEntropyBytes)
	if err != nil {
		return "", Device{}, err
	}
	now := r.stamp()
	var expiredCode bool

	err = r.mutate(func(f *registryFile) (receipt, error) {
		rec := f.find(c.DeviceID)
		if rec == nil {
			return receipt{}, ErrNoSuchDevice
		}
		if rec.State != DevicePending || rec.PairingCodeFingerprint == "" {
			return receipt{}, ErrNotPending
		}
		// Compare first, THEN check expiry. The other order answers "was that
		// code right?" for free to anyone who can make a device expire, and the
		// comparison is the part that must not be skippable.
		match := subtle.ConstantTimeCompare(
			[]byte(rec.PairingCodeFingerprint),
			[]byte(Fingerprint(c.PairingCode)),
		) == 1
		expired := r.expired(rec.PairingExpiresAt)
		if !match {
			return receipt{}, ErrPairingCode
		}
		if expired {
			// The code is burned, not merely late. Leaving it in place would
			// make the TTL depend on the system clock never moving backwards,
			// and on a laptop the clock is user-controlled: set the date back
			// and yesterday's photographed QR code works again. Note that this
			// only fires for a code that MATCHED, so a wrong guess can never
			// burn somebody else's pending pairing.
			rec.PairingCodeFingerprint = ""
			expiredCode = true
			return receipt{Event: "expired", DeviceID: c.DeviceID, At: now}, nil
		}

		rec.State = DeviceActive
		rec.TokenFingerprint = Fingerprint(token)
		rec.PairingCodeFingerprint = ""
		rec.PairingExpiresAt = ""
		rec.Label = c.Label
		rec.Platform = c.Platform
		rec.PublicKey = c.PublicKey
		dev = rec.device()
		if err := dev.Validate(); err != nil {
			return receipt{}, err
		}
		return receipt{Event: "confirmed", DeviceID: c.DeviceID, At: now}, nil
	})
	unaudited := errors.Is(err, ErrReceiptNotWritten)
	if err != nil && !unaudited {
		return "", Device{}, err
	}
	if expiredCode {
		// The burn was committed, so the refusal is reported after the write
		// rather than instead of it. No credential was minted, so there is no
		// holder to strand; the audit warning is joined on so a caller can
		// still see that the row is missing.
		if unaudited {
			return "", Device{}, errors.Join(ErrPairingExpired, err)
		}
		return "", Device{}, ErrPairingExpired
	}
	// The device is ACTIVE in the record file and this token is the only copy
	// of its credential. Returning ("", err) here because the audit row failed
	// would strand exactly the credential nobody holds that the ordering exists
	// to prevent, so the token goes back with the warning attached.
	return token, dev, err
}

// Authenticate resolves a bearer token to its device.
//
// Every stored fingerprint is compared, at full width, with no exit before the
// loop and none inside it, so the time taken does not depend on the token, on
// which device matched, or on how far down the file it sits. The two shapes
// that used to break that are both gone: an early return for the empty string
// answered "was that even a token?" before any comparison ran, and a `continue`
// past a credential-less device made the loop's cost depend on how many devices
// carried one.
//
// A revoked device keeps its fingerprint precisely so this can answer
// "revoked_device" rather than "unknown_device": an operator who cannot tell a
// stale client from an attacker cannot respond to either.
func (r *Registry) Authenticate(token string) (Device, error) {
	f, err := r.load()
	if err != nil {
		return Device{}, err
	}
	// A per-call random digest stands in for any stored fingerprint that is not
	// canonical — an empty one on a pending device, a truncated one in a
	// corrupt file. It is random rather than a fixed constant so it cannot be
	// matched deliberately, and it is the same width as a real digest so
	// ConstantTimeCompare never short-circuits on a length mismatch.
	seed, err := r.newSecret(tokenEntropyBytes)
	if err != nil {
		return Device{}, err
	}
	absent := []byte(Fingerprint(seed))

	// Fingerprint is fixed width: every input, the empty string included,
	// produces the same "sha256:<64 hex>" string. Hashing unconditionally is
	// what lets an empty or malformed token cost exactly what a real one costs.
	want := []byte(Fingerprint(token))

	matched := -1
	for i, rec := range f.Devices {
		stored := []byte(rec.TokenFingerprint)
		if len(stored) != len(absent) {
			stored = absent
		}
		if subtle.ConstantTimeCompare(stored, want) == 1 {
			matched = i
		}
	}
	if matched < 0 {
		return Device{}, &AuthError{Reason: ReasonUnknownDevice}
	}
	rec := f.Devices[matched]
	if rec.State != DeviceActive {
		return Device{}, &AuthError{Reason: ReasonRevokedDevice}
	}
	return rec.device(), nil
}

// MarkSeen records that a device just authenticated. It is separate from
// Authenticate so the read path stays a read: a listener that wrote the record
// file on every request would serialize its own traffic behind one lock.
func (r *Registry) MarkSeen(deviceID string) error {
	now := r.stamp()
	return r.mutate(func(f *registryFile) (receipt, error) {
		rec := f.find(deviceID)
		if rec == nil {
			return receipt{}, ErrNoSuchDevice
		}
		if rec.LastSeenAt == now {
			return receipt{}, errNoChange
		}
		rec.LastSeenAt = now
		// No receipt: last_seen_at moves on every request a phone makes, and an
		// audit row per request is a log, not an audit trail.
		return receipt{}, nil
	})
}

// Revoke ends a device's access. It reports whether this call was the one that
// changed anything, so a caller can tell "revoked now" from "already revoked"
// without the second case being an error — re-revoking under uncertainty is
// exactly what an operator should be able to do freely.
//
// The token fingerprint survives revocation on purpose; see Authenticate.
func (r *Registry) Revoke(deviceID string) (dev Device, changed bool, err error) {
	if err := validateID("device_id", PrefixDevice, deviceID); err != nil {
		return Device{}, false, err
	}
	now := r.stamp()

	err = r.mutate(func(f *registryFile) (receipt, error) {
		rec := f.find(deviceID)
		if rec == nil {
			return receipt{}, ErrNoSuchDevice
		}
		if rec.State == DeviceRevoked {
			dev, changed = rec.device(), false
			return receipt{}, errNoChange
		}
		rec.State = DeviceRevoked
		rec.RevokedAt = now
		// A half-finished pairing is revoked too: the code dies with the
		// device, or a revoked device could be brought back by a QR code
		// somebody photographed a minute earlier.
		rec.PairingCodeFingerprint = ""
		rec.PairingExpiresAt = ""
		dev, changed = rec.device(), true
		if err := dev.Validate(); err != nil {
			return receipt{}, err
		}
		return receipt{Event: "revoked", DeviceID: deviceID, At: now}, nil
	})
	// The revocation committed; only its audit row did not. Reporting failure
	// here would tell an operator their revocation did not take when it did,
	// which is the same class of lie as a receipt that outlives a failed
	// commit — just pointed the other way.
	if err != nil && !errors.Is(err, ErrReceiptNotWritten) {
		return Device{}, false, err
	}
	return dev, changed, err
}

// List returns every device, newest registration last. Pending devices whose
// code has expired are reported as they are stored — expiry is a property of
// the code, not a state transition, so a device does not silently change state
// between two reads that did nothing.
func (r *Registry) List() ([]Device, error) {
	f, err := r.load()
	if err != nil {
		return nil, err
	}
	out := make([]Device, 0, len(f.Devices))
	for _, rec := range f.Devices {
		out = append(out, rec.device())
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].DeviceID < out[j].DeviceID
	})
	return out, nil
}

// Status is the registry summary: counts and whether a pairing window is open
// right now.
//
// It carries no path, because a status line ends up in screenshots and bug
// reports and a home directory is not something to publish there. It carries no
// host fingerprint either: that value is random per install, so a document
// containing it can never be frozen against a golden, and the moment it is
// actually needed is the pairing moment, where PairingPayload already carries
// it for the human comparing two screens. Call HostFingerprint directly if a
// later caller needs it on its own.
type Status struct {
	Active            int    `json:"active"`
	Pending           int    `json:"pending"`
	Revoked           int    `json:"revoked"`
	PairingOpen       bool   `json:"pairing_open"`
	NextPairingExpiry string `json:"next_pairing_expiry,omitempty"`
}

// Status summarizes the registry.
func (r *Registry) Status() (Status, error) {
	f, err := r.load()
	if err != nil {
		return Status{}, err
	}
	var s Status
	for _, rec := range f.Devices {
		switch rec.State {
		case DeviceActive:
			s.Active++
		case DevicePending:
			s.Pending++
			if rec.PairingExpiresAt == "" || r.expired(rec.PairingExpiresAt) {
				continue
			}
			s.PairingOpen = true
			if s.NextPairingExpiry == "" || rec.PairingExpiresAt < s.NextPairingExpiry {
				s.NextPairingExpiry = rec.PairingExpiresAt
			}
		case DeviceRevoked:
			s.Revoked++
		}
	}
	return s, nil
}

// HostFingerprint returns the stable identity of this Mac, creating the seed on
// first use. It is what a phone pins so a pairing code replayed against a
// different host is visible to the human comparing two printed fingerprints.
//
// The seed is random rather than derived from a hostname or a machine id: a
// derived fingerprint changes when the user renames their Mac, and a phone
// would read that rename as a host substitution.
func (r *Registry) HostFingerprint() (string, error) {
	// Fast path: the seed exists, so no lock is needed. writeSecretFile
	// publishes by rename, so a concurrent reader sees either no file or the
	// whole file, never a partial one.
	if seed, err := r.readHostSeed(); err == nil {
		return Fingerprint(string(seed)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	// Creation goes through the registry lock. Publishing by rename is not
	// enough on its own: Windows refuses a rename onto a path that exists or
	// that another process has open, which is exactly what two concurrent first
	// pairings produce. Under the lock the destination provably does not exist.
	if err := r.ensureDir(); err != nil {
		return "", err
	}
	held, err := r.lock()
	if err != nil {
		return "", err
	}
	defer held.release()

	// Re-read inside the lock: whoever held it before this call may have been
	// creating the very seed this call was about to generate.
	if seed, err := r.readHostSeed(); err == nil {
		return Fingerprint(string(seed)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	seed := make([]byte, tokenEntropyBytes)
	if _, err := io.ReadFull(r.entropy, seed); err != nil {
		return "", fmt.Errorf("companion: read entropy: %w", err)
	}
	if err := writeSecretFile(r.hostKeyPath(), seed, func() error {
		if !held.stillOwns() {
			return ErrLocked
		}
		return nil
	}); err != nil {
		return "", err
	}
	return Fingerprint(string(seed)), nil
}

// readHostSeed returns the stored seed. A file of the wrong size is a corrupt
// identity, and it is an error rather than a reason to mint a new one: silently
// rotating the host identity would make every already-paired phone see what
// looks like a host substitution.
func (r *Registry) readHostSeed() ([]byte, error) {
	// The mode is asserted before the bytes are read, not after. Reading first
	// and chmodding second leaves the seed readable for exactly as long as it
	// takes to read it, which is the only window that matters.
	if err := r.hardenExisting(); err != nil {
		return nil, err
	}
	if err := hardenPath(r.hostKeyPath(), secretFileMode); err != nil {
		return nil, err
	}
	seed, err := os.ReadFile(r.hostKeyPath())
	if err != nil {
		return nil, err
	}
	if len(seed) != tokenEntropyBytes {
		return nil, fmt.Errorf("companion: %s is %d bytes, not %d — the host identity is corrupt; "+
			"move it aside and re-pair every device", r.hostKeyPath(), len(seed), tokenEntropyBytes)
	}
	return seed, nil
}

func (f *registryFile) find(id string) *deviceRecord {
	for _, rec := range f.Devices {
		if rec.DeviceID == id {
			return rec
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func (r *Registry) dir() string         { return filepath.Join(r.configDir, "companion") }
func (r *Registry) filePath() string    { return filepath.Join(r.dir(), "devices.json") }
func (r *Registry) lockPath() string    { return filepath.Join(r.dir(), ".lock") }
func (r *Registry) hostKeyPath() string { return filepath.Join(r.dir(), "host.key") }
func (r *Registry) receiptDir() string {
	return filepath.Join(r.stateDir, "companion", "receipts")
}

// ensureDir creates the credential directory and asserts the mode of BOTH it
// and its parent.
//
// Hardening only the subdirectory was not enough. A ConfigDir that already
// existed at 0755 — made by an older mora, restored from an archive, unpacked
// by an installer — lets another local account list it, watch `companion`
// appear, and see the file count and timestamps change on every pairing and
// revocation. 0700 on the parent is also the mode internal/config already
// creates it with, so this repairs drift rather than inventing a policy.
func (r *Registry) ensureDir() error {
	if err := os.MkdirAll(r.configDir, secretDirMode); err != nil {
		return err
	}
	if err := hardenPath(r.configDir, secretDirMode); err != nil {
		return err
	}
	if err := os.MkdirAll(r.dir(), secretDirMode); err != nil {
		return err
	}
	// MkdirAll honours the umask, and a directory restored from an archive
	// carries whatever mode the archive held, so the mode is asserted rather
	// than assumed.
	return hardenPath(r.dir(), secretDirMode)
}

// hardenExisting asserts the modes of whatever part of the tree is already
// there, without creating anything. Read paths use it: `mora companion list`
// before the first pairing must not conjure a credential directory, but if one
// exists it must not be read through a permissive parent either.
func (r *Registry) hardenExisting() error {
	for _, path := range []string{r.configDir, r.dir()} {
		if err := hardenPath(path, secretDirMode); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// load reads the record file. A missing file is an empty registry, not an
// error: `mora companion list` before the first pairing is a legitimate call.
func (r *Registry) load() (*registryFile, error) {
	// Every mode is repaired BEFORE the read, not after, and the parent
	// directories come first: hardening a file the process has already read
	// leaves the whole read window at whatever mode the file was found in, and
	// hardening a file inside a listable directory leaves its existence and
	// size public for the same window.
	if err := r.hardenExisting(); err != nil {
		return nil, err
	}
	if err := hardenPath(r.filePath(), secretFileMode); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// The size is checked before the bytes are read. Authenticate runs this on
	// every request N12's listener serves, so an oversized record file would be
	// a request-rate memory amplifier long before it was a parse error.
	info, err := os.Stat(r.filePath())
	if err == nil && info.Size() > MaxRegistryBytes {
		return nil, fmt.Errorf("companion: the device registry is %d bytes, over the %d-byte limit; "+
			"this is a corrupt or hostile file, not a large one — move it aside and pair again",
			info.Size(), MaxRegistryBytes)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	body, err := os.ReadFile(r.filePath())
	if errors.Is(err, os.ErrNotExist) {
		return &registryFile{Version: registryFileVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	var f registryFile
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("companion: %s is not readable as a device registry (%w); "+
			"move it aside and pair again", r.filePath(), err)
	}
	if f.Version > registryFileVersion {
		return nil, fmt.Errorf("companion: device registry is version %d, this build understands %d — upgrade mora",
			f.Version, registryFileVersion)
	}
	if len(f.Devices) > MaxDevices {
		return nil, fmt.Errorf("companion: the device registry holds %d devices, over the %d limit; "+
			"revoke what you do not recognize, or move the file aside and pair again",
			len(f.Devices), MaxDevices)
	}
	return &f, nil
}

// mutate runs fn against the record file under the cross-process lock and
// writes the result atomically.
//
// fn RETURNS the receipt rather than the caller passing one in, because only fn
// knows whether anything moved: a re-revocation of an already-revoked device
// must not leave a second audit row saying it did. An empty Event writes none.
//
// The read happens INSIDE the lock: reading first and locking second is the
// lost-update bug this exists to prevent, where two `mora companion pair` runs
// each write a file containing only their own device.
func (r *Registry) mutate(fn func(*registryFile) (receipt, error)) error {
	if err := r.ensureDir(); err != nil {
		return err
	}
	held, err := r.lock()
	if err != nil {
		return err
	}
	defer held.release()

	f, err := r.load()
	if err != nil {
		return err
	}
	rcpt, err := fn(f)
	if errors.Is(err, errNoChange) {
		// Nothing moved, so nothing is written. Rewriting the file for a no-op
		// would re-serialize whatever was already there — including a record an
		// older build wrote that no longer validates — and would stamp an audit
		// row on an event that did not happen.
		return nil
	}
	if err != nil {
		return err
	}
	f.Version = registryFileVersion
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	// The size is checked here, on the way out, and not only on the way in.
	// MaxDevices bounds the count and says nothing about the bytes: a registry
	// well under both limits when it was written compactly can cross
	// MaxRegistryBytes once it is re-indented and one more device is appended,
	// and the result is a file this build will refuse to LOAD — a registry
	// bricked by its own write. Refusing the write is the only outcome that
	// leaves the vault readable.
	if len(body) > MaxRegistryBytes {
		return fmt.Errorf("%w: this change would write %d bytes, over the %d-byte limit; "+
			"revoke a device with `mora companion revoke` first", ErrRegistryTooLarge, len(body), MaxRegistryBytes)
	}

	// stillOwns is checked immediately before each rename rather than once
	// here, because a check here answers a question about a moment that has
	// already passed by the time the rename runs. See writeSecretFile.
	stillHolds := func() error {
		if !held.stillOwns() {
			return ErrLocked
		}
		return nil
	}
	commit := func() error { return r.writeRecord(r.filePath(), body, stillHolds) }

	// The record file is the single source of truth, so it commits FIRST,
	// always, for every event. Two invariants have to hold:
	//
	//	(a) a receipt must never assert a state the record file does not hold;
	//	(b) a committed change must never be discarded because its audit failed.
	//
	// Record-first is what makes (a) unconditional. An earlier design wrote
	// Confirm's receipt first, reasoning that a receipt failure would otherwise
	// strand an ACTIVE credential nobody holds — but the fix for that is not to
	// reorder the writes, it is to stop throwing the credential away. A
	// `confirmed` row over a still-pending device is an audit trail that lies,
	// and there is no arrangement of two writes that makes a lying audit trail
	// the better failure.
	//
	// So (b) is satisfied by the RETURN, not by the ordering. A receipt failure
	// after a successful commit surfaces as ErrReceiptNotWritten, which callers
	// treat as a warning: the change is real and durable, its audit row is not,
	// and the caller must still hand back the token or the pairing code it just
	// minted. Every mutating method here does exactly that.
	if rcpt.Event == "" {
		return commit()
	}
	if err := commit(); err != nil {
		return err
	}
	if err := r.writeAudit(rcpt, stillHolds); err != nil {
		return fmt.Errorf("%w: %v", ErrReceiptNotWritten, err)
	}
	return nil
}

// lockTimeout / lockPoll bound how long a caller waits for the write lock. The
// operations under it are a read, a small mutation and two file writes, so a
// wait past this is a wedged peer rather than a busy one.
const (
	lockTimeout = 5 * time.Second
	lockPoll    = 20 * time.Millisecond
)

// lock takes the cross-process write lock.
//
// The lock is held by the operating system on an open handle — flock on POSIX,
// a zero share mode on Windows — and released when that handle closes,
// including when the process dies. See registry_lock_unix.go for why the
// previous O_EXCL-plus-staleness-sweep design could not be made correct: any
// rule that lets one process take a lock away from another is a check-then-use
// race on both sides of the handoff.
func (r *Registry) lock() (*lockFile, error) {
	return acquireLock(r.lockPath(), secretFileMode, lockTimeout, lockPoll)
}

// receipt is one audit row. It names the device and the moment, never the
// credential and never the payload, so the receipts directory stays safe to
// include in a bug report.
type receipt struct {
	Event    string `json:"event"`
	DeviceID string `json:"device_id"`
	At       string `json:"at"`
}

func (r *Registry) writeReceipt(rcpt receipt, beforeRename func() error) error {
	if err := os.MkdirAll(r.receiptDir(), secretDirMode); err != nil {
		return err
	}
	if err := hardenPath(r.receiptDir(), secretDirMode); err != nil {
		return err
	}
	suffix, err := r.newSecret(4)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(struct {
		Header
		receipt
	}{Header: newHeader(schemaRegistryReceipt), receipt: rcpt}, "", "  ")
	if err != nil {
		return err
	}
	// Colons are legal in an RFC3339 stamp and illegal in a Windows filename,
	// so the compact form is used for the name and the exact stamp stays in the
	// body.
	stamp := strings.NewReplacer("-", "", ":", "").Replace(rcpt.At)
	name := fmt.Sprintf("%s-%s-%s-%s.json", stamp, rcpt.Event, rcpt.DeviceID, suffix)
	return writeSecretFile(filepath.Join(r.receiptDir(), name), append(body, '\n'), beforeRename)
}

// schemaRegistryReceipt names the audit row. It is not one of the published
// wire schemas: no device ever receives it, and it is versioned only so a later
// reader can tell what it is looking at.
const schemaRegistryReceipt = "mora.companion.registry.receipt"

// writeSecretFile writes body to path atomically at 0600.
//
// The mode is set on the temporary file BEFORE the rename, not on the final
// path after it: a chmod after rename leaves a window in which the secret is
// world-readable, and that window is exactly when a watcher would look.
//
// beforeRename, when non-nil, runs in the last instant before the rename
// publishes the file, and a non-nil return aborts the write with nothing
// published. Callers use it to re-assert that they still hold the write lock.
// The position is the whole point: a lock check anywhere earlier answers a
// question about a moment that has already passed by the time the rename runs,
// which is the window the round-two review found. It narrows the window to the
// gap between the check and one syscall; it does not close it, and nothing
// pretends otherwise.
func writeSecretFile(path string, body []byte, beforeRename func() error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(secretFileMode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if beforeRename != nil {
		if err := beforeRename(); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// The rename is atomic against other readers the moment it returns, and not
	// yet durable: the new file's blocks are on disk and the directory entry
	// that points at them may still be in cache. Without this a power loss
	// after a revocation can bring the revoked device back.
	if err := syncDir(dir); err != nil {
		return err
	}
	return hardenPath(path, secretFileMode)
}

// hardenPath re-asserts a mode and FAILS CLOSED if it does not take.
//
// A chmod can return nil and change nothing — on a filesystem mounted without
// permission support, on an exotic ACL, on a path somebody else owns — so the
// mode is read back and compared. Returning success there would leave the
// caller believing a credential is protected when it is world-readable, which
// is worse than refusing to store one at all.
//
// Windows is exempt from the read-back, not from the chmod. There os.Chmod only
// moves the read-only bit and Stat reports a synthesized mode, so the
// comparison could never succeed on a system that has no POSIX permissions to
// get wrong; exclusion there comes from the share mode and the ACL the profile
// directory already carries.
func hardenPath(path string, mode os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() == mode {
		return nil
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	after, err := os.Stat(path)
	if err != nil {
		return err
	}
	if after.Mode().Perm() != mode {
		return fmt.Errorf("companion: %s is mode %04o and will not stay %04o — "+
			"refusing to keep credentials somewhere the mode cannot be enforced",
			path, after.Mode().Perm(), mode)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Identifiers, secrets and time
// ---------------------------------------------------------------------------

// secretEncoding is unpadded, uppercase base32. Base32 rather than base64
// because these strings are read aloud and typed by hand in the fallback
// pairing flow, and base32's alphabet has no case-sensitive collisions.
var secretEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func (r *Registry) newSecret(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r.entropy, buf); err != nil {
		return "", fmt.Errorf("companion: read entropy: %w", err)
	}
	return secretEncoding.EncodeToString(buf), nil
}

// newDeviceID mints an identifier in the shape the rest of Mora already
// generates: a kind prefix, a date, a time, and a random hex tail
// (`dev_20260903_120000_a1b2c3d4`). Matching that shape is not cosmetic — the
// contract corpus normalizes exactly this pattern, so a device id in any other
// shape would make `mora companion list` unfreezable.
//
// Nothing may be parsed out of the opaque half; it is timestamped for the same
// reason every other Mora id is, so a human reading a log can order two of them.
func (r *Registry) newDeviceID() (string, error) {
	tail := make([]byte, deviceIDEntropyBytes)
	if _, err := io.ReadFull(r.entropy, tail); err != nil {
		return "", fmt.Errorf("companion: read entropy: %w", err)
	}
	return fmt.Sprintf("%s%s_%s", PrefixDevice, r.now().UTC().Format("20060102_150405"), hex.EncodeToString(tail)), nil
}

// stamp renders the current time in the one timestamp format this package
// publishes: RFC3339, UTC, second precision.
func (r *Registry) stamp() string { return r.stampAt(r.now()) }

func (r *Registry) stampAt(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// expired reports whether a stored deadline has passed. An unparseable or
// missing deadline counts as expired: a pairing window nobody can read the end
// of is not a window that should stay open.
func (r *Registry) expired(deadline string) bool {
	if deadline == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, deadline)
	if err != nil {
		return true
	}
	// Equality counts as expired. A deadline that is still valid AT the
	// deadline is a deadline one second longer than the one published.
	return !r.now().Before(t)
}
