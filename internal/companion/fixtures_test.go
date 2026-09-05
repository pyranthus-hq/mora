package companion

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// frozenKeys is the published field set of every schema, as "<path>:<json
// type>", unioned over that schema's fixtures.
//
// It is the removal gate. Regenerating the goldens cannot bypass it: the
// generator rewrites testdata, but a dropped, renamed or retyped field still
// fails here, and this list can only change by editing a test file — which the
// coordinator diffs on completion. That is the whole point. A field added to a
// schema is MINOR and appears here as a new line; a field that disappears from
// here without a SchemaVersion bump is a broken contract.
var frozenKeys = map[string][]string{
	"mora.companion.pairing.grant": {
		"device_id:string",
		"issued_at:string",
		"schema:string",
		"schema_version:number",
		"token:string",
		"token_fingerprint:string",
	},
	"mora.companion.capture": {
		"captured_at:string",
		"device_id:string",
		"idempotency_key:string",
		"intent:string",
		"payload_fingerprint:string",
		"requested_lane:string",
		"schema:string",
		"schema_version:number",
		"scope:string",
		"text:string",
	},
	"mora.companion.context": {
		"evidence:array",
		"evidence[].deep_link:string",
		"evidence[].memory_id:string",
		"evidence[].occurred_at:string",
		"evidence[].snippet:string",
		"evidence[].source:string",
		"evidence[]:object",
		"freshness:array",
		"freshness[].age_seconds:number",
		"freshness[].error_code:string",
		"freshness[].key:string",
		"freshness[].last_success_at:string",
		"freshness[].state:string",
		"freshness[]:object",
		"gaps:array",
		"gaps[]:string",
		"generated_at:string",
		"health.policy:string",
		"health.state:string",
		"health:object",
		"mode:string",
		"query:string",
		"schema:string",
		"schema_version:number",
		"synthesis_prompt:string",
	},
	"mora.companion.context.request": {
		"mode:string",
		"query:string",
		"schema:string",
		"schema_version:number",
		"scope:string",
	},
	"mora.companion.device": {
		"created_at:string",
		"device_id:string",
		"label:string",
		"last_seen_at:string",
		"platform:string",
		"revoked_at:string",
		"schema:string",
		"schema_version:number",
		"state:string",
		"token_fingerprint:string",
	},
	"mora.companion.health": {
		"generated_at:string",
		"index.built_at:string",
		"index.memories:number",
		"index.state:string",
		"index:object",
		"policy:string",
		"schema:string",
		"schema_version:number",
		"sources:array",
		"sources[].age_seconds:number",
		"sources[].error_code:string",
		"sources[].key:string",
		"sources[].last_success_at:string",
		"sources[].state:string",
		"sources[]:object",
		"state:string",
	},
	"mora.companion.operation": {
		"attempt.count:number",
		"attempt.execution_id:string",
		"attempt.max:number",
		"attempt:object",
		"created_at:string",
		"freshness:array",
		"freshness[].age_seconds:number",
		"freshness[].error_code:string",
		"freshness[].key:string",
		"freshness[].last_success_at:string",
		"freshness[].state:string",
		"freshness[]:object",
		"idempotency_key:string",
		"intent:string",
		"kind:string",
		"lane:string",
		"payload.bytes:number",
		"payload.fingerprint:string",
		"payload.media_type:string",
		"payload.ref:string",
		"payload:object",
		"provenance.accepted_at:string",
		"provenance.device_id:string",
		"provenance.origin:string",
		"provenance.surface:string",
		"provenance:object",
		"request_id:string",
		"result.error_code:string",
		"result.memory_id:string",
		"result.receipt_id:string",
		"result:object",
		"schema:string",
		"schema_version:number",
		"state:string",
		"updated_at:string",
	},
	"mora.companion.pairing": {
		"device_id:string",
		"endpoint:string",
		"expires_at:string",
		"host_fingerprint:string",
		"pairing_code:string",
		"schema:string",
		"schema_version:number",
	},
	"mora.companion.pairing.confirmation": {
		"confirmed_at:string",
		"device_id:string",
		"label:string",
		"pairing_code:string",
		"platform:string",
		"public_key:string",
		"schema:string",
		"schema_version:number",
	},
	"mora.companion.receipt": {
		"device_id:string",
		"idempotency_key:string",
		"memory_id:string",
		"operation.attempts:number",
		"operation.kind:string",
		"operation.lane:string",
		"operation.request_id:string",
		"operation.state:string",
		"operation.updated_at:string",
		"operation:object",
		"payload_fingerprint:string",
		"policy:string",
		"reason:string",
		"receipt_id:string",
		"received_at:string",
		"request_id:string",
		"schema:string",
		"schema_version:number",
		"settled_at:string",
		"state:string",
	},
	"mora.companion.today": {
		"freshness:array",
		"freshness[].age_seconds:number",
		"freshness[].error_code:string",
		"freshness[].key:string",
		"freshness[].last_success_at:string",
		"freshness[].state:string",
		"freshness[]:object",
		"generated_at:string",
		"health.policy:string",
		"health.state:string",
		"health:object",
		"items:array",
		"items[].body:string",
		"items[].evidence:array",
		"items[].evidence[].deep_link:string",
		"items[].evidence[].memory_id:string",
		"items[].evidence[].occurred_at:string",
		"items[].evidence[].snippet:string",
		"items[].evidence[].source:string",
		"items[].evidence[]:object",
		"items[].id:string",
		"items[].kind:string",
		"items[].title:string",
		"items[]:object",
		"schema:string",
		"schema_version:number",
		"truncated:bool",
	},
}

