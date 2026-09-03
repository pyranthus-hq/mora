package companion

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageIsALeaf enforces the territory rule for graph node N02: this
// package is the wire contract and nothing else, so it may import only the
// standard library. An import of internal/mora would make the contract
// impossible to compile into a fixture generator or a Swift-parity harness
// without dragging the kernel along, and it would reintroduce the connector
// import cycle AGENTS.md rule 1 exists to prevent.
func TestPackageIsALeaf(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".go" {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			first, _, _ := strings.Cut(path, "/")
			if strings.Contains(first, ".") {
				t.Errorf("%s imports %s; this package may import only the standard library", e.Name(), path)
			}
		}
	}
}

// TestVocabularyIsFrozen pins every published enum value. A shell renders these
// strings and a golden carries them, so adding one is a deliberate act and
// removing one breaks a client that already shipped.
func TestVocabularyIsFrozen(t *testing.T) {
	want := map[string][]string{
		"lane":            {"memory", "research"},
		"intent":          {"remember", "ask", "investigate"},
		"kind":            {"capture", "ask", "research"},
		"operation_state": {"captured", "triaged", "leased", "running", "done", "needs_input", "failed"},
		"receipt_state":   {"accepted", "applied", "rejected"},
		"reject_reason":   {"policy", "unknown_device", "revoked_device", "too_large", "malformed", "unsupported_lane", "idempotency_conflict", "unavailable", "internal"},
		"write_policy":    {"open", "propose", "readonly"},
		"context_mode":    {"think", "search", "meeting_prep"},
		"health_state":    {"healthy", "degraded", "unhealthy"},
		"freshness_state": {"fresh", "stale", "failed", "never"},
		"device_state":    {"pending", "active", "revoked"},
		"platform":        {"ios", "macos", "other"},
		"today_item_kind": {"needs_attention", "changed", "commitment"},
	}
	if len(Vocabulary) != len(want) {
		t.Errorf("Vocabulary has %d families, %d are frozen here", len(Vocabulary), len(want))
	}
	for family, values := range want {
		got := Vocabulary[family]
		if len(got) != len(values) {
			t.Errorf("%s: %v, frozen as %v", family, got, values)
			continue
		}
		for i := range values {
			if got[i] != values[i] {
				t.Errorf("%s[%d] = %q, frozen as %q", family, i, got[i], values[i])
			}
		}
	}
	// The freshness vocabulary is copied from internal/health rather than
	// imported. If that package's constants move, this comment is the only
	// thing that will remind the next reader to move these too.
	if string(FreshnessFresh) != "fresh" || string(FreshnessNever) != "never" {
		t.Error("freshness vocabulary drifted from internal/health")
	}
}

func TestUnknownEnumValueIsRejected(t *testing.T) {
	c := CaptureFixture()
	c.Intent = "summarise"
	err := c.Validate()
	var e *Error
	if !asError(err, &e) || e.Code != CodeInvalidEnum {
		t.Fatalf("want %s, got %v", CodeInvalidEnum, err)
	}
	if e.Field != "intent" {
		t.Errorf("field = %q, want intent", e.Field)
	}
}

func TestTimestampFormatIsNarrow(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"canonical", "2026-09-03T11:00:00Z", true},
		{"fractional", "2026-09-03T11:00:00.250Z", false},
		{"offset", "2026-09-03T04:00:00-07:00", false},
		{"date only", "2026-09-03", false},
		{"empty", "", false},
		{"lowercase z", "2026-09-03T11:00:00z", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTimestamp("f", tc.in)
			if tc.ok != (err == nil) {
				t.Fatalf("validateTimestamp(%q) = %v, want ok=%v", tc.in, err, tc.ok)
			}
		})
	}
}

func TestOversizePayloadIsRejectedBeforeDecoding(t *testing.T) {
	c := CaptureFixture()
	c.Text = strings.Repeat("a", MaxCaptureBytes+1)
	c.PayloadFingerprint = Fingerprint(c.Text)
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var got Capture
	err = Unmarshal(body, &got)
	var e *Error
	if !asError(err, &e) || e.Code != CodeTooLarge {
		t.Fatalf("want %s, got %v", CodeTooLarge, err)
	}
	// The whole body exceeded MaxCaptureBytes, so nothing was decoded: an
	// oversize capture must never reach the text bound by way of a populated
	// struct.
	if got.Text != "" {
		t.Error("an oversize body was decoded before its size was checked")
	}
}

