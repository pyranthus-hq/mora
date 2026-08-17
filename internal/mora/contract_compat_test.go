package mora

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// CON-05 is enforced here rather than asserted.
//
// A `schema_version` integer proves nothing on its own. What proves backward
// compatibility is a frozen document plus two decodes in opposite directions:
//
//   - Decode TODAY's output into the v1 field set. Every v1 field must still
//     populate — that is the additive-is-safe half (contract_compat A).
//   - Decode the frozen v1 GOLDEN strictly against today's shape. A removed or
//     renamed field shows up as an unknown field and fails — that is the
//     removal-is-caught half (contract_compat B), and it is the load-bearing one.
//
// This file holds the corpus (Task 1); the two decodes live alongside it.

// contractGoldenDir is the frozen v1 corpus. A golden here is NEVER edited in
// place to accommodate a removed field: that requires bumping the schema's
// `schema_version` and adding testdata/contracts/v2/.
const contractGoldenDir = "testdata/contracts/v1"

// contractGoldenUpdateEnv regenerates the corpus. It is deliberately
// ADDITIVE-ONLY: regeneration refuses to write a document that has lost a key
// the committed golden carries. Otherwise the gate would be bypassable by
// following its own failure message — remove a field, regenerate, go green.
const contractGoldenUpdateEnv = "MORA_UPDATE_CONTRACT_GOLDENS"

// contractVolatileTimestamp matches the RFC3339 instants Mora stamps into
// receipts, in both the `Z` and the numeric-offset forms observed in live
// output (`2026-08-17T10:55:23Z`, `2026-08-17T03:55:23-07:00`).
var contractVolatileTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)

// contractVolatileID matches Mora's generated identifiers — a lowercase kind
// prefix, a date, a time, and a random hex suffix (`mem_20260817_035523_a59425fe`,
// `run_20260817_105523_31bb45a4`, `gov_...`). They also appear embedded in
// filesystem paths, which is why normalization runs over the whole string.
var contractVolatileID = regexp.MustCompile(`[a-z]+_\d{8}_\d{6}_[0-9a-f]{8}`)

// contractVolatileStamp matches the date-time stamp in generated file names
// (`mora-20260817-035523.tar.gz`).
var contractVolatileStamp = regexp.MustCompile(`\d{8}-\d{6}`)

// contractVolatileDate matches a bare calendar date. Applied AFTER the RFC3339
// rule so a full timestamp is never split into a date plus a remainder.
var contractVolatileDate = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

// contractVolatileLeaves names the leaves whose VALUE is inherently variable
// and cannot be pinned by a clock or a fixture, keyed by their dotted path
// from the schema name. Each is replaced by a placeholder of the same JSON
// type, so the leaf's presence and type stay frozen while its value does not.
//
// The list is deliberately tiny and hand-reviewed rather than pattern-matched:
// a broad "normalize every number" rule would silently stop freezing values
// that matter. Every entry was found by the two-run determinism check below,
// not guessed.
var contractVolatileLeaves = map[string]any{
	// The gzip size of a tar containing timestamped files. Varies by a few
	// bytes between two runs seconds apart.
	"mora.backup.bytes":                float64(0),
	"mora.doctor.report.storage_bytes": float64(0),
}

