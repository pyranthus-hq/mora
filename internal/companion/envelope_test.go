package companion

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestStateMachineIsFrozen pins the published transitions. A shell renders
// these states and the spine drives them, so a change here is a protocol
// change, not a refactor.
func TestStateMachineIsFrozen(t *testing.T) {
	want := map[OperationState][]OperationState{
		StateCaptured:   {StateTriaged, StateFailed},
		StateTriaged:    {StateLeased, StateFailed},
		StateLeased:     {StateRunning, StateTriaged, StateFailed},
		StateRunning:    {StateDone, StateNeedsInput, StateFailed, StateTriaged},
		StateNeedsInput: {StateRunning, StateFailed},
		StateDone:       nil,
		StateFailed:     nil,
	}
	for _, s := range []OperationState{StateCaptured, StateTriaged, StateLeased, StateRunning, StateNeedsInput, StateDone, StateFailed} {
		got := NextStates(s)
		if len(got) != len(want[s]) {
			t.Errorf("%s -> %v, frozen as %v", s, got, want[s])
			continue
		}
		for i := range got {
			if got[i] != want[s][i] {
				t.Errorf("%s -> %v, frozen as %v", s, got, want[s])
				break
			}
		}
	}
	// Every published state must appear in the machine, or the vocabulary
	// would carry a state nothing can reach.
	for _, v := range Vocabulary["operation_state"] {
		s := OperationState(v)
		if len(NextStates(s)) == 0 && !s.IsTerminal() {
			t.Errorf("%s is neither terminal nor able to move", s)
		}
	}
}

func TestNextStatesReturnsACopy(t *testing.T) {
	got := NextStates(StateRunning)
	if len(got) == 0 {
		t.Fatal("running has successors")
	}
	got[0] = StateCaptured
	if NextStates(StateRunning)[0] == StateCaptured {
		t.Fatal("NextStates handed out the package's own slice")
	}
}

func TestValidateTransition(t *testing.T) {
	cases := []struct {
		from, to OperationState
		ok       bool
	}{
		{StateCaptured, StateTriaged, true},
		{StateTriaged, StateLeased, true},
		{StateLeased, StateRunning, true},
		{StateRunning, StateDone, true},
		{StateRunning, StateNeedsInput, true},
		{StateNeedsInput, StateRunning, true},
		// A lapsed lease returns the job to the pool rather than stranding it.
		{StateLeased, StateTriaged, true},
		{StateRunning, StateTriaged, true},
		// A repeated update is idempotent.
		{StateRunning, StateRunning, true},
		{StateDone, StateDone, true},
		// Terminal means terminal.
		{StateDone, StateRunning, false},
		{StateFailed, StateTriaged, false},
		// No skipping the queue.
		{StateCaptured, StateRunning, false},
		{StateTriaged, StateDone, false},
		{StateCaptured, StateDone, false},
	}
	for _, tc := range cases {
		if err := ValidateTransition(tc.from, tc.to); tc.ok != (err == nil) {
			t.Errorf("%s -> %s = %v, want ok=%v", tc.from, tc.to, err, tc.ok)
		}
	}
	if err := ValidateTransition("parked", StateDone); err == nil {
		t.Error("an unpublished state must be rejected")
	}
}

func TestTerminalAndAwaitingUserAreDistinct(t *testing.T) {
	if !StateDone.IsTerminal() || !StateFailed.IsTerminal() {
		t.Error("done and failed are terminal")
	}
	if StateNeedsInput.IsTerminal() {
		t.Error("needs_input resumes; it is not terminal")
	}
	if !StateNeedsInput.AwaitsUser() {
		t.Error("needs_input awaits the user")
	}
	if StateRunning.AwaitsUser() {
		t.Error("running does not await the user; a spinner over it is honest")
	}
}

// forbiddenEnvelopeFields are the field names that would break one of the two
// properties the operation envelope exists to hold: it carries no user content,
// and it names no executor.
var forbiddenEnvelopeFields = []string{
	// content
	"text", "body", "prompt", "answer", "snippet", "query", "title", "label",
	"content", "message", "result_body", "citations",
	// executor
	"worker", "model", "host", "hostname", "provider", "agent", "runtime",
	"lease_owner", "node", "machine", "endpoint",
}

func jsonFieldNames(t *testing.T, rt reflect.Type, prefix string, out *[]string) {
	t.Helper()
	for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice {
		rt = rt.Elem()
	}
	if rt.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			// An embedded struct with no tag marshals flat.
			jsonFieldNames(t, f.Type, prefix, out)
			continue
		}
		*out = append(*out, name)
		jsonFieldNames(t, f.Type, prefix+name+".", out)
	}
}

