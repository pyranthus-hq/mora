package mora

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	indexpkg "github.com/pyranthus-hq/mora/internal/index"
)

// The error taxonomy is a published contract (CON-03/CON-07). These tests are
// the machinery that stops it from drifting: internal/mora/errors.go declares
// the codes, internal/mora/eval/error-code-registry.json publishes them, and a
// go/ast sweep refuses to let one change without the other.

type errorCodeRegistry struct {
	SchemaVersion int    `json:"schema_version"`
	Issue         int    `json:"issue"`
	AuditedAt     string `json:"audited_at"`
	Contract      struct {
		DriftAndMutationTest string `json:"drift_and_mutation_test"`
		ExitCodeTest         string `json:"exit_code_test"`
		Sweep                string `json:"sweep"`
		Mutation             string `json:"mutation"`
		ClassPolicy          string `json:"class_policy"`
		ExitCodePolicy       string `json:"exit_code_policy"`
		AdditivePolicy       string `json:"additive_policy"`
	} `json:"contract"`
	Classes           []errorClassRow `json:"classes"`
	ExitCodes         []exitCodeRow   `json:"exit_codes"`
	ReservedExitCodes struct {
		From   int    `json:"from"`
		To     int    `json:"to"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"reserved_exit_codes"`
	FirstAllocatableExitCode int            `json:"first_allocatable_exit_code"`
	Codes                    []errorCodeRow `json:"codes"`
}

type errorClassRow struct {
	Class    string `json:"class"`
	ExitCode int    `json:"exit_code"`
	Meaning  string `json:"meaning"`
}

type exitCodeRow struct {
	Code    int    `json:"code"`
	Status  string `json:"status"`
	Meaning string `json:"meaning"`
	Source  string `json:"source"`
	Witness string `json:"witness"`
}

type errorCodeRow struct {
	Code       string `json:"code"`
	Class      string `json:"class"`
	ErrorClass string `json:"error_class,omitempty"`
	ExitCode   int    `json:"exit_code"`
	Retryable  bool   `json:"retryable"`
	Meaning    string `json:"meaning"`
}

func loadErrorCodeRegistry(t *testing.T) errorCodeRegistry {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("eval", "error-code-registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry errorCodeRegistry
	if err := json.Unmarshal(body, &registry); err != nil {
		t.Fatalf("parse error code registry: %v", err)
	}
	return registry
}

// parseErrorCodeConstants is the go/ast sweep, following cli_registry_test.go's
// parseProductionFunctions rather than a regex: it parses every non-test .go
// file in this package and collects each string literal bound to an identifier
// named errCode*. The returned map is code literal -> declaring identifier, so a
// failure can name the constant a maintainer has to look at.
func parseErrorCodeConstants(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	codes := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range spec.Names {
				if !strings.HasPrefix(name.Name, "errCode") {
					continue
				}
				if i >= len(spec.Values) {
					t.Fatalf("%s: %s has no value; an error code must be bound to a string literal, not derived from iota",
						entry.Name(), name.Name)
				}
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s: %s must be bound to a plain string literal so the registry sweep can read it",
						entry.Name(), name.Name)
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %s literal %s: %v", entry.Name(), name.Name, lit.Value, err)
				}
				if prior, dup := codes[value]; dup {
					t.Fatalf("error code %q is declared twice: %s and %s", value, prior, name.Name)
				}
				codes[value] = name.Name
			}
			return true
		})
	}
	return codes
}