// contractNormalize replaces the classes of value that legitimately differ
// between two runs of the same command, so a golden freezes the SHAPE and the
// stable values without flapping on ids, clocks, and temp paths:
//
//  1. the test home prefix, which is a fresh temp directory every run;
//  2. RFC3339 timestamps;
//  3. generated ids and date-time file stamps;
//  4. the named leaves in contractVolatileLeaves.
//
// Every other leaf is frozen verbatim. The generator proves this list is
// sufficient by running the whole command sequence twice and failing on any
// normalized divergence, so a missed pattern is a hard generation failure
// rather than a CI flap.
func contractNormalize(value any, home, path string) any {
	if placeholder, ok := contractVolatileLeaves[path]; ok {
		return placeholder
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, inner := range typed {
			out[key] = contractNormalize(inner, home, path+"."+key)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, inner := range typed {
			out[i] = contractNormalize(inner, home, fmt.Sprintf("%s[%d]", path, i))
		}
		// Slice ORDER is not frozen. `mora list` and `mora search` sort by
		// creation time, and two memories written inside the same second tie,
		// so their order flips between runs. Sorting by canonical
		// serialization makes the corpus stable. The cost is stated plainly:
		// this corpus pins the field set, the types, and the stable values —
		// not the ordering of a collection. Ordering is not what CON-05
		// promises a consumer, and freezing a tie-broken order would be
		// freezing an accident.
		sort.Slice(out, func(i, j int) bool {
			left, _ := json.Marshal(out[i])
			right, _ := json.Marshal(out[j])
			return string(left) < string(right)
		})
		return out
	case string:
		return contractNormalizeString(typed, home)
	default:
		return value
	}
}

func contractNormalizeString(value, home string) string {
	if home != "" {
		value = strings.ReplaceAll(value, home, "<home>")
		if resolved, err := filepath.EvalSymlinks(home); err == nil && resolved != home {
			value = strings.ReplaceAll(value, resolved, "<home>")
		}
	}
	value = contractVolatileTimestamp.ReplaceAllString(value, "<timestamp>")
	value = contractVolatileID.ReplaceAllString(value, "<id>")
	value = contractVolatileStamp.ReplaceAllString(value, "<stamp>")
	// Bare calendar dates are stable within a day and change at midnight, so
	// they are the one volatility class a two-run check cannot see. Rendered
	// surfaces such as the context bundle's live-tasks table carry them.
	value = contractVolatileDate.ReplaceAllString(value, "<date>")
	return value
}

// contractLiveDocuments drives every executable non-exempt registry row in the
// CURRENT temp home and returns its normalized document, keyed by the schema
// name the payload publishes.
//
// Row selection reuses Plan 01-07's tables (contractEnvelopeNonExecutable,
// contractEnvelopeArgs, contractEnvelopeOrder) rather than a second copy: a row
// that plan judged unsafe to execute is unsafe to execute here too, and the
// ordering is what makes the documents reproducible.
func contractLiveDocuments(t *testing.T) map[string]any {
	t.Helper()
	run(t, "init")
	seed := run(t, "write", "--title", "envelope seed", "--text", "envelope probe seed", "--json")
	var seedDoc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(seed), &seedDoc); err != nil || seedDoc.ID == "" {
		t.Fatalf("seed write receipt: %v\n%s", err, seed)
	}
	// Memory timestamps have SECOND granularity. The seed and the `write` row
	// below land in the same second, their created_at values tie, and the tie
	// is broken differently between processes — which reorders `mora list`,
	// `mora search`, and the memory sections rendered inside `mora context`'s
	// text bundle. The last of those is a string, so no amount of slice
	// sorting reaches it. Crossing a second boundary removes the tie at its
	// source instead of papering over it downstream.
	time.Sleep(1100 * time.Millisecond)

	home, _ := os.UserHomeDir()
	registry := loadCLIRegistry(t)
	rows := map[string]cliRegistryRow{}
	for _, row := range registry.Commands {
		if row.JSONContract == "exempt" || row.Platform != "all" {
			continue
		}
		if _, denied := contractEnvelopeNonExecutable[row.Path]; denied {
			continue
		}
		rows[row.Path] = row
	}

	docs := map[string]any{}
	seen := map[string]bool{}
	drive := func(path string) {
		row, ok := rows[path]
		if !ok || seen[path] {
			return
		}
		seen[path] = true
		args := append(strings.Fields(row.Path), contractEnvelopeArgsFor(row.Path, seedDoc.ID)...)
		stdout, stderr, err := runSplit(t, append(args, "--json")...)
		if strings.TrimSpace(stdout) == "" {
			t.Fatalf("%s --json produced no document (err=%v)\nstderr: %s", row.Path, err, stderr)
		}
		var value any
		if derr := json.Unmarshal([]byte(stdout), &value); derr != nil {
			t.Fatalf("%s --json did not decode: %v\n%s", row.Path, derr, stdout)
		}
		object, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s --json is not a JSON object; the envelope cannot ride on it", row.Path)
		}
		schema, _ := object["schema"].(string)
		if schema != row.Payload {
			t.Fatalf("%s emitted schema %q, registry says %q", row.Path, schema, row.Payload)
		}
		if _, exists := docs[schema]; exists {
			t.Fatalf("schema %q is emitted by two executed paths; the corpus is keyed by schema name", schema)
		}
		docs[schema] = contractNormalize(object, home, schema)
	}

	for _, path := range contractEnvelopeOrder {
		drive(path)
	}
	for _, row := range registry.Commands {
		drive(row.Path)
	}
	return docs
}

