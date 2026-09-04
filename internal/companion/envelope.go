package companion

import (
	"fmt"
	"strings"
)

// This file holds the async operation envelope: the one seam that couples the
// companion program to the agent spine (graph decision D6).
//
// Two properties make it a seam rather than a leak:
//
//  1. It is content-free. The envelope carries a reference to the payload — a
//     fingerprint, a byte count, a media type — and never the user's text.
//     A queue, a lease table, a sweeper log and a phone Activity row can all
//     hold an envelope without holding a word the user wrote.
//     TestOperationCarriesNoUserText enforces it.
//
//  2. It names no executor. There is no worker, host, model, or provider field.
//     The phone learns that a job is running and nothing about what is running
//     it, so the spine can change runtimes without a client release and without
//     a privacy story to rewrite. TestOperationNamesNoExecutor enforces it.

// OperationKind is what the kernel will actually do. The kernel assigns it; the
// device's Intent does not.
type OperationKind string

const (
	// KindCapture writes a memory into the vault under the write policy.
	KindCapture OperationKind = "capture"
	// KindAsk builds a grounded context bundle for a shell to reason over.
	KindAsk OperationKind = "ask"
	// KindResearch hands the work to the async lane and returns a result
	// later.
	KindResearch OperationKind = "research"
)

// OperationState is the job state machine a receipt renders.
//
//	captured -> triaged -> leased -> running -> done | needs_input | failed
//
// A lease that expires returns the job to triaged rather than stranding it, so
// no operation is ever silently claimed forever.
type OperationState string

const (
	StateCaptured   OperationState = "captured"
	StateTriaged    OperationState = "triaged"
	StateLeased     OperationState = "leased"
	StateRunning    OperationState = "running"
	StateDone       OperationState = "done"
	StateNeedsInput OperationState = "needs_input"
	StateFailed     OperationState = "failed"
)

// transitions is the whole state machine. A state missing from the map is
// terminal.
var transitions = map[OperationState][]OperationState{
	StateCaptured: {StateTriaged, StateFailed},
	StateTriaged:  {StateLeased, StateFailed},
	// leased -> triaged is lease expiry: the claim lapsed and the job goes
	// back to the pool.
	StateLeased: {StateRunning, StateTriaged, StateFailed},
	// running -> triaged is the same lapse seen from a worker that died
	// mid-execution.
	StateRunning:    {StateDone, StateNeedsInput, StateFailed, StateTriaged},
	StateNeedsInput: {StateRunning, StateFailed},
}

// IsTerminal reports whether an operation will not move again on its own.
// needs_input is not terminal: it is waiting for a human, and it resumes.
func (s OperationState) IsTerminal() bool {
	return s == StateDone || s == StateFailed
}

// AwaitsUser reports whether the operation stopped because it needs an answer.
// A shell must render this differently from running: a spinner over a question
// nobody was asked is the dishonest case.
func (s OperationState) AwaitsUser() bool { return s == StateNeedsInput }

// ValidateTransition reports whether from -> to is a legal move. A no-op move
// (from == to) is legal so a heartbeat or a re-delivered update is idempotent.
func ValidateTransition(from, to OperationState) error {
	if err := inVocabulary("operation_state", string(from), "from"); err != nil {
		return err
	}
	if err := inVocabulary("operation_state", string(to), "to"); err != nil {
		return err
	}
	if from == to {
		return nil
	}
	for _, allowed := range transitions[from] {
		if allowed == to {
			return nil
		}
	}
	return errf(CodeInvalidState, "state", "%s cannot move to %s", from, to)
}

// NextStates returns the legal successors of s, for a shell that renders what
// can happen next. The returned slice is a copy.
func NextStates(s OperationState) []OperationState {
	out := make([]OperationState, len(transitions[s]))
	copy(out, transitions[s])
	return out
}

// PayloadRef is the bounded reference that stands in for the user's text.
//
// Ref is an opaque locator the kernel resolves; it is restricted to the opaque
// character set so it cannot become a second, unbounded text field.
type PayloadRef struct {
	Ref         string `json:"ref"`
	Fingerprint string `json:"fingerprint"`
	Bytes       int    `json:"bytes"`
	MediaType   string `json:"media_type"`
}

func (p PayloadRef) validate(field string) error {
	if err := validateText(field+".ref", p.Ref, MaxIdempotencyKeyBytes, true); err != nil {
		return err
	}
	if !opaqueRunes(p.Ref) {
		return errf(CodeInvalidValue, field+".ref", "payload reference carries characters outside [A-Za-z0-9_.:-]")
	}
	if err := validateFingerprint(field+".fingerprint", p.Fingerprint); err != nil {
		return err
	}
	if p.Bytes < 0 {
		return errf(CodeInvalidValue, field+".bytes", "size cannot be negative")
	}
	if p.Bytes > MaxCaptureTextBytes {
		return errf(CodeTooLarge, field+".bytes", "payload exceeds %d bytes", MaxCaptureTextBytes)
	}
	return inVocabulary("media_type", p.MediaType, field+".media_type")
}