func TestTextBoundIsEnforcedInsideTheBodyBound(t *testing.T) {
	// A text just over the text bound but well inside the body bound must
	// still be rejected, or the text bound would be decorative.
	c := CaptureFixture()
	c.Text = strings.Repeat("a", MaxCaptureTextBytes+1)
	c.PayloadFingerprint = Fingerprint(c.Text)
	err := c.Validate()
	var e *Error
	if !asError(err, &e) || e.Code != CodeTooLarge || e.Field != "text" {
		t.Fatalf("want %s at text, got %v", CodeTooLarge, err)
	}
}

func TestTrailingDataIsMalformed(t *testing.T) {
	body := append(goldenBytes(t, SchemaCapture), []byte(`{"another":"document"}`)...)
	err := Unmarshal(body, &Capture{})
	var e *Error
	if !asError(err, &e) || e.Code != CodeMalformed {
		t.Fatalf("want %s, got %v", CodeMalformed, err)
	}
}

func TestWrongSchemaNameIsRejected(t *testing.T) {
	c := CaptureFixture()
	c.Schema = SchemaReceipt
	err := c.Validate()
	var e *Error
	if !asError(err, &e) || e.Code != CodeSchemaMismatch {
		t.Fatalf("want %s, got %v", CodeSchemaMismatch, err)
	}
	c = CaptureFixture()
	c.SchemaVersion = 2
	if err := c.Validate(); err == nil {
		t.Fatal("a future schema_version must be rejected by a v1 kernel")
	}
}

func TestScopeIsPersonalOrProject(t *testing.T) {
	cases := map[string]bool{
		"personal":       true,
		"project:pilot":  true,
		"project:":       false,
		"projects:pilot": false,
		"gmail":          false,
		"":               false,
		"project:a b":    false,
	}
	for in, ok := range cases {
		if err := validateScope("scope", in); ok != (err == nil) {
			t.Errorf("validateScope(%q) = %v, want ok=%v", in, err, ok)
		}
	}
}

func TestIdempotencyKeyRejectsProse(t *testing.T) {
	// The key travels into the content-free operation envelope as part of the
	// payload reference, so it may not carry sentences.
	if err := validateIdempotencyKey("k", "Ravi wants the pilot scoped"); err == nil {
		t.Fatal("an idempotency key with spaces must be rejected")
	}
	if err := validateIdempotencyKey("k", strings.Repeat("k", MaxIdempotencyKeyBytes+1)); err == nil {
		t.Fatal("an oversize idempotency key must be rejected")
	}
	if err := validateIdempotencyKey("k", fixtureIdemKey); err != nil {
		t.Fatalf("the fixture key must be legal: %v", err)
	}
}

func TestIdentifierPrefixIsEnforced(t *testing.T) {
	c := CaptureFixture()
	c.DeviceID = "01J8Z2K7Q4RN3T5V"
	if err := c.Validate(); err == nil {
		t.Fatal("a device id without its prefix must be rejected")
	}
	c.DeviceID = PrefixDevice
	if err := c.Validate(); err == nil {
		t.Fatal("a bare prefix carries no identity and must be rejected")
	}
}

func TestFingerprintFormatIsEnforced(t *testing.T) {
	bad := []string{"", "sha256:", "sha1:" + strings.Repeat("a", 40), "sha256:" + strings.Repeat("A", 64), "sha256:" + strings.Repeat("z", 64)}
	for _, in := range bad {
		if err := validateFingerprint("f", in); err == nil {
			t.Errorf("validateFingerprint(%q) accepted a bad fingerprint", in)
		}
	}
}