// TestErrorCodeRegistryMatchesSource is the contract this registry names. It
// fails in BOTH directions: a code constructed in source with no registry row,
// and a registry row with no constant behind it.
//
// MUTATION: add `errCodeFoo = "foo.bar"` to errors.go, or delete any row from
// `codes` in error-code-registry.json. Either must fail this test by name.
func TestErrorCodeRegistryMatchesSource(t *testing.T) {
	registry := loadErrorCodeRegistry(t)
	if registry.SchemaVersion != 1 || registry.Issue != 416 {
		t.Fatalf("registry header = schema %d issue %d, want schema 1 issue 416", registry.SchemaVersion, registry.Issue)
	}
	if registry.Contract.DriftAndMutationTest != "TestErrorCodeRegistryMatchesSource" {
		t.Fatalf("registry contract names %q as its drift test; this test is TestErrorCodeRegistryMatchesSource",
			registry.Contract.DriftAndMutationTest)
	}
	if registry.Contract.Mutation == "" || registry.Contract.Sweep == "" {
		t.Fatal("registry contract must state the sweep and the mutation that breaks it")
	}

	declared := parseErrorCodeConstants(t)
	registered := map[string]errorCodeRow{}
	for _, row := range registry.Codes {
		if _, dup := registered[row.Code]; dup {
			t.Fatalf("registry lists %q twice", row.Code)
		}
		registered[row.Code] = row
	}

	var unregistered, unconstructed []string
	for code, ident := range declared {
		if _, ok := registered[code]; !ok {
			unregistered = append(unregistered, fmt.Sprintf("%s (%s)", code, ident))
		}
	}
	for code := range registered {
		if _, ok := declared[code]; !ok {
			unconstructed = append(unconstructed, code)
		}
	}
	sort.Strings(unregistered)
	sort.Strings(unconstructed)
	if len(unregistered) > 0 {
		t.Errorf("constructed in source but NOT registered in eval/error-code-registry.json: %v", unregistered)
	}
	if len(unconstructed) > 0 {
		t.Errorf("registered in eval/error-code-registry.json but NEVER constructed in source: %v", unconstructed)
	}
	if len(unregistered) > 0 || len(unconstructed) > 0 {
		t.FailNow()
	}

	// The class vocabulary is closed and identical on both sides.
	registryClasses := map[string]bool{}
	for _, row := range registry.Classes {
		if registryClasses[row.Class] {
			t.Fatalf("registry declares class %q twice", row.Class)
		}
		registryClasses[row.Class] = true
		if row.Meaning == "" {
			t.Fatalf("class %q has no meaning", row.Class)
		}
		if got := exitCodeForClass(row.Class); got != row.ExitCode {
			t.Errorf("class %q: registry exit_code %d, exitCodeForClass %d", row.Class, row.ExitCode, got)
		}
	}
	sourceClasses := map[string]bool{}
	for class := range exitCodeByClass {
		sourceClasses[class] = true
	}
	assertSameStringSet(t, "class vocabulary", "exitCodeByClass", sourceClasses, "the registry", registryClasses)

	// Every row agrees with the source tables, code by code.
	connectorErrorClasses := map[string]bool{}
	for _, row := range registry.Codes {
		if row.Meaning == "" {
			t.Errorf("%s: no meaning", row.Code)
		}
		if !registryClasses[row.Class] {
			t.Errorf("%s: class %q is not in the declared class vocabulary", row.Code, row.Class)
		}
		if got := classForErrorCode(row.Code); got != row.Class {
			t.Errorf("%s: registry class %q, classForErrorCode %q", row.Code, row.Class, got)
		}
		if got := exitCodeForClass(row.Class); got != row.ExitCode {
			t.Errorf("%s: registry exit_code %d, exitCodeForClass(%q) %d", row.Code, row.ExitCode, row.Class, got)
		}
		if row.Class == errClassConnector {
			if row.ErrorClass == "" {
				t.Errorf("%s: a connector row must carry a CON-07 error_class", row.Code)
				continue
			}
			if got := connectorErrorClassOf(row.Code); got != row.ErrorClass {
				t.Errorf("%s: registry error_class %q, connectorErrorClassOf %q", row.Code, row.ErrorClass, got)
			}
			if connectorErrorClasses[row.ErrorClass] {
				t.Errorf("error_class %q is claimed by more than one code; CON-07 requires them pairwise distinct", row.ErrorClass)
			}
			connectorErrorClasses[row.ErrorClass] = true
			continue
		}
		if row.ErrorClass != "" {
			t.Errorf("%s: error_class %q on a non-connector row", row.Code, row.ErrorClass)
		}
	}

	wantErrorClasses := map[string]bool{
		connectorClassMalformed:    true,
		connectorClassUnavailable:  true,
		connectorClassUnauthorized: true,
		connectorClassStale:        true,
		connectorClassEmpty:        true,
		connectorClassUnclassified: true,
	}
	assertSameStringSet(t, "CON-07 error_class set", "errors.go", wantErrorClasses, "the registry", connectorErrorClasses)

	// No registry row may claim a status in the reserved band, and none may
	// claim a low code that is not one of the three grandfathered ones.
	for _, row := range registry.Codes {
		assertExitCodeIsAllocatable(t, fmt.Sprintf("registry row %s", row.Code), row.ExitCode)
	}
}