// goldenBytes reads a committed fixture.
func goldenBytes(t *testing.T, key string) []byte {
	t.Helper()
	b, err := os.ReadFile(GoldenPath(key))
	if err != nil {
		t.Fatalf("read golden for %s: %v", key, err)
	}
	return b
}

// keyPaths walks a decoded document and returns every field path with its JSON
// type. An array contributes "<path>[]" plus the union of its elements' paths.
func keyPaths(prefix string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		if prefix != "" {
			out[prefix] = "object"
		}
		for k, vv := range t {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			keyPaths(p, vv, out)
		}
	case []any:
		out[prefix] = "array"
		for _, e := range t {
			keyPaths(prefix+"[]", e, out)
		}
	case string:
		out[prefix] = "string"
	case float64:
		out[prefix] = "number"
	case bool:
		out[prefix] = "bool"
	case nil:
		out[prefix] = "null"
	}
}

func sortedPaths(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, ty := range m {
		out = append(out, k+":"+ty)
	}
	sort.Strings(out)
	return out
}

// TestGoldenCorpusIsFrozen proves the committed bytes are what the fixture
// builders emit today. It is the direction that catches an accidental change to
// a shipped payload.
func TestGoldenCorpusIsFrozen(t *testing.T) {
	for _, key := range FixtureNames() {
		t.Run(key, func(t *testing.T) {
			fixture, ok := FixtureFor(key)
			if !ok {
				t.Fatalf("no fixture for %s", key)
			}
			got, err := Marshal(fixture)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			want := goldenBytes(t, key)
			if string(got) != string(want) {
				t.Errorf("golden %s is stale.\ngot:\n%s\nwant:\n%s", GoldenPath(key), got, want)
			}
		})
	}
}