// TestOperationNamesNoExecutorAndNoContent is the structural half of the two
// envelope properties: it inspects the type, so a field added later fails here
// even if no fixture ever populates it.
func TestOperationNamesNoExecutorAndNoContent(t *testing.T) {
	var names []string
	jsonFieldNames(t, reflect.TypeOf(Operation{}), "", &names)
	if len(names) == 0 {
		t.Fatal("walked no fields")
	}
	for _, name := range names {
		for _, bad := range forbiddenEnvelopeFields {
			if name == bad {
				t.Errorf("the operation envelope carries a %q field; it must stay content-free and executor-free", name)
			}
		}
	}
}

// TestOperationCarriesNoUserText is the behavioural half: the marshaled
// envelope must not contain the text it refers to. This is what lets a queue, a
// lease table and a sweeper log hold envelopes without holding what the user
// wrote.
func TestOperationCarriesNoUserText(t *testing.T) {
	op := OperationFixture()
	if op.Payload.Fingerprint != Fingerprint(fixtureText) {
		t.Fatal("the fixture envelope must refer to the fixture capture")
	}
	body, err := Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	haystack := strings.ToLower(string(body))
	for _, word := range strings.Fields(fixtureText) {
		word = strings.Trim(strings.ToLower(word), ".,")
		if len(word) < 5 {
			continue
		}
		if strings.Contains(haystack, word) {
			t.Errorf("the envelope leaked %q from the capture text:\n%s", word, body)
		}
	}
	if int(op.Payload.Bytes) != len(fixtureText) {
		t.Errorf("payload.bytes = %d, want %d", op.Payload.Bytes, len(fixtureText))
	}
}

// TestOperationEnvelopeIsSmall backs the content-free claim with a bound: a
// 4 KiB ceiling leaves no room for a smuggled paragraph.
func TestOperationEnvelopeIsSmall(t *testing.T) {
	op := OperationFixture()
	body, err := Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > MaxOperationBytes {
		t.Errorf("the envelope is %d bytes, the bound is %d", len(body), MaxOperationBytes)
	}
	op.Payload.Ref = "capture:" + strings.Repeat("k", MaxIdempotencyKeyBytes)
	if err := op.Validate(); err == nil {
		t.Fatal("an oversize payload reference must be rejected")
	}
}

func TestPayloadReferenceRejectsProse(t *testing.T) {
	op := OperationFixture()
	op.Payload.Ref = "Ravi wants the pilot scoped"
	err := op.Validate()
	var e *Error
	if !asError(err, &e) || e.Code != CodeInvalidValue {
		t.Fatalf("want %s, got %v", CodeInvalidValue, err)
	}
}

func TestPayloadMediaTypeIsPinned(t *testing.T) {
	op := OperationFixture()
	op.Payload.MediaType = "text/markdown"
	if err := op.Validate(); err == nil {
		t.Fatal("v1 carries only plain UTF-8 text")
	}
}

func TestPayloadBytesCannotExceedTheCaptureBound(t *testing.T) {
	op := OperationFixture()
	op.Payload.Bytes = MaxCaptureTextBytes + 1
	err := op.Validate()
	var e *Error
	if !asError(err, &e) || e.Code != CodeTooLarge {
		t.Fatalf("want %s, got %v", CodeTooLarge, err)
	}
}

// TestProvenanceRequiresADeviceForCompanionOrigin keeps a companion operation
// attributable to a revocable device.
func TestProvenanceRequiresADeviceForCompanionOrigin(t *testing.T) {
	op := OperationFixture()
	op.Provenance.DeviceID = ""
	err := op.Validate()
	var e *Error
	if !asError(err, &e) || e.Code != CodeMissingField {
		t.Fatalf("want %s, got %v", CodeMissingField, err)
	}
	op.Provenance.Origin = "somewhere_else"
	if err := op.Validate(); err == nil {
		t.Fatal("an unpublished origin must be rejected")
	}
}

func TestAttemptCapIsRequiredAndEnforced(t *testing.T) {
	op := OperationFixture()
	op.Attempt = Attempt{Count: 1, Max: 0}
	if err := op.Validate(); err == nil {
		t.Fatal("an operation without an attempt cap can retry forever")
	}
	op.Attempt = Attempt{Count: 4, Max: 3}
	if err := op.Validate(); err == nil {
		t.Fatal("attempts beyond the cap must be rejected")
	}
}