// TestExitCodeAllocationIsGrandfathered pins the exit-code half of CON-03: the
// three shipped statuses keep their meanings, 3 through 9 stay unused, and
// exitCodeForClass can never invent a status outside that policy.
//
// MUTATION: make exitCodeForClass return 3 for any class, or add a fourth entry
// to grandfatheredExitCodes without a registry row. Either must fail here.
func TestExitCodeAllocationIsGrandfathered(t *testing.T) {
	registry := loadErrorCodeRegistry(t)
	if registry.Contract.ExitCodeTest != "TestExitCodeAllocationIsGrandfathered" {
		t.Fatalf("registry contract names %q as its exit-code test", registry.Contract.ExitCodeTest)
	}

	registered := map[int]exitCodeRow{}
	for _, row := range registry.ExitCodes {
		if _, dup := registered[row.Code]; dup {
			t.Fatalf("registry lists exit code %d twice", row.Code)
		}
		if row.Status != "grandfathered" {
			t.Errorf("exit code %d: status %q, want grandfathered — this plan allocated none", row.Code, row.Status)
		}
		if row.Witness == "" || row.Source == "" {
			t.Errorf("exit code %d: needs both a production source and a witness test", row.Code)
		}
		registered[row.Code] = row
	}
	for code, meaning := range grandfatheredExitCodes {
		row, ok := registered[code]
		if !ok {
			t.Errorf("exit code %d is declared in grandfatheredExitCodes but not published in the registry", code)
			continue
		}
		if row.Meaning == "" {
			t.Errorf("exit code %d (%s): registry meaning is empty", code, meaning)
		}
	}
	for code := range registered {
		if _, ok := grandfatheredExitCodes[code]; !ok {
			t.Errorf("exit code %d is published in the registry but not declared in grandfatheredExitCodes", code)
		}
	}

	// The shipped statuses are exactly the ones the rest of the tree produces.
	if got := grandfatheredExitCodes[loopSkipExitCode]; got == "" || loopSkipExitCode != 10 {
		t.Fatalf("loopSkipExitCode = %d (%q); the loop skip status is grandfathered at 10", loopSkipExitCode, got)
	}
	if exitCodePulseUnhealthy != 2 {
		t.Fatalf("exitCodePulseUnhealthy = %d, want the shipped 2", exitCodePulseUnhealthy)
	}
	if exitCodeGenericFailure != 1 {
		t.Fatalf("exitCodeGenericFailure = %d, want the shipped 1", exitCodeGenericFailure)
	}

	if registry.ReservedExitCodes.From != exitCodeReservedLow || registry.ReservedExitCodes.To != exitCodeReservedHigh {
		t.Errorf("registry reserves %d..%d, source reserves %d..%d",
			registry.ReservedExitCodes.From, registry.ReservedExitCodes.To, exitCodeReservedLow, exitCodeReservedHigh)
	}
	if registry.FirstAllocatableExitCode != exitCodeFirstAllocatable {
		t.Errorf("registry first_allocatable_exit_code = %d, source = %d",
			registry.FirstAllocatableExitCode, exitCodeFirstAllocatable)
	}

	// exitCodeForClass never returns a reserved status, and never returns a low
	// status that is not one of the three grandfathered ones.
	for class := range exitCodeByClass {
		assertExitCodeIsAllocatable(t, "exitCodeForClass("+class+")", exitCodeForClass(class))
	}
	assertExitCodeIsAllocatable(t, "exitCodeForClass(unknown class)", exitCodeForClass("no.such.class"))
	if got := exitCodeForClass("no.such.class"); got != exitCodeGenericFailure {
		t.Errorf("exitCodeForClass(unknown) = %d, want the generic failure status %d", got, exitCodeGenericFailure)
	}

	// The exit code an agent actually observes comes through moraError.
	for code, class := range moraErrorCodeClass {
		err := newCodedError(code, nil, "synthetic")
		if err.Class != class {
			t.Errorf("newCodedError(%q).Class = %q, want %q", code, err.Class, class)
		}
		assertExitCodeIsAllocatable(t, "moraError("+code+").ExitCode()", err.ExitCode())
		got, ok := ExitCodeFor(err)
		if !ok || got != err.ExitCode() {
			t.Errorf("ExitCodeFor(moraError %q) = (%d, %v), want (%d, true)", code, got, ok, err.ExitCode())
		}
	}
}