func contractGoldenPath(schema string) string {
	return filepath.Join(contractGoldenDir, schema+".json")
}

func contractMarshalGolden(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(body, '\n')
}

func contractReadGolden(t *testing.T, schema string) (any, bool) {
	t.Helper()
	body, err := os.ReadFile(contractGoldenPath(schema))
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("golden %s: %v", contractGoldenPath(schema), err)
	}
	return value, true
}

// TestContractGoldenCorpusIsFrozen compares today's output against the frozen
// v1 corpus and, on drift, names the ONE legal remedy for the kind of drift it
// found. Additive drift may be regenerated. Drift that DROPS a v1 key may not:
// that message names a schema_version bump instead, because regenerating would
// launder a breaking change into a green suite.
func TestContractGoldenCorpusIsFrozen(t *testing.T) {
	withTempHome(t)
	live := contractLiveDocuments(t)

	if os.Getenv(contractGoldenUpdateEnv) == "1" {
		contractRegenerateCorpus(t, live)
		return
	}

	for _, schema := range contractSortedKeys(live) {
		t.Run(schema, func(t *testing.T) {
			golden, ok := contractReadGolden(t, schema)
			if !ok {
				t.Fatalf("no frozen golden for schema %s; generate the corpus with %s=1",
					schema, contractGoldenUpdateEnv)
			}
			got := contractMarshalGolden(t, live[schema])
			want := contractMarshalGolden(t, golden)
			if bytes.Equal(want, got) {
				return
			}
			// Branch on the KIND of drift. A dropped key is never a
			// regeneration; it is a version bump.
			if findings := contractCompareShapes(schema, golden, live[schema]); len(findings) > 0 {
				t.Fatalf("%s", strings.Join(findings, "\n"))
			}
			t.Fatalf("%s drifted without losing a v1 field; regenerate the corpus with %s=1\nwant:\n%s\ngot:\n%s",
				schema, contractGoldenUpdateEnv, want, got)
		})
	}
}

// contractRegenerateCorpus writes the corpus, but only after proving two
// things the corpus is worthless without:
//
//  1. Determinism. The whole command sequence runs a second time in a fresh
//     home and every normalized document must match, so a volatile leaf the
//     normalizer misses fails HERE rather than flapping in CI.
//  2. Additive-only regeneration. A document that lost a key the committed
//     golden carries is refused, with the schema_version remedy. This is what
//     stops the update env var from being a removal-laundering path.
func contractRegenerateCorpus(t *testing.T, live map[string]any) {
	t.Helper()

	withTempHome(t)
	second := contractLiveDocuments(t)
	for _, schema := range contractSortedKeys(live) {
		other, ok := second[schema]
		if !ok {
			t.Fatalf("schema %s appeared in one generation run and not the other", schema)
		}
		if path, differs := contractFirstDifference(schema, live[schema], other); differs {
			t.Fatalf("schema %s is not deterministic: %s differs between two runs.\n"+
				"Add its pattern to contractNormalizeString or pin the clock that feeds it.", schema, path)
		}
	}

	for _, schema := range contractSortedKeys(live) {
		if golden, ok := contractReadGolden(t, schema); ok {
			if findings := contractCompareShapes(schema, golden, live[schema]); len(findings) > 0 {
				t.Fatalf("refusing to regenerate %s — regeneration is ADDITIVE ONLY:\n%s",
					schema, strings.Join(findings, "\n"))
			}
		}
	}

	if err := os.MkdirAll(contractGoldenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, schema := range contractSortedKeys(live) {
		body := contractMarshalGolden(t, live[schema])
		if err := os.WriteFile(contractGoldenPath(schema), body, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", schema, err)
		}
	}
	t.Logf("froze %d v1 goldens under %s", len(live), contractGoldenDir)
}

// TestContractGoldenCorpusIsComplete reconciles the corpus against the command
// registry so a new versioned payload cannot silently escape the compatibility
// gate the way an MCP tool without a budgetCases() row escapes T0.
//
// Scope finding, recorded rather than hidden: the plan's acceptance criterion
// asked for one golden per DISTINCT registry payload (94). That is not
// reachable — 45 of those payloads belong to rows this suite must never
// execute (destructive, network-bound, host-mutating, or platform-gated), and
// fabricating a document for them would freeze a shape no command emits. The
// corpus therefore covers every EXECUTABLE payload, and the reconciliation
// asserts golden ∪ shape-only == every non-exempt payload, with no orphans.
func TestContractGoldenCorpusIsComplete(t *testing.T) {
	registry := loadCLIRegistry(t)
	all := map[string]bool{}
	executable := map[string]bool{}
	shapeOnly := map[string]bool{}
	for _, row := range registry.Commands {
		if row.JSONContract == "exempt" {
			continue
		}
		if row.Payload == "" {
			t.Errorf("registry row %q is non-exempt with no payload name", row.Path)
			continue
		}
		all[row.Payload] = true
		_, denied := contractEnvelopeNonExecutable[row.Path]
		if row.Platform != "all" || denied {
			shapeOnly[row.Payload] = true
			continue
		}
		executable[row.Payload] = true
	}

	entries, err := os.ReadDir(contractGoldenDir)
	if err != nil {
		t.Fatalf("read %s: %v (generate with %s=1)", contractGoldenDir, err, contractGoldenUpdateEnv)
	}
	goldens := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			t.Errorf("%s holds a non-JSON file %q", contractGoldenDir, name)
			continue
		}
		goldens[strings.TrimSuffix(name, ".json")] = true
	}

	for schema := range executable {
		if !goldens[schema] {
			t.Errorf("schema %s has no frozen golden at %s; it would escape the compatibility gate",
				schema, contractGoldenPath(schema))
		}
	}
	for schema := range goldens {
		if !all[schema] {
			t.Errorf("orphan golden %s: no non-exempt registry row publishes that payload",
				contractGoldenPath(schema))
		}
		// The file name is the contract key; a golden whose document
		// disagrees with its name would be compared against the wrong shape.
		if value, ok := contractReadGolden(t, schema); ok {
			object, _ := value.(map[string]any)
			if got, _ := object["schema"].(string); got != schema {
				t.Errorf("golden %s carries schema %q", contractGoldenPath(schema), got)
			}
		}
	}
	for schema := range all {
		if !goldens[schema] && !shapeOnly[schema] {
			t.Errorf("payload %s is neither goldened nor shape-only", schema)
		}
	}
	t.Logf("corpus reconciliation: %d goldens, %d shape-only payloads, %d distinct non-exempt payloads",
		len(goldens), len(shapeOnly), len(all))
}

func contractSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// contractFirstDifference reports the first path at which two decoded JSON
// values diverge. It exists to make a non-deterministic golden name its own
// offending leaf instead of dumping two documents at the reader.
func contractFirstDifference(path string, a, b any) (string, bool) {
	switch left := a.(type) {
	case map[string]any:
		right, ok := b.(map[string]any)
		if !ok {
			return path, true
		}
		keys := map[string]bool{}
		for key := range left {
			keys[key] = true
		}
		for key := range right {
			keys[key] = true
		}
		for _, key := range contractSortedKeys(keys) {
			leftValue, leftOK := left[key]
			rightValue, rightOK := right[key]
			if leftOK != rightOK {
				return path + "." + key, true
			}
			if where, differs := contractFirstDifference(path+"."+key, leftValue, rightValue); differs {
				return where, true
			}
		}
		return "", false
	case []any:
		right, ok := b.([]any)
		if !ok || len(left) != len(right) {
			return path, true
		}
		for i := range left {
			if where, differs := contractFirstDifference(fmt.Sprintf("%s[%d]", path, i), left[i], right[i]); differs {
				return where, true
			}
		}
		return "", false
	default:
		if fmt.Sprintf("%v", a) != fmt.Sprintf("%v", b) {
			return path, true
		}
		return "", false
	}
}

// contractRemovalRemedy is the ONE legal remedy for a removed or renamed
// field, stated and nothing else. It deliberately does NOT mention
// MORA_UPDATE_CONTRACT_GOLDENS: regenerating a v1 golden to accommodate a
// removal is exactly the move this gate exists to refuse.
//
// Tone copied from mora_mcp_budget_test.go's wantRED messages — tell the
// reader the one way to make it green, not the several ways to silence it.
func contractRemovalRemedy(schema, field string) string {
	return fmt.Sprintf("field %s removed from %s: bump %s.schema_version and move the golden to %s",
		field, schema, schema, "testdata/contracts/v<N>/")
}