// Provenance says where an operation came from. Every field is a coarse,
// non-identifying token: a device id the user can revoke, a surface family, and
// the time the kernel took responsibility.
//
// Origin is stamped by the kernel from the authenticated caller, never read
// from the request body — that is what stops a device claiming its capture came
// from the CLI.
type Provenance struct {
	Origin     string   `json:"origin"`
	DeviceID   string   `json:"device_id,omitempty"`
	Surface    Platform `json:"surface"`
	AcceptedAt string   `json:"accepted_at"`
}

// Published origins.
const (
	OriginCompanion = "companion"
	OriginCLI       = "cli"
	OriginMCP       = "mcp"
)

func (p Provenance) validate(field string) error {
	if err := inVocabulary("origin", p.Origin, field+".origin"); err != nil {
		return err
	}
	if p.Origin == OriginCompanion && p.DeviceID == "" {
		return errf(CodeMissingField, field+".device_id", "a companion operation names its device")
	}
	if err := validateOptionalID(field+".device_id", PrefixDevice, p.DeviceID); err != nil {
		return err
	}
	if err := inVocabulary("platform", string(p.Surface), field+".surface"); err != nil {
		return err
	}
	return validateTimestamp(field+".accepted_at", p.AcceptedAt)
}

// Attempt is the retry and idempotency bookkeeping a result POST needs.
//
// ExecutionID is stable per attempt: it is what makes a re-delivered result
// idempotent when a worker retries. There is deliberately no lease owner here —
// see the note at the top of this file.
type Attempt struct {
	Count       int    `json:"count"`
	Max         int    `json:"max"`
	ExecutionID string `json:"execution_id,omitempty"`
}

func (a Attempt) validate(field string) error {
	if a.Count < 0 {
		return errf(CodeInvalidValue, field+".count", "count cannot be negative")
	}
	if a.Max <= 0 {
		return errf(CodeInvalidValue, field+".max", "an attempt cap is required and is positive")
	}
	if a.Max > MaxAttempts {
		return errf(CodeTooLarge, field+".max", "the retry cap is at most %d", MaxAttempts)
	}
	if a.Count > a.Max {
		return errf(CodeInvalidState, field+".count", "attempts exceed the cap")
	}
	return validateOptionalID(field+".execution_id", PrefixExecution, a.ExecutionID)
}

// OperationResult is the terminal outcome, still content-free: it names the
// receipt and the memory, and reports failure as a code rather than as prose a
// worker wrote.
type OperationResult struct {
	ReceiptID string       `json:"receipt_id,omitempty"`
	MemoryID  string       `json:"memory_id,omitempty"`
	ErrorCode RejectReason `json:"error_code,omitempty"`
}

// Operation is the versioned async operation envelope.
//
// RequestID is the operation's identity for its whole life; IdempotencyKey is
// the device's, and is what makes a phone retry a no-op.
type Operation struct {
	Header
	RequestID      string         `json:"request_id"`
	IdempotencyKey string         `json:"idempotency_key"`
	Kind           OperationKind  `json:"kind"`
	Lane           Lane           `json:"lane"`
	Intent         Intent         `json:"intent"`
	State          OperationState `json:"state"`
	Payload        PayloadRef     `json:"payload"`
	Provenance     Provenance     `json:"provenance"`
	Attempt        Attempt        `json:"attempt"`
	CreatedAt      string         `json:"created_at"`
	UpdatedAt      string         `json:"updated_at"`
	// Freshness is the grounding the operation ran against. It travels with
	// the envelope so a result can be read months later next to how stale its
	// inputs were.
	Freshness []SourceFreshness `json:"freshness"`
	Result    *OperationResult  `json:"result,omitempty"`
}

// NewOperation returns an operation with its envelope and empty collections
// filled in.
func NewOperation() Operation {
	return Operation{Header: newHeader(SchemaOperation), Freshness: []SourceFreshness{}}
}

func (o *Operation) SchemaName() string { return SchemaOperation }
func (o *Operation) ByteLimit() int     { return MaxOperationBytes }

// Status projects the job-state slice a receipt carries.
func (o *Operation) Status() OperationStatus {
	return OperationStatus{
		RequestID: o.RequestID,
		Lane:      o.Lane,
		Kind:      o.Kind,
		State:     o.State,
		UpdatedAt: o.UpdatedAt,
		Attempts:  o.Attempt.Count,
	}
}