// TestGoldenCorpusIsComplete fails when a published schema has no frozen
// document, so a new payload cannot escape the gate by simply not having a
// fixture.
func TestGoldenCorpusIsComplete(t *testing.T) {
	published := []string{
		SchemaDevice, SchemaPairing, SchemaPairingOK, SchemaPairingGrant,
		SchemaToday, SchemaContext, SchemaContextRq, SchemaCapture,
		SchemaReceipt, SchemaHealth, SchemaOperation,
	}
	have := map[string]bool{}
	for _, name := range SchemaNames() {
		have[name] = true
	}
	for _, name := range published {
		if !have[name] {
			t.Errorf("schema %s has no fixture", name)
		}
	}
	if len(SchemaNames()) != len(published) {
		t.Errorf("fixture registry covers %d schemas, %d are published", len(SchemaNames()), len(published))
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(FixtureNames()) {
		t.Errorf("testdata/v1 holds %d files, the registry has %d fixtures", len(entries), len(FixtureNames()))
	}
}

// TestFrozenKeysCoverEverySchema keeps the removal gate honest: a schema with
// no frozen key list would be silently unguarded.
func TestFrozenKeysCoverEverySchema(t *testing.T) {
	for _, name := range SchemaNames() {
		if len(frozenKeys[name]) == 0 {
			t.Errorf("schema %s has no frozen key list", name)
		}
	}
	for name := range frozenKeys {
		if _, ok := FixtureFor(name); !ok {
			t.Errorf("frozen key list %s has no fixture", name)
		}
	}
}

// TestGoldenFieldSetIsFrozen is the removal and retype gate described on
// frozenKeys.
func TestGoldenFieldSetIsFrozen(t *testing.T) {
	union := map[string]map[string]string{}
	for _, key := range FixtureNames() {
		var doc any
		if err := json.Unmarshal(goldenBytes(t, key), &doc); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		name, _, _ := strings.Cut(key, "#")
		if union[name] == nil {
			union[name] = map[string]string{}
		}
		keyPaths("", doc, union[name])
	}
	for _, name := range SchemaNames() {
		got := sortedPaths(union[name])
		want := append([]string(nil), frozenKeys[name]...)
		sort.Strings(want)
		if len(got) != len(want) {
			t.Errorf("%s: field set changed\ngot:  %v\nwant: %v", name, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: field %q is not the frozen %q. Removing, renaming or retyping a field requires a SchemaVersion bump and testdata/v%d/.", name, got[i], want[i], SchemaVersion+1)
			}
		}
	}
}

// TestGoldensDecodeStrictAndValidate proves every committed document survives
// the exact path the kernel uses on inbound bytes.
func TestGoldensDecodeStrictAndValidate(t *testing.T) {
	for _, key := range FixtureNames() {
		t.Run(key, func(t *testing.T) {
			fixture, _ := FixtureFor(key)
			fresh := newEmptyLike(t, fixture)
			if err := Unmarshal(goldenBytes(t, key), fresh); err != nil {
				t.Fatalf("strict decode: %v", err)
			}
			round, err := Marshal(fresh)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			if string(round) != string(goldenBytes(t, key)) {
				t.Errorf("round trip is not byte-identical:\n%s", round)
			}
		})
	}
}

// newEmptyLike returns a zero value of the same concrete type as p, so a decode
// test never reuses the fixture's own populated struct.
func newEmptyLike(t *testing.T, p Payload) Payload {
	t.Helper()
	switch p.(type) {
	case *Device:
		return &Device{}
	case *PairingPayload:
		return &PairingPayload{}
	case *PairingConfirmation:
		return &PairingConfirmation{}
	case *PairingGrant:
		return &PairingGrant{}
	case *HealthProjection:
		return &HealthProjection{}
	case *TodayProjection:
		return &TodayProjection{}
	case *ContextRequest:
		return &ContextRequest{}
	case *ContextBundle:
		return &ContextBundle{}
	case *Capture:
		return &Capture{}
	case *Receipt:
		return &Receipt{}
	case *Operation:
		return &Operation{}
	}
	t.Fatalf("no empty value for %T", p)
	return nil
}

// TestUnknownFieldIsRejected is the inbound half of the compatibility contract:
// the kernel refuses a field it does not know rather than ignoring it.
func TestUnknownFieldIsRejected(t *testing.T) {
	for _, key := range FixtureNames() {
		t.Run(key, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(goldenBytes(t, key), &doc); err != nil {
				t.Fatal(err)
			}
			doc["a_field_from_a_later_version"] = "surprise"
			injected, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			fixture, _ := FixtureFor(key)
			err = Unmarshal(injected, newEmptyLike(t, fixture))
			var e *Error
			if !asError(err, &e) || e.Code != CodeUnknownField {
				t.Fatalf("want %s, got %v", CodeUnknownField, err)
			}
		})
	}
}