// contractRetypeRemedy states the same remedy for a field whose JSON type
// changed. A retype breaks a pinned consumer just as hard as a removal.
func contractRetypeRemedy(schema, field, was, now string) string {
	return fmt.Sprintf("field %s of %s changed type from %s to %s: bump %s.schema_version and move the golden to %s",
		field, schema, was, now, schema, "testdata/contracts/v<N>/")
}

// contractJSONKind names the JSON type of a decoded value.
func contractJSONKind(value any) string {
	switch value.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// contractCompareShapes walks a frozen v1 golden against today's document and
// reports every v1 field that is gone, renamed, or retyped — each as the
// remedy message, so the failure output is a set of instructions rather than a
// diff to interpret.
//
// This is the key-by-key half of the removal detector. The strict-decode half
// (json.Decoder.DisallowUnknownFields) lives in TestContractCompatRemovalIsCaught;
// this walk covers what strict decoding cannot see, namely inside an array
// that is empty in today's output, and retypes of scalar leaves.
func contractCompareShapes(schema string, golden, live any) []string {
	var findings []string
	var walk func(path string, want, got any)
	walk = func(path string, want, got any) {
		// A null in the frozen golden pins no type, so anything satisfies it.
		if want == nil {
			return
		}
		if wantKind, gotKind := contractJSONKind(want), contractJSONKind(got); wantKind != gotKind {
			if got == nil {
				findings = append(findings, contractRemovalRemedy(schema, path))
				return
			}
			findings = append(findings, contractRetypeRemedy(schema, path, wantKind, gotKind))
			return
		}
		switch wantValue := want.(type) {
		case map[string]any:
			gotValue := got.(map[string]any)
			for _, key := range contractSortedKeys(wantValue) {
				child := key
				if path != "" {
					child = path + "." + key
				}
				inner, present := gotValue[key]
				if !present {
					findings = append(findings, contractRemovalRemedy(schema, child))
					continue
				}
				walk(child, wantValue[key], inner)
			}
		case []any:
			gotValue := got.([]any)
			for i := range wantValue {
				if i >= len(gotValue) {
					break
				}
				walk(fmt.Sprintf("%s[%d]", path, i), wantValue[i], gotValue[i])
			}
		}
	}
	walk("", golden, live)
	return findings
}

// contractAnyType is the element type used wherever a shape cannot be derived.
var contractAnyType = reflect.TypeOf((*any)(nil)).Elem()

// contractShapeType builds a Go struct type that mirrors a decoded JSON
// document: one exported field per object key, tagged with that key, nested
// recursively. Fields are named F0..Fn because a JSON key is not necessarily a
// legal Go identifier; the json tag carries the real name.
//
// Building the type from a document rather than hand-writing 50 structs is what
// keeps this honest as the corpus grows. A hand-maintained struct set drifts
// silently; a derived one cannot.
func contractShapeType(value any) reflect.Type {
	switch typed := value.(type) {
	case map[string]any:
		keys := contractSortedKeys(typed)
		fields := make([]reflect.StructField, 0, len(keys))
		for i, key := range keys {
			if strings.ContainsAny(key, `"\,`) {
				// A key that cannot be expressed in a struct tag would
				// silently become an unmatched field, which would make the
				// strict decode below lie. Refuse the whole object instead.
				return contractAnyType
			}
			fields = append(fields, reflect.StructField{
				Name: fmt.Sprintf("F%d", i),
				Type: contractShapeType(typed[key]),
				Tag:  reflect.StructTag(fmt.Sprintf(`json:"%s"`, key)),
			})
		}
		return reflect.StructOf(fields)
	case []any:
		// Merge every element's keys so the element type covers the whole
		// array, not just its first row.
		merged := map[string]any{}
		object := len(typed) > 0
		for _, element := range typed {
			inner, ok := element.(map[string]any)
			if !ok {
				object = false
				break
			}
			for key, innerValue := range inner {
				if existing, seen := merged[key]; !seen || existing == nil {
					merged[key] = innerValue
				}
			}
		}
		if !object {
			return reflect.SliceOf(contractAnyType)
		}
		return reflect.SliceOf(contractShapeType(merged))
	default:
		return contractAnyType
	}
}

// contractDecodeStrict decodes `document` into a struct shaped like `shape`
// with unknown fields REFUSED. It is the stdlib half of the removal detector:
// a key the frozen golden carries that today's payload no longer has is, by
// construction, an unknown field.
func contractDecodeStrict(document []byte, shape any) error {
	target := reflect.New(contractShapeType(shape))
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target.Interface())
}