func TestTerminalOperationMustCarryAResult(t *testing.T) {
	op := OperationFixture()
	op.State = StateDone
	err := op.Validate()
	var e *Error
	if !asError(err, &e) || e.Code != CodeMissingField || e.Field != "result" {
		t.Fatalf("want %s at result, got %v", CodeMissingField, err)
	}

	done := DoneOperationFixture()
	if err := done.Validate(); err != nil {
		t.Fatalf("the done fixture must be valid: %v", err)
	}
	done.Result.ErrorCode = ReasonInternal
	if err := done.Validate(); err == nil {
		t.Fatal("a done operation carries no error code")
	}

	failed := FailedOperationFixture()
	if err := failed.Validate(); err != nil {
		t.Fatalf("the failed fixture must be valid: %v", err)
	}
	failed.Result.MemoryID = fixtureMemoryID
	if err := failed.Validate(); err == nil {
		t.Fatal("a failed operation created no memory")
	}
	failed = FailedOperationFixture()
	failed.Result.ErrorCode = ""
	if err := failed.Validate(); err == nil {
		t.Fatal("a failed operation must name its error code")
	}
}

func TestInFlightOperationCarriesNoResult(t *testing.T) {
	op := OperationFixture()
	op.Result = &OperationResult{ReceiptID: fixtureReceiptID}
	if err := op.Validate(); err == nil {
		t.Fatal("only a terminal operation carries a result")
	}
}

func TestKindMustMatchLane(t *testing.T) {
	op := OperationFixture()
	op.Lane = LaneMemory
	op.Intent = IntentAsk
	op.Kind = KindResearch
	err := op.Validate()
	var e *Error
	if !asError(err, &e) || e.Field != "kind" {
		t.Fatalf("want a kind error, got %v", err)
	}
}

// TestKindForIsTheOnlyRoutingPath proves the kernel derives kind and lane from
// the device's intent rather than trusting a lane the device asked for.
func TestKindForIsTheOnlyRoutingPath(t *testing.T) {
	cases := map[Intent]struct {
		kind OperationKind
		lane Lane
	}{
		IntentRemember:    {KindCapture, LaneMemory},
		IntentAsk:         {KindAsk, LaneMemory},
		IntentInvestigate: {KindResearch, LaneResearch},
	}
	for intent, want := range cases {
		kind, lane, err := KindFor(intent)
		if err != nil {
			t.Fatalf("KindFor(%s): %v", intent, err)
		}
		if kind != want.kind || lane != want.lane {
			t.Errorf("KindFor(%s) = %s/%s, want %s/%s", intent, kind, lane, want.kind, want.lane)
		}
		if err := validateKindForLane(lane, kind); err != nil {
			t.Errorf("KindFor produced a pair its own validator rejects: %v", err)
		}
	}
	if _, _, err := KindFor("summarise"); err == nil {
		t.Error("an unpublished intent must not route")
	}
}

func TestUpdatedAtCannotPrecedeCreatedAt(t *testing.T) {
	op := OperationFixture()
	op.UpdatedAt = "2026-09-03T10:00:00Z"
	err := op.Validate()
	var e *Error
	if !asError(err, &e) || e.Code != CodeInvalidState {
		t.Fatalf("want %s, got %v", CodeInvalidState, err)
	}
}

// TestStatusProjectsTheEnvelope proves the receipt's job-state block is a
// projection of the operation and not a second source of truth that could drift.
func TestStatusProjectsTheEnvelope(t *testing.T) {
	op := OperationFixture()
	got := op.Status()
	want := OperationStatus{
		RequestID: op.RequestID,
		Lane:      op.Lane,
		Kind:      op.Kind,
		State:     op.State,
		UpdatedAt: op.UpdatedAt,
		Attempts:  op.Attempt.Count,
	}
	if got != want {
		t.Errorf("Status() = %+v, want %+v", got, want)
	}
}

// TestReceiptJobStateRendersTheSpineVocabulary covers the Activity screen's
// contract: every state it must render is reachable through a receipt.
func TestReceiptJobStateRendersTheSpineVocabulary(t *testing.T) {
	r := ReceiptFixture()
	for _, v := range Vocabulary["operation_state"] {
		r.Operation.State = OperationState(v)
		if err := r.Validate(); err != nil {
			t.Errorf("a receipt cannot carry job state %s: %v", v, err)
		}
	}
}

func TestStateMachineRendersEveryState(t *testing.T) {
	text := StateMachine()
	for _, v := range Vocabulary["operation_state"] {
		if !strings.Contains(text, v) {
			t.Errorf("StateMachine() omits %s:\n%s", v, text)
		}
	}
	if !strings.Contains(text, "(terminal)") {
		t.Error("StateMachine() must mark the terminal states")
	}
}

// TestOperationGoldenHasNoResultUntilTerminal guards the fixture set itself:
// the in-flight golden is what a client decodes most often.
func TestOperationGoldenHasNoResultUntilTerminal(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(goldenBytes(t, SchemaOperation), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["result"]; ok {
		t.Error("the in-flight operation golden carries a result")
	}
	if err := json.Unmarshal(goldenBytes(t, SchemaOperation+"#done"), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["result"]; !ok {
		t.Error("the done operation golden carries no result")
	}
}