// TestRoutingMatrixIsEnforced covers the published lane-and-intent matrix.
func TestRoutingMatrixIsEnforced(t *testing.T) {
	cases := []struct {
		lane   Lane
		intent Intent
		ok     bool
	}{
		{LaneMemory, IntentRemember, true},
		{LaneMemory, IntentAsk, true},
		{LaneMemory, IntentInvestigate, false},
		{LaneResearch, IntentInvestigate, true},
		{LaneResearch, IntentRemember, false},
		{LaneResearch, IntentAsk, false},
	}
	for _, tc := range cases {
		if err := ValidateRouting(tc.lane, tc.intent); tc.ok != (err == nil) {
			t.Errorf("ValidateRouting(%s, %s) = %v, want ok=%v", tc.lane, tc.intent, err, tc.ok)
		}
	}
}

// TestAppliedReceiptMustNameItsMemory is the honesty rule the phone depends on:
// "Saved" is rendered only for an applied receipt, so an applied receipt that
// names no memory would be a lie the client cannot detect.
func TestAppliedReceiptMustNameItsMemory(t *testing.T) {
	r := ReceiptFixture()
	r.MemoryID = ""
	err := r.Validate()
	var e *Error
	if !asError(err, &e) || e.Code != CodeInvalidState || e.Field != "memory_id" {
		t.Fatalf("want %s at memory_id, got %v", CodeInvalidState, err)
	}

	r = ReceiptFixture()
	r.SettledAt = ""
	if err := r.Validate(); err == nil {
		t.Fatal("an applied receipt must be settled")
	}
}

func TestAcceptedReceiptCannotClaimAMemory(t *testing.T) {
	r := AcceptedReceiptFixture()
	if err := r.Validate(); err != nil {
		t.Fatalf("the accepted fixture must be valid: %v", err)
	}
	r.MemoryID = fixtureMemoryID
	if err := r.Validate(); err == nil {
		t.Fatal("propose accepted a capture; it has not published a memory")
	}
}

func TestRejectedReceiptMustNameItsReason(t *testing.T) {
	r := RejectedReceiptFixture()
	if err := r.Validate(); err != nil {
		t.Fatalf("the rejected fixture must be valid: %v", err)
	}
	r.Reason = ""
	err := r.Validate()
	var e *Error
	if !asError(err, &e) || e.Code != CodeMissingField || e.Field != "reason" {
		t.Fatalf("want %s at reason, got %v", CodeMissingField, err)
	}
}

func TestReceiptOperationMustBelongToTheRequest(t *testing.T) {
	r := ReceiptFixture()
	r.Operation.RequestID = "req_someoneelse"
	if err := r.Validate(); err == nil {
		t.Fatal("a receipt must not carry another request's job state")
	}
}

// TestDeviceCarriesNoCredential proves a device record can be listed, logged or
// screenshotted without carrying a bearer token.
func TestDeviceCarriesNoCredential(t *testing.T) {
	body := goldenBytes(t, SchemaDevice)
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "bearer", "secret", "private_key", "key"} {
		if _, ok := doc[forbidden]; ok {
			t.Errorf("the device schema carries a %q field", forbidden)
		}
	}
	if fp, ok := doc["token_fingerprint"].(string); !ok || !strings.HasPrefix(fp, "sha256:") {
		t.Error("a device identifies its credential by fingerprint")
	}
}

func TestRevokedDeviceRecordsWhen(t *testing.T) {
	d := RevokedDeviceFixture()
	if err := d.Validate(); err != nil {
		t.Fatalf("the revoked fixture must be valid: %v", err)
	}
	d.RevokedAt = ""
	if err := d.Validate(); err == nil {
		t.Fatal("a revoked device must record its revocation time")
	}
	live := DeviceFixture()
	live.RevokedAt = fixtureUpdatedAt
	if err := live.Validate(); err == nil {
		t.Fatal("an active device must not carry a revocation time")
	}
}

// TestPairingRedactionMasksTheOneTimeCode covers the only path that is allowed
// to print a pairing payload.
func TestPairingRedactionMasksTheOneTimeCode(t *testing.T) {
	p := PairingFixture()
	red := p.Redacted()
	if red.PairingCode != "[redacted]" {
		t.Errorf("pairing code = %q, want [redacted]", red.PairingCode)
	}
	if p.PairingCode == "[redacted]" {
		t.Error("Redacted must not mutate its receiver")
	}
	c := PairingConfirmationFixture()
	if c.Redacted().PairingCode != "[redacted]" {
		t.Error("the confirmation must redact too")
	}
}