// contractUnknownField extracts the field name from encoding/json's unknown
// field error, so the failure can be restated as the remedy rather than as a
// decoder diagnostic.
var contractUnknownField = regexp.MustCompile(`unknown field "([^"]*)"`)

// TestContractCompatRemovalIsCaught is the load-bearing half.
//
// For every schema it decodes the FROZEN v1 golden against today's shape with
// DisallowUnknownFields. A field removed or renamed since v1 is an unknown
// field in that direction and fails here. The key-by-key walk runs alongside
// it because strict decoding cannot see inside an array that is empty in
// today's output, and cannot see a scalar retype.
//
// Both report the same remedy: bump the schema's `schema_version` and move the
// golden. Neither offers regeneration, because regenerating is how a removal
// would be laundered into a green suite.
func TestContractCompatRemovalIsCaught(t *testing.T) {
	withTempHome(t)
	live := contractLiveDocuments(t)

	for _, schema := range contractSortedKeys(live) {
		t.Run(schema, func(t *testing.T) {
			golden, ok := contractReadGolden(t, schema)
			if !ok {
				t.Fatalf("no frozen golden for %s", schema)
			}
			body := contractMarshalGolden(t, golden)
			if err := contractDecodeStrict(body, live[schema]); err != nil {
				if match := contractUnknownField.FindStringSubmatch(err.Error()); match != nil {
					t.Fatalf("%s", contractRemovalRemedy(schema, match[1]))
				}
				t.Fatalf("%s: frozen v1 golden no longer decodes against today's payload: %v\n%s",
					schema, err, contractRemovalRemedy(schema, "<see error>"))
			}
			if findings := contractCompareShapes(schema, golden, live[schema]); len(findings) > 0 {
				t.Fatalf("%s", strings.Join(findings, "\n"))
			}
		})
	}
}

// contractFutureField is the key injected to simulate a LATER release adding a
// field. A pinned v1 consumer must be unmoved by it.
const contractFutureField = "mora_future_field_added_by_a_later_phase"

// TestContractCompatAdditiveIsSafe is the other direction.
//
// For every schema it builds the v1 consumer type FROM the frozen golden and
// decodes today's output into it with a plain json.Unmarshal — unknown fields
// ignored, exactly as a real pinned consumer behaves. Every v1 field must come
// back carrying the value the golden carries.
//
// It then does the thing the phase actually promises Phases 3, 5, and 7:
// injects a field that does not exist today, at the top level and inside every
// array element, and asserts the v1 consumer's view is byte-identical. Additive
// safety is proven here rather than assumed from the version number.
func TestContractCompatAdditiveIsSafe(t *testing.T) {
	withTempHome(t)
	live := contractLiveDocuments(t)

	for _, schema := range contractSortedKeys(live) {
		t.Run(schema, func(t *testing.T) {
			golden, ok := contractReadGolden(t, schema)
			if !ok {
				t.Fatalf("no frozen golden for %s", schema)
			}
			want := contractMarshalGolden(t, golden)

			seen := contractPinnedConsumerView(t, golden, live[schema])
			if diff, differs := contractFirstDifference("", golden, seen); differs {
				field := strings.TrimPrefix(diff, ".")
				t.Fatalf("a consumer pinned to %s v1 no longer sees %s.\n%s",
					schema, field, contractRemovalRemedy(schema, field))
			}

			extended := contractAddFutureFields(live[schema])
			afterExtension := contractPinnedConsumerView(t, golden, extended)
			got := contractMarshalGolden(t, afterExtension)
			if !bytes.Equal(want, got) {
				t.Fatalf("%s: adding a field moved what a pinned v1 consumer sees; "+
					"the envelope is not additively safe\nwant:\n%s\ngot:\n%s", schema, want, got)
			}
		})
	}
}