func (o *Operation) Validate() error {
	if err := o.validate(SchemaOperation); err != nil {
		return err
	}
	if err := validateID("request_id", PrefixRequest, o.RequestID); err != nil {
		return err
	}
	if err := validateIdempotencyKey("idempotency_key", o.IdempotencyKey); err != nil {
		return err
	}
	if err := inVocabulary("kind", string(o.Kind), "kind"); err != nil {
		return err
	}
	if err := inVocabulary("lane", string(o.Lane), "lane"); err != nil {
		return err
	}
	if err := inVocabulary("intent", string(o.Intent), "intent"); err != nil {
		return err
	}
	if err := inVocabulary("operation_state", string(o.State), "state"); err != nil {
		return err
	}
	if err := ValidateRouting(o.Lane, o.Intent); err != nil {
		return err
	}
	if err := validateKindForLane(o.Lane, o.Kind); err != nil {
		return err
	}
	if err := o.Payload.validate("payload"); err != nil {
		return err
	}
	if err := o.Provenance.validate("provenance"); err != nil {
		return err
	}
	if err := o.Attempt.validate("attempt"); err != nil {
		return err
	}
	if err := validateTimestamp("created_at", o.CreatedAt); err != nil {
		return err
	}
	if err := validateTimestamp("updated_at", o.UpdatedAt); err != nil {
		return err
	}
	if o.UpdatedAt < o.CreatedAt {
		// RFC3339 UTC with fixed precision sorts lexicographically, which is
		// the second reason the timestamp format is pinned so narrowly.
		return errf(CodeInvalidState, "updated_at", "an operation cannot be updated before it was created")
	}
	if o.Freshness == nil {
		return errf(CodeMissingField, "freshness", "an empty collection is [], never null")
	}
	// The freshness an operation carries is the grounding it was accepted
	// against, so the reference for every age is when it was created, not
	// when it was last touched.
	if err := validateFreshness("freshness", o.Freshness, o.CreatedAt); err != nil {
		return err
	}
	return o.validateResult()
}

func (o *Operation) validateResult() error {
	if o.Result == nil {
		if o.State.IsTerminal() {
			return errf(CodeMissingField, "result", "a terminal operation carries a result")
		}
		return nil
	}
	if !o.State.IsTerminal() {
		return errf(CodeInvalidState, "result", "only a terminal operation carries a result")
	}
	r := o.Result
	if err := validateOptionalID("result.receipt_id", PrefixReceipt, r.ReceiptID); err != nil {
		return err
	}
	if err := validateOptionalID("result.memory_id", PrefixMemory, r.MemoryID); err != nil {
		return err
	}
	if r.ErrorCode != "" {
		if err := inVocabulary("reject_reason", string(r.ErrorCode), "result.error_code"); err != nil {
			return err
		}
	}
	switch o.State {
	case StateDone:
		if r.ErrorCode != "" {
			return errf(CodeInvalidState, "result.error_code", "a done operation carries no error code")
		}
		if r.ReceiptID == "" {
			return errf(CodeMissingField, "result.receipt_id", "a done operation names its receipt")
		}
	case StateFailed:
		if r.ErrorCode == "" {
			return errf(CodeMissingField, "result.error_code", "a failed operation names its error code")
		}
		if r.MemoryID != "" {
			return errf(CodeInvalidState, "result.memory_id", "a failed operation created no memory")
		}
	}
	return nil
}

// validateKindForLane keeps the executor and the work in agreement: a research
// kind cannot run in the memory lane, and a vault write cannot be handed to the
// async lane, which has no vault.
func validateKindForLane(lane Lane, kind OperationKind) error {
	switch lane {
	case LaneMemory:
		if kind == KindCapture || kind == KindAsk {
			return nil
		}
	case LaneResearch:
		if kind == KindResearch {
			return nil
		}
	}
	return errf(CodeInvalidState, "kind", "kind %q is not executed by lane %q", kind, lane)
}

// KindFor returns the kind the kernel assigns to a device's declared intent.
// It is the only supported way to turn an untrusted intent into a routing
// decision.
func KindFor(intent Intent) (OperationKind, Lane, error) {
	switch intent {
	case IntentRemember:
		return KindCapture, LaneMemory, nil
	case IntentAsk:
		return KindAsk, LaneMemory, nil
	case IntentInvestigate:
		return KindResearch, LaneResearch, nil
	}
	return "", "", errf(CodeInvalidEnum, "intent", "not a published intent")
}

// StateMachine renders the published transitions as text, for the contract doc
// and for a failure message that has to explain itself.
func StateMachine() string {
	order := []OperationState{StateCaptured, StateTriaged, StateLeased, StateRunning, StateNeedsInput, StateDone, StateFailed}
	var b strings.Builder
	for _, s := range order {
		next := NextStates(s)
		if len(next) == 0 {
			fmt.Fprintf(&b, "%s -> (terminal)\n", s)
			continue
		}
		parts := make([]string, len(next))
		for i, n := range next {
			parts[i] = string(n)
		}
		fmt.Fprintf(&b, "%s -> %s\n", s, strings.Join(parts, " | "))
	}
	return b.String()
}