func TestPairingEndpointMustBeHTTPSOrLoopback(t *testing.T) {
	p := PairingFixture()
	p.Endpoint = "http://mora-mac.example.com/v1/companion"
	if err := p.Validate(); err == nil {
		t.Fatal("a cleartext non-loopback endpoint must be rejected")
	}
	p.Endpoint = "http://127.0.0.1:8765/v1/companion"
	if err := p.Validate(); err != nil {
		t.Fatalf("loopback is allowed for a local test: %v", err)
	}
}

// TestEveryProjectionCarriesGeneratedAt covers the rule that no projection may
// be shown without saying when it was made.
func TestEveryProjectionCarriesGeneratedAt(t *testing.T) {
	for _, name := range []string{SchemaToday, SchemaContext, SchemaHealth} {
		var doc map[string]any
		if err := json.Unmarshal(goldenBytes(t, name), &doc); err != nil {
			t.Fatal(err)
		}
		got, ok := doc["generated_at"].(string)
		if !ok {
			t.Errorf("%s has no generated_at", name)
			continue
		}
		if err := validateTimestamp("generated_at", got); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestEmptyCollectionsAreNeverNull covers the machine-contract promise that an
// array-valued field is [] rather than null, in both directions.
func TestEmptyCollectionsAreNeverNull(t *testing.T) {
	tp := NewTodayProjection()
	tp.GeneratedAt = fixtureCreatedAt
	tp.Health = HealthSummary{State: HealthHealthy, Policy: PolicyOpen}
	if err := tp.Validate(); err != nil {
		t.Fatalf("an empty Today is legal: %v", err)
	}
	b, err := Marshal(&tp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"items": []`) || !strings.Contains(string(b), `"freshness": []`) {
		t.Errorf("empty collections must marshal as []:\n%s", b)
	}

	tp.Items = nil
	if err := tp.Validate(); err == nil {
		t.Fatal("a nil collection must not validate")
	}
}

func TestTodayIsBoundedToThreeItems(t *testing.T) {
	tp := TodayFixture()
	item := tp.Items[0]
	tp.Items = []TodayItem{item, item, item, item}
	err := tp.Validate()
	var e *Error
	if !asError(err, &e) || e.Code != CodeTooManyItems {
		t.Fatalf("want %s, got %v", CodeTooManyItems, err)
	}
}

func TestTodayItemMustCarryEvidence(t *testing.T) {
	tp := TodayFixture()
	tp.Items[0].Evidence = []Evidence{}
	if err := tp.Validate(); err == nil {
		t.Fatal("a Today item without evidence is an uncheckable claim")
	}
}

func TestContextBundleIsNotAnAnswer(t *testing.T) {
	// The bundle's shape is the guarantee: it has evidence, gaps and a
	// synthesis prompt, and no field in which a model answer could hide.
	var doc map[string]any
	if err := json.Unmarshal(goldenBytes(t, SchemaContext), &doc); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"answer", "completion", "response", "summary", "body"} {
		if _, ok := doc[forbidden]; ok {
			t.Errorf("the context bundle carries a %q field; the shell owns the answer", forbidden)
		}
	}
	for _, required := range []string{"evidence", "gaps", "synthesis_prompt", "freshness"} {
		if _, ok := doc[required]; !ok {
			t.Errorf("the context bundle is missing %q", required)
		}
	}
	c := ContextFixture()
	c.Gaps = nil
	if err := c.Validate(); err == nil {
		t.Fatal("gaps is a claim about what the vault does not know; it is never omitted")
	}
}

func TestFreshnessNeverHasNoLastSuccess(t *testing.T) {
	h := HealthFixture()
	h.Sources[2].LastSuccessAt = fixtureCreatedAt
	if err := h.Validate(); err == nil {
		t.Fatal("a source that never succeeded cannot report a last success")
	}
}