// contractPinnedConsumerView decodes `document` through a type frozen at the
// golden's field set and returns what that consumer ends up holding.
func contractPinnedConsumerView(t *testing.T, golden, document any) any {
	t.Helper()
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	target := reflect.New(contractShapeType(golden))
	// A pinned consumer IGNORES fields it does not know. No DisallowUnknownFields.
	if err := json.Unmarshal(body, target.Interface()); err != nil {
		t.Fatalf("a consumer pinned to v1 can no longer decode today's output: %v", err)
	}
	round, err := json.Marshal(target.Elem().Interface())
	if err != nil {
		t.Fatal(err)
	}
	var view any
	if err := json.Unmarshal(round, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

// contractAddFutureFields simulates a later release: every object in the
// document gains a key nothing today emits.
func contractAddFutureFields(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed)+1)
		for key, inner := range typed {
			out[key] = contractAddFutureFields(inner)
		}
		out[contractFutureField] = "a field a later phase added"
		return out
	case []any:
		out := make([]any, len(typed))
		for i, inner := range typed {
			out[i] = contractAddFutureFields(inner)
		}
		return out
	default:
		return value
	}
}

// TestContractCompatRemedyMessage covers the failure text itself, so the one
// thing a future maintainer reads at 2am cannot rot untested, and proves the
// detector fires on a synthetic removal rather than only on a real one.
func TestContractCompatRemedyMessage(t *testing.T) {
	t.Run("removal message names the bump and not the regeneration", func(t *testing.T) {
		message := contractRemovalRemedy("mora.doctor.report", "storage_bytes")
		for _, want := range []string{"bump", "mora.doctor.report", "schema_version", "storage_bytes",
			"testdata/contracts/v<N>/"} {
			if !strings.Contains(message, want) {
				t.Errorf("remedy message is missing %q: %s", want, message)
			}
		}
		// Naming the update env var here would tell the reader to launder the
		// removal. The message must offer exactly one way out.
		if strings.Contains(message, contractGoldenUpdateEnv) {
			t.Errorf("remedy message offers regeneration as a way out: %s", message)
		}
	})

	t.Run("retype message names the bump", func(t *testing.T) {
		message := contractRetypeRemedy("mora.list", "memories", "array", "object")
		for _, want := range []string{"bump", "mora.list", "schema_version", "array", "object"} {
			if !strings.Contains(message, want) {
				t.Errorf("retype message is missing %q: %s", want, message)
			}
		}
	})

	t.Run("synthetic removal is caught by the strict decode", func(t *testing.T) {
		golden := map[string]any{
			"schema":         "mora.synthetic",
			"schema_version": float64(1),
			"kept":           "value",
			"removed":        "value",
			"nested":         map[string]any{"kept": "value", "removed": "value"},
		}
		today := map[string]any{
			"schema":         "mora.synthetic",
			"schema_version": float64(1),
			"kept":           "value",
			"nested":         map[string]any{"kept": "value"},
		}
		body, err := json.Marshal(golden)
		if err != nil {
			t.Fatal(err)
		}
		err = contractDecodeStrict(body, today)
		if err == nil {
			t.Fatal("strict decode accepted a golden carrying a field today's payload no longer has; " +
				"the removal detector does not fire")
		}
		match := contractUnknownField.FindStringSubmatch(err.Error())
		if match == nil {
			t.Fatalf("strict decode failed for the wrong reason: %v", err)
		}
		if got := contractRemovalRemedy("mora.synthetic", match[1]); !strings.Contains(got, "bump") {
			t.Fatalf("remedy message: %s", got)
		}
	})

	t.Run("synthetic removal is caught by the key walk", func(t *testing.T) {
		golden := map[string]any{
			"kept":    "value",
			"removed": "value",
			"rows":    []any{map[string]any{"kept": "value", "removed": "value"}},
			"retyped": "value",
		}
		today := map[string]any{
			"kept":    "value",
			"rows":    []any{map[string]any{"kept": "value"}},
			"retyped": float64(1),
		}
		findings := contractCompareShapes("mora.synthetic", golden, today)
		joined := strings.Join(findings, "\n")
		for _, want := range []string{
			contractRemovalRemedy("mora.synthetic", "removed"),
			contractRemovalRemedy("mora.synthetic", "rows[0].removed"),
			contractRetypeRemedy("mora.synthetic", "retyped", "string", "number"),
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("key walk missed a finding.\nwant: %s\ngot:\n%s", want, joined)
			}
		}
	})

	t.Run("an added field produces no finding", func(t *testing.T) {
		golden := map[string]any{"kept": "value"}
		today := map[string]any{"kept": "value", "added": "value"}
		if findings := contractCompareShapes("mora.synthetic", golden, today); len(findings) > 0 {
			t.Fatalf("adding a field must not be a breaking change: %v", findings)
		}
	})
}
