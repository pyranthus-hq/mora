package companion

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// This file builds the canonical fixture for every published schema. The
// fixtures are the contract's executable half: they are frozen under
// testdata/v1/, the Go tests decode them, and the Swift client decodes the same
// bytes (graph node N14). A fixture that only Go can read proves nothing.
//
// Everything here is deterministic — fixed identifiers, fixed timestamps,
// fingerprints computed from the fixture text — so a regenerated golden differs
// only when the schema did.

// Fixed instants used by every fixture. They are far enough apart to make an
// ordering bug visible.
const (
	fixtureCreatedAt = "2026-09-03T11:00:00Z"
	fixtureUpdatedAt = "2026-09-03T11:04:00Z"
	fixtureExpiresAt = "2026-09-03T11:10:00Z"
	fixtureCaptureAt = "2026-09-03T10:59:30Z"
)

const (
	fixtureDeviceID    = "dev_01J8Z2K7Q4RN3T5V"
	fixtureRequestID   = "req_01J8Z2K7Q4RN3T5W"
	fixtureReceiptID   = "rcp_01J8Z2K7Q4RN3T5X"
	fixtureMemoryID    = "mem_01J8Z2K7Q4RN3T5Y"
	fixtureExecutionID = "exe_01J8Z2K7Q4RN3T5Z"
	fixtureIdemKey     = "ios.capture.2026-09-03T10:59:30Z.7f3a"
	fixtureText        = "Ravi wants the pilot scoped to one team before the October board meeting."
)