func assertExitCodeIsAllocatable(t *testing.T, what string, code int) {
	t.Helper()
	if code >= exitCodeReservedLow && code <= exitCodeReservedHigh {
		t.Errorf("%s = %d, which is inside the permanently reserved %d..%d band",
			what, code, exitCodeReservedLow, exitCodeReservedHigh)
		return
	}
	if code >= exitCodeFirstAllocatable {
		return
	}
	if _, ok := grandfatheredExitCodes[code]; !ok {
		t.Errorf("%s = %d: a status below %d must be one of the grandfathered codes %v",
			what, code, exitCodeFirstAllocatable, sortedExitCodes(grandfatheredExitCodes))
	}
}

// assertSameStringSet reports both directions of a vocabulary drift by name, so
// a failure says which side is missing what instead of printing two sets.
func assertSameStringSet(t *testing.T, what, leftName string, left map[string]bool, rightName string, right map[string]bool) {
	t.Helper()
	var missingRight, missingLeft []string
	for value := range left {
		if !right[value] {
			missingRight = append(missingRight, value)
		}
	}
	for value := range right {
		if !left[value] {
			missingLeft = append(missingLeft, value)
		}
	}
	sort.Strings(missingRight)
	sort.Strings(missingLeft)
	if len(missingRight) > 0 {
		t.Errorf("%s: in %s but missing from %s: %v", what, leftName, rightName, missingRight)
	}
	if len(missingLeft) > 0 {
		t.Errorf("%s: in %s but missing from %s: %v", what, rightName, leftName, missingLeft)
	}
}

func sortedExitCodes(codes map[int]string) []int {
	out := make([]int, 0, len(codes))
	for code := range codes {
		out = append(out, code)
	}
	sort.Ints(out)
	return out
}

// TestMoraErrorUnwrapsPackageSentinels proves the taxonomy is additive over the
// existing errors.New sentinels: wrapping one in a typed moraError must not
// break an errors.Is check that already ships.
func TestMoraErrorUnwrapsPackageSentinels(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     string
		sentinel error
		other    error
	}{
		{"index unmarkable", errCodeIndexUnavailable, indexpkg.ErrUnmarkable, errEmbedderUnavailable},
		{"embedder unavailable", errCodeConnectorUnavailable, errEmbedderUnavailable, indexpkg.ErrUnmarkable},
		{"rebuild blocked", errCodeDataCorrupt, errRebuildBlocked, errLoopLockHeld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := newCodedError(tc.code, tc.sentinel, "wrapped: %v", tc.sentinel)
			if !errors.Is(wrapped, tc.sentinel) {
				t.Fatalf("errors.Is lost %v through a moraError wrap", tc.sentinel)
			}
			if errors.Is(wrapped, tc.other) {
				t.Fatalf("errors.Is matched the wrong sentinel %v", tc.other)
			}
			var typed moraError
			if !errors.As(wrapped, &typed) || typed.Code != tc.code {
				t.Fatalf("errors.As did not recover the code %q", tc.code)
			}
			// Wrapping again must not hide either half.
			outer := fmt.Errorf("outer: %w", wrapped)
			if !errors.Is(outer, tc.sentinel) || !errors.As(outer, &typed) {
				t.Fatal("a second wrap hid the sentinel or the code")
			}
		})
	}
}

// TestConnectorErrorClassBackfill pins the read-time rule for records persisted
// before this taxonomy shipped: Mora reads an untyped failure as unclassified
// rather than rewriting the file on disk to add a field.
func TestConnectorErrorClassBackfill(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      string
		lastError string
		want      string
		wantClass string
	}{
		{"typed code wins", errCodeConnectorMalformed, "some prose", errCodeConnectorMalformed, connectorClassMalformed},
		{"historic failure backfills", "", "database is locked", errCodeConnectorUnclassified, connectorClassUnclassified},
		{"healthy record stays empty", "", "", "", connectorClassUnclassified},
		{"unknown code reads unclassified", "connector.from_the_future", "", "connector.from_the_future", connectorClassUnclassified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := syncErrorCodeOrUnclassified(tc.code, tc.lastError); got != tc.want {
				t.Errorf("syncErrorCodeOrUnclassified(%q, %q) = %q, want %q", tc.code, tc.lastError, got, tc.want)
			}
			if got := connectorErrorClassOf(tc.code); got != tc.wantClass {
				t.Errorf("connectorErrorClassOf(%q) = %q, want %q", tc.code, got, tc.wantClass)
			}
		})
	}
}