// pinnedTodayItem is a consumer that pinned v1 and knows only three fields.
type pinnedTodayItem struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

type pinnedToday struct {
	Schema        string            `json:"schema"`
	SchemaVersion int               `json:"schema_version"`
	GeneratedAt   string            `json:"generated_at"`
	Items         []pinnedTodayItem `json:"items"`
}

// TestAddingAFieldIsSafeForAPinnedConsumer is the outbound half. A device
// decodes tolerantly, so a field that arrives from a later kernel must leave
// its view byte-identical. This is measured, not assumed.
func TestAddingAFieldIsSafeForAPinnedConsumer(t *testing.T) {
	golden := goldenBytes(t, SchemaToday)

	var before pinnedToday
	if err := json.Unmarshal(golden, &before); err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(golden, &doc); err != nil {
		t.Fatal(err)
	}
	doc["weather"] = "a field v1 never had"
	items := doc["items"].([]any)
	items[0].(map[string]any)["confidence"] = 0.9
	injected, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	var after pinnedToday
	if err := json.Unmarshal(injected, &after); err != nil {
		t.Fatalf("a pinned consumer failed on an additive change: %v", err)
	}
	b1, _ := json.Marshal(before)
	b2, _ := json.Marshal(after)
	if string(b1) != string(b2) {
		t.Errorf("the pinned view changed:\n%s\n%s", b1, b2)
	}
}

// TestGoldensAreWithinTheirByteLimit proves the published bounds are not
// aspirational.
func TestGoldensAreWithinTheirByteLimit(t *testing.T) {
	for _, key := range FixtureNames() {
		fixture, _ := FixtureFor(key)
		if got, limit := len(goldenBytes(t, key)), fixture.ByteLimit(); got > limit {
			t.Errorf("%s: golden is %d bytes, limit is %d", key, got, limit)
		}
	}
}

// TestFingerprintIsTheDocumentedDerivation pins the derivation both a device
// and the kernel must implement, so a Swift client can reproduce it.
func TestFingerprintIsTheDocumentedDerivation(t *testing.T) {
	// sha256("") is a fixed, widely published value; if this line ever
	// changes, the derivation changed.
	const emptySHA = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := Fingerprint(""); got != emptySHA {
		t.Errorf("Fingerprint(\"\") = %s, want %s", got, emptySHA)
	}
	if err := validateFingerprint("f", Fingerprint("anything")); err != nil {
		t.Errorf("a produced fingerprint must pass validation: %v", err)
	}
	if CaptureFixture().PayloadFingerprint != Fingerprint(fixtureText) {
		t.Error("the capture fixture's fingerprint does not cover its own text")
	}
}

// TestGoldenRegenerationIsExplicit documents the regeneration path in one
// place, and proves it is a test-only affordance rather than product code.
func TestGoldenRegenerationIsExplicit(t *testing.T) {
	if os.Getenv("MORA_UPDATE_COMPANION_GOLDEN") != "1" {
		t.Skip("set MORA_UPDATE_COMPANION_GOLDEN=1 to rewrite testdata/v1; the frozen key list in this file still gates removals")
	}
	for _, key := range FixtureNames() {
		fixture, _ := FixtureFor(key)
		b, err := Marshal(fixture)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if err := os.WriteFile(GoldenPath(key), b, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", GoldenPath(key), len(b))
	}
}

// asError is errors.As without the import, kept local so the package's test
// helpers stay as small as the package.
func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}