// Fingerprint returns the payload fingerprint for a capture's text. It is the
// published derivation: sha256 over the exact UTF-8 bytes the device sent,
// rendered lowercase with a "sha256:" prefix. A device computes it, the kernel
// recomputes it, and a mismatch on a repeated idempotency key is
// ReasonIdempotencyConflict.
func Fingerprint(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func fixtureFreshness() []SourceFreshness {
	return []SourceFreshness{
		{Key: "gmail:work", State: FreshnessFresh, AgeSeconds: 900, LastSuccessAt: "2026-09-03T10:45:00Z"},
		{Key: "calendar", State: FreshnessStale, AgeSeconds: 97200, LastSuccessAt: "2026-09-02T08:00:00Z"},
		{Key: "imessage", State: FreshnessNever, AgeSeconds: -1, ErrorCode: "full_disk_access_missing"},
	}
}

func fixtureEvidence() []Evidence {
	return []Evidence{
		{
			MemoryID:   fixtureMemoryID,
			Source:     "gmail:work",
			OccurredAt: "2026-09-03T09:12:00Z",
			Snippet:    "Ravi: let us keep the pilot to the platform team until the board meets.",
			DeepLink:   "mora://memory/mem_01J8Z2K7Q4RN3T5Y",
		},
	}
}

// DeviceFixture is the canonical paired device.
func DeviceFixture() *Device {
	d := NewDevice()
	d.DeviceID = fixtureDeviceID
	d.Label = "Adit iPhone"
	d.Platform = PlatformIOS
	d.State = DeviceActive
	d.TokenFingerprint = Fingerprint("fixture-bearer-token")
	d.CreatedAt = fixtureCreatedAt
	d.LastSeenAt = fixtureUpdatedAt
	return &d
}

// PairingFixture is the canonical QR payload. The pairing code here is a
// fixture string, not a token format: the real format is a human decision gate.
func PairingFixture() *PairingPayload {
	p := NewPairingPayload()
	p.DeviceID = fixtureDeviceID
	p.Endpoint = "https://mora-mac.tail-scale.ts.net/v1/companion"
	p.PairingCode = "fixture-pairing-code"
	p.ExpiresAt = fixtureExpiresAt
	p.HostFingerprint = Fingerprint("fixture-host-key")
	return &p
}

// PairingConfirmationFixture is the canonical reply from the phone.
func PairingConfirmationFixture() *PairingConfirmation {
	p := NewPairingConfirmation()
	p.DeviceID = fixtureDeviceID
	p.PairingCode = "fixture-pairing-code"
	p.Label = "Adit iPhone"
	p.Platform = PlatformIOS
	p.PublicKey = "fixture-device-public-key"
	p.ConfirmedAt = fixtureCreatedAt
	return &p
}

// HealthFixture is the canonical health projection: one fresh source, one
// stale, one that never ran. A fixture where everything is green would let a
// client ship without ever rendering the honest cases.
func HealthFixture() *HealthProjection {
	h := NewHealthProjection()
	h.GeneratedAt = fixtureCreatedAt
	h.State = HealthDegraded
	h.Policy = PolicyPropose
	h.Index = IndexHealth{State: HealthHealthy, Memories: 2837, BuiltAt: "2026-09-03T10:50:00Z"}
	h.Sources = fixtureFreshness()
	return &h
}

// TodayFixture is the canonical Today projection.
func TodayFixture() *TodayProjection {
	t := NewTodayProjection()
	t.GeneratedAt = fixtureCreatedAt
	t.Health = HealthSummary{State: HealthDegraded, Policy: PolicyPropose}
	t.Items = []TodayItem{
		{
			ID:       "today.needs_attention.1",
			Kind:     ItemNeedsAttention,
			Title:    "Ravi is waiting on the pilot scope",
			Body:     "Asked twice this week; the last reply was six days ago.",
			Evidence: fixtureEvidence(),
		},
	}
	t.Freshness = fixtureFreshness()
	t.Truncated = true
	return &t
}

// ContextRequestFixture is the canonical inbound context request.
func ContextRequestFixture() *ContextRequest {
	c := NewContextRequest()
	c.Mode = ModeThink
	c.Query = "What did I promise Ravi about the pilot?"
	c.Scope = "project:pilot"
	return &c
}

// ContextFixture is the canonical context bundle. It carries a gap on purpose:
// a bundle that never says what the vault does not know is the failure mode
// this schema exists to prevent.
func ContextFixture() *ContextBundle {
	c := NewContextBundle()
	c.GeneratedAt = fixtureCreatedAt
	c.Mode = ModeThink
	c.Query = "What did I promise Ravi about the pilot?"
	c.Evidence = fixtureEvidence()
	c.Gaps = []string{"No calendar coverage after 2026-09-02; a scheduling reply may be missing."}
	c.Freshness = fixtureFreshness()
	c.SynthesisPrompt = "Answer only from the evidence below. Cite each claim by memory_id. Say what is missing."
	c.Health = HealthSummary{State: HealthDegraded, Policy: PolicyPropose}
	return &c
}

// CaptureFixture is the canonical inbound capture.
func CaptureFixture() *Capture {
	c := NewCapture()
	c.IdempotencyKey = fixtureIdemKey
	c.DeviceID = fixtureDeviceID
	c.CapturedAt = fixtureCaptureAt
	c.RequestedLane = LaneMemory
	c.Intent = IntentRemember
	c.Scope = "project:pilot"
	c.Text = fixtureText
	c.PayloadFingerprint = Fingerprint(fixtureText)
	return &c
}

// ReceiptFixture is the canonical applied receipt with its async job state.
func ReceiptFixture() *Receipt {
	r := NewReceipt()
	r.ReceiptID = fixtureReceiptID
	r.RequestID = fixtureRequestID
	r.IdempotencyKey = fixtureIdemKey
	r.DeviceID = fixtureDeviceID
	r.State = ReceiptApplied
	r.MemoryID = fixtureMemoryID
	r.PayloadFingerprint = Fingerprint(fixtureText)
	r.Policy = PolicyOpen
	r.ReceivedAt = fixtureCreatedAt
	r.SettledAt = fixtureUpdatedAt
	op := OperationFixture()
	status := op.Status()
	r.Operation = &status
	return &r
}

// OperationFixture is the canonical async operation envelope, mid-flight.
func OperationFixture() *Operation {
	o := NewOperation()
	o.RequestID = fixtureRequestID
	o.IdempotencyKey = fixtureIdemKey
	o.Kind = KindResearch
	o.Lane = LaneResearch
	o.Intent = IntentInvestigate
	o.State = StateRunning
	o.Payload = PayloadRef{
		Ref:         "capture:" + fixtureIdemKey,
		Fingerprint: Fingerprint(fixtureText),
		Bytes:       len(fixtureText),
		MediaType:   "text/plain; charset=utf-8",
	}
	o.Provenance = Provenance{
		Origin:     OriginCompanion,
		DeviceID:   fixtureDeviceID,
		Surface:    PlatformIOS,
		AcceptedAt: fixtureCreatedAt,
	}
	o.Attempt = Attempt{Count: 1, Max: 3, ExecutionID: fixtureExecutionID}
	o.CreatedAt = fixtureCreatedAt
	o.UpdatedAt = fixtureUpdatedAt
	o.Freshness = fixtureFreshness()
	return &o
}

// ResearchCaptureFixture is a capture routed to the async lane.
func ResearchCaptureFixture() *Capture {
	c := CaptureFixture()
	c.IdempotencyKey = "ios.capture.2026-09-03T10:59:31Z.9b12"
	c.RequestedLane = LaneResearch
	c.Intent = IntentInvestigate
	return c
}

// RevokedDeviceFixture is a device after `mora companion revoke`.
func RevokedDeviceFixture() *Device {
	d := DeviceFixture()
	d.State = DeviceRevoked
	d.RevokedAt = fixtureUpdatedAt
	return d
}

// AcceptedReceiptFixture is what the propose write policy returns: the capture
// is staged for local approval and nothing is in the vault yet.
func AcceptedReceiptFixture() *Receipt {
	r := ReceiptFixture()
	r.State = ReceiptAccepted
	r.Policy = PolicyPropose
	r.MemoryID = ""
	r.SettledAt = ""
	r.Operation = nil
	return r
}

// RejectedReceiptFixture is what the readonly write policy returns.
func RejectedReceiptFixture() *Receipt {
	r := ReceiptFixture()
	r.State = ReceiptRejected
	r.Reason = ReasonPolicy
	r.Policy = PolicyReadonly
	r.MemoryID = ""
	r.Operation = nil
	return r
}

// DoneOperationFixture is a completed async operation.
func DoneOperationFixture() *Operation {
	o := OperationFixture()
	o.State = StateDone
	o.Result = &OperationResult{ReceiptID: fixtureReceiptID, MemoryID: fixtureMemoryID}
	return o
}

// FailedOperationFixture is an async operation that exhausted its attempts.
func FailedOperationFixture() *Operation {
	o := OperationFixture()
	o.State = StateFailed
	o.Attempt = Attempt{Count: 3, Max: 3, ExecutionID: fixtureExecutionID}
	o.Result = &OperationResult{ErrorCode: ReasonUnavailable}
	return o
}

// fixtures maps every frozen document to its builder. A key is a schema name,
// or "<schema>#<variant>" for a second shape of the same schema — the honest
// cases (rejected, failed, revoked) are frozen too, because a client that only
// ever decoded the happy path has not been tested.
//
// A schema with no base entry fails TestGoldenCorpusIsComplete, so nothing
// ships unfrozen.
var fixtures = map[string]func() Payload{
	SchemaDevice:                func() Payload { return DeviceFixture() },
	SchemaDevice + "#revoked":   func() Payload { return RevokedDeviceFixture() },
	SchemaPairing:               func() Payload { return PairingFixture() },
	SchemaPairingOK:             func() Payload { return PairingConfirmationFixture() },
	SchemaHealth:                func() Payload { return HealthFixture() },
	SchemaToday:                 func() Payload { return TodayFixture() },
	SchemaContextRq:             func() Payload { return ContextRequestFixture() },
	SchemaContext:               func() Payload { return ContextFixture() },
	SchemaCapture:               func() Payload { return CaptureFixture() },
	SchemaCapture + "#research": func() Payload { return ResearchCaptureFixture() },
	SchemaReceipt:               func() Payload { return ReceiptFixture() },
	SchemaReceipt + "#accepted": func() Payload { return AcceptedReceiptFixture() },
	SchemaReceipt + "#rejected": func() Payload { return RejectedReceiptFixture() },
	SchemaOperation:             func() Payload { return OperationFixture() },
	SchemaOperation + "#done":   func() Payload { return DoneOperationFixture() },
	SchemaOperation + "#failed": func() Payload { return FailedOperationFixture() },
}

// SchemaNames returns every published schema name, sorted. Variants are not
// schemas; they are second shapes of one.
func SchemaNames() []string {
	seen := map[string]bool{}
	names := make([]string, 0, len(fixtures))
	for key := range fixtures {
		name, _, _ := strings.Cut(key, "#")
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// FixtureNames returns every frozen document key, sorted.
func FixtureNames() []string {
	names := make([]string, 0, len(fixtures))
	for key := range fixtures {
		names = append(names, key)
	}
	sort.Strings(names)
	return names
}

// FixtureFor returns the canonical fixture for a document key.
func FixtureFor(key string) (Payload, bool) {
	build, ok := fixtures[key]
	if !ok {
		return nil, false
	}
	return build(), true
}

// GoldenPath returns the committed fixture path for a document key, relative to
// the package directory.
func GoldenPath(key string) string {
	return "testdata/v1/" + strings.ReplaceAll(key, "#", ".") + ".json"
}
