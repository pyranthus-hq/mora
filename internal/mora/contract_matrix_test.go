package mora

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// contractMatrixPlatforms is the DECLARED platform vocabulary, kept in Go rather
// than in the registry on purpose.
//
// A registry that declared its own legal values would be tautological — the same
// trap Plan 01-09 recorded for `TestCapabilitiesMatchesRegistries`, where both
// sides of the comparison read the same bytes. Holding the vocabulary here means
// a new or misspelled `platform` value is RED until a human adds it, instead of
// silently falling through to the shape-only branch of every registry-driven
// test in the package.
var contractMatrixPlatforms = map[string]bool{
	"all":                true,
	"native-seam":        true,
	"darwin-seam":        true,
	"network-seam":       true,
	"git-seam":           true,
	"git-or-bucket-seam": true,
	"loopback-seam":      true,
}

// contractMatrixJSONContracts is the declared `json_contract` vocabulary, held
// in Go for the same reason.
var contractMatrixJSONContracts = map[string]bool{
	"result":  true,
	"receipt": true,
	"exempt":  true,
}

// contractMatrixDashLedUnsweepable names executable rows the dash-led sweep must
// not drive, with the reason. Every entry is a path where appending a token
// would do real work or block, NOT a path that is allowed to accept a dash-led
// positional — an accepted dash-led token is a failure everywhere.
var contractMatrixDashLedUnsweepable = map[string]string{
	"backup":        "writes a real backup archive on every invocation",
	"index rebuild": "rebuilds the whole index; covered by the rebuild receipt tests",
	"reingest":      "re-fetches every enabled source",
	"schedule run":  "runs the real scheduled pulse job",
	"sync git":      "runs git against a real remote",
	"upgrade":       "self-replacing binary",
	"mcp":           "long-running JSON-RPC server",
	"mcp serve":     "long-running JSON-RPC server",
	"serve":         "long-running loopback HTTP server entrypoint",
	"serve http":    "long-running loopback HTTP server",
	"connect":       "interactive browser OAuth handoff",
	"init":          "interactive first-run wizard",
}

// contractMatrixRowClass is the bucket a registry row falls into. Every row must
// land in exactly one; a row that falls through fails the matrix.
type contractMatrixRowClass int

const (
	contractMatrixUnclassified contractMatrixRowClass = iota
	contractMatrixExempt
	contractMatrixSeam
	contractMatrixUnsweepable
	contractMatrixExecutable
)

// TestCLIContractMatrix is the phase-wide, registry-driven contract matrix
// CON-06 names. It deliberately does NOT re-execute the payload/envelope half:
// TestContractEveryPayloadIsVersioned already drives every executable non-exempt
// row, asserts `schema`/`schema_version`, shape-checks the seam rows against the
// Go source, and fails on a coverage gap. Re-running those ~50 commands here
// would double the package's wall time and prove nothing new.
//
// What this matrix adds, and what nothing else in the package asserts:
//
//  1. Every one of the registry's rows — including the 35 exempt ones, which the
//     envelope test skips past — is VISITED and lands in exactly one class. The
//     visited count must equal the row count.
//  2. `platform` and `json_contract` are checked against a Go-side vocabulary,
//     so a misspelled `platform` fails loudly instead of quietly demoting a row
//     to a shape-only assertion in four other tests.
//  3. The dash-led positional bug CLASS is swept over the registry, not over a
//     hand-kept list of the seven instances that were found the hard way. A
//     command added in a later phase is swept the day its registry row lands.
func TestCLIContractMatrix(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--title", "matrix seed", "--text", "contract matrix probe seed")

	registry := loadCLIRegistry(t)

	// The two declared query-slot exceptions, indexed for the sweep below.
	queryException := map[string]bool{}
	for _, path := range contractDashLedQuerySlots {
		queryException[strings.Join(path, " ")] = true
	}

	classified := map[contractMatrixRowClass]int{}
	visited := map[string]bool{}

	for _, row := range registry.Commands {
		row := row

		if visited[row.Path] {
			t.Errorf("registry declares %q more than once", row.Path)
			continue
		}
		visited[row.Path] = true

		if !contractMatrixPlatforms[row.Platform] {
			t.Errorf("registry row %q has unrecognized platform %q; add it to "+
				"contractMatrixPlatforms after deciding how the contract tests "+
				"should treat it, or fix the typo", row.Path, row.Platform)
			continue
		}
		if !contractMatrixJSONContracts[row.JSONContract] {
			t.Errorf("registry row %q has unrecognized json_contract %q", row.Path, row.JSONContract)
			continue
		}

		// Zero value is contractMatrixUnclassified. The switch has NO default on
		// purpose: a row matching none of the branches keeps the zero value and
		// fails below, so a future classification condition cannot silently
		// swallow rows the way `default:` would.
		var class contractMatrixRowClass
		switch {
		case row.JSONContract == "exempt":
			class = contractMatrixExempt
			if strings.TrimSpace(row.Reason) == "" {
				t.Errorf("%s is exempt with no reason", row.Path)
			}
			if row.Payload != "" {
				t.Errorf("%s is exempt but names payload %q", row.Path, row.Payload)
			}
		case row.Platform != "all":
			class = contractMatrixSeam
			if strings.TrimSpace(row.Payload) == "" {
				t.Errorf("%s is a %s row with no payload name", row.Path, row.Platform)
			}
		case row.Platform == "all":
			class = contractMatrixExecutable
			if strings.TrimSpace(row.Payload) == "" {
				t.Errorf("%s is executable with no payload name", row.Path)
			}
		}

		// The dash-led sweep runs over exempt and executable `platform: all`
		// rows alike: a dispatch-only group verb must refuse a dash-led token
		// just as a leaf must. Seam rows are not driven at all.
		if row.Platform == "all" {
			if _, denied := contractMatrixDashLedUnsweepable[row.Path]; denied {
				if class == contractMatrixExecutable {
					class = contractMatrixUnsweepable
				}
			} else {
				t.Run("dash-led/"+strings.ReplaceAll(row.Path, " ", "/"), func(t *testing.T) {
					contractMatrixAssertDashLedRefused(t, row.Path, queryException[row.Path])
				})
			}
		}

		if class == contractMatrixUnclassified {
			t.Errorf("%s fell through every classification branch", row.Path)
			continue
		}
		classified[class]++
	}

	total := classified[contractMatrixExempt] + classified[contractMatrixSeam] +
		classified[contractMatrixUnsweepable] + classified[contractMatrixExecutable]
	if total != len(registry.Commands) {
		t.Fatalf("coverage gap: %d classified rows != %d registry rows "+
			"(exempt %d, seam %d, unsweepable %d, executable %d)",
			total, len(registry.Commands),
			classified[contractMatrixExempt], classified[contractMatrixSeam],
			classified[contractMatrixUnsweepable], classified[contractMatrixExecutable])
	}
	t.Logf("contract matrix: %d rows — %d exempt, %d seam, %d unsweepable, %d executable",
		total, classified[contractMatrixExempt], classified[contractMatrixSeam],
		classified[contractMatrixUnsweepable], classified[contractMatrixExecutable])

	// The unsweepable list must not rot into an amnesty. Every entry has to name
	// a real registry row, so a deleted or renamed command cannot leave a
	// permanent hole behind it.
	for path := range contractMatrixDashLedUnsweepable {
		if !visited[path] {
			t.Errorf("contractMatrixDashLedUnsweepable names %q, which is not a registry row", path)
		}
	}
	for path := range contractMatrixDashLedIgnored {
		if !visited[path] {
			t.Errorf("contractMatrixDashLedIgnored names %q, which is not a registry row", path)
		}
	}
	for path := range queryException {
		if !visited[path] {
			t.Errorf("contractDashLedQuerySlots names %q, which is not a registry row", path)
		}
	}
}

// contractMatrixSharedGuardPaths are the call sites of refuseDashLedPositional
// (helpers.go). They need a witness of their own, and the reason is a measured
// hole rather than a hypothetical.
//
// Mutation, run while writing this matrix: neutering refuseDashLedPositional to
// `return nil` left the ENTIRE package green. Every one of these paths uses the
// token as a LOOKUP KEY, so with the guard gone they still fail — one step
// later, at "no such entry" — and an err != nil check cannot tell the two apart.
// The shared check 01-07 introduced was therefore unwitnessed: it could have
// been deleted and no test would have noticed.
//
// This closes that. These paths must be refused BY THE GUARD, identified by its
// phrasing, not by a downstream lookup miss. The difference matters: a lookup
// miss means the mistyped flag was carried into the lookup, which is one refactor
// away from being carried into a mutation.
var contractMatrixSharedGuardPaths = []string{
	"merge undo",
	"teach undo",
	"teach identity undo",
	"mcp proposals approve",
	"mcp proposals reject",
}

// TestContractSharedDashLedGuardIsWitnessed is the mutation witness for
// refuseDashLedPositional. Neutering that function must fail this test.
func TestContractSharedDashLedGuardIsWitnessed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	for _, path := range contractMatrixSharedGuardPaths {
		path := path
		t.Run(strings.ReplaceAll(path, " ", "/"), func(t *testing.T) {
			args := append(strings.Fields(path), "--bogus-positional")
			stdout, _, err := runSplit(t, args...)
			if err == nil {
				t.Fatalf("%s accepted a dash-led lookup key\nstdout: %s", path, stdout)
			}
			if !strings.Contains(err.Error(), "is a flag, not the") {
				t.Fatalf("%s was refused by something OTHER than refuseDashLedPositional, "+
					"which means the dash-led token reached the lookup: %v\n"+
					"If the guard was intentionally removed here, remove the path from "+
					"contractMatrixSharedGuardPaths too — do not relax this assertion.",
					path, err)
			}
		})
	}
	// The list must name real call sites, so a deleted guard cannot leave a
	// stale row asserting nothing.
	body, err := os.ReadFile("helpers.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "func refuseDashLedPositional(") {
		t.Fatal("refuseDashLedPositional is gone from helpers.go; this witness is stale")
	}
}

// contractMatrixDashLedIgnored names the `platform: all` paths that SILENTLY
// ACCEPT a dash-led trailing token today, each with the reason it is recorded
// rather than fixed inside this plan. Measured by the sweep below, not assumed —
// before this matrix existed, nobody knew this set was non-empty.
//
// None of them STORES the token or uses it as a lookup key, which is the bug
// class Plans 01-05 and 01-07 closed (`tasks add --json` created a task named
// "--json"). These ignore it. That is a lesser defect — silent acceptance of a
// mistyped flag — and it is pinned here so it cannot grow: an entry that starts
// refusing must be deleted, and a path NOT on this list that starts accepting
// fails the matrix. That is what closes the CLASS rather than the instances.
var contractMatrixDashLedIgnored = map[string]string{
	"help":   "help text ignores trailing arguments by universal CLI convention",
	"--help": "help alias",
	"-h":     "help alias",

	// Claude Code owns this stdout: these emit the FOREIGN hookSpecificOutput
	// envelope, and changing their argument handling risks the hook contract for
	// a benefit Mora does not own. Deliberately untouched.
	"hook session-start": "emits Claude Code's foreign hook envelope; its argument contract is not ours",
	"hook recall":        "emits Claude Code's foreign hook envelope; its argument contract is not ours",

	// These four MUTATE configuration while ignoring a mistyped flag, so they are
	// the most interesting entries on the list. Recorded for the phase checkpoint
	// rather than changed here: making them refuse is a behavior change to a
	// shipped surface, in the plan whose purpose is to freeze behavior.
	"usage on":          "toggles usage tracking; ignores a trailing token instead of refusing it",
	"usage off":         "toggles usage tracking; ignores a trailing token instead of refusing it",
	"usage queries on":  "toggles query capture; ignores a trailing token instead of refusing it",
	"usage queries off": "toggles query capture; ignores a trailing token instead of refusing it",

	"teach consent status": "read-only receipt; ignores a trailing token instead of refusing it",
}

// contractMatrixAssertDashLedRefused drives one path with a dash-led token in
// its first positional slot.
//
// The invariant asserted here is the CLASS: a dash-led token must never be
// SILENTLY ACCEPTED. A command with no positional slot refuses it as an unknown
// flag or an unexpected argument; a command with one must refuse it through
// refuseDashLedPositional. Either refusal is correct, and an error that merely
// QUOTES the token — `unknown subcommand "--bogus-positional"` — is a refusal,
// not a defect. The stricter "the token was used as a lookup key" reading is
// asserted where it belongs, over the paths that actually take a lookup key, by
// TestContractDashLedPositionalsAreRefusedEverywhere. Duplicating it over every
// registry row only produced false positives on dispatch errors.
func contractMatrixAssertDashLedRefused(t *testing.T, path string, isQueryException bool) {
	t.Helper()

	// The token is passed ALONE. An earlier draft appended `--yes` to get past
	// confirmation gates, and that silently defeated the sweep: with the guard in
	// `tasks add` removed, the reopened bug did NOT fail, because the extra
	// `--yes` is an unknown flag there and produced a refusal of its own. A probe
	// that can be rescued by its own scaffolding proves nothing. A confirmation
	// gate refusing first is fine — it means the token mutated nothing.
	args := append(strings.Fields(path), "--bogus-positional")
	stdout, _, err := runSplit(t, args...)

	if isQueryException {
		// `search` and `think` take FREE TEXT, so a dash-led token is query
		// input, not a mistyped flag. Phase 01 decided to keep and publish that
		// rather than break a legitimate search for a token starting with a
		// dash — see docs/architecture/22-cli-contracts.md. The exception is
		// PINNED: if one of these starts refusing, delete it from
		// contractDashLedQuerySlots rather than loosening this test.
		if err != nil {
			t.Fatalf("%s now REFUSES a dash-led query token; it is no longer an "+
				"exception — remove it from contractDashLedQuerySlots: %v", path, err)
		}
		return
	}

	if reason, ignored := contractMatrixDashLedIgnored[path]; ignored {
		if err == nil {
			return
		}
		t.Fatalf("%s now REFUSES a dash-led trailing token (%v), so it is no longer on "+
			"the ignored list (%q) — delete the entry", path, err, reason)
	}

	if err == nil {
		t.Fatalf("%s ACCEPTED a dash-led positional instead of refusing it. This is the "+
			"silent-acceptance class Plans 01-05 and 01-07 closed for mutation paths: a "+
			"mistyped flag becomes input. Guard the slot with refuseDashLedPositional, or, "+
			"if it genuinely must ignore extras, add it to contractMatrixDashLedIgnored "+
			"with a reason.\nstdout: %s", path, stdout)
	}
}

// TestCLIContractProseExemptionsAreDeclared enforces adopted default C5, the
// scope boundary of CON-06.
//
// Two claims, both machine-checked:
//
//  1. `prose_assertion_exemptions` names the test files that pin HUMAN text on
//     purpose. The exemption is proved HONEST rather than taken on trust: each
//     listed file must contain no `--json` call site at all. A blanket exemption
//     therefore cannot be used to smuggle a machine-surface assertion past
//     CON-06 — the moment one of these files starts driving `--json`, it stops
//     qualifying and this test says so.
//  2. `json_substring_assertion_backlog` names the files that assert a SUBSTRING
//     over `--json` stdout instead of decoding the document. That is the same
//     defect family as `len(d) > 0` over an enveloped payload: it keeps passing
//     whatever the shape becomes, so it does not hold the contract it appears to
//     hold. The backlog is closed FORWARD — a new file joining it fails here —
//     and it cannot rot, because a listed file that no longer offends fails too.
//
// The analysis is deliberately narrow: see contractMatrixProseOffenders.
func TestCLIContractProseExemptionsAreDeclared(t *testing.T) {
	registry := loadCLIRegistry(t)
	if len(registry.Contract.ProseAssertionExemptions) == 0 {
		t.Fatal("registry declares no contract.prose_assertion_exemptions")
	}
	for _, required := range []string{"mora_help_guards_test.go", "mora_usage_test.go"} {
		found := false
		for _, name := range registry.Contract.ProseAssertionExemptions {
			if name == required {
				found = true
			}
		}
		if !found {
			t.Errorf("prose_assertion_exemptions must name %s", required)
		}
	}

	fset := token.NewFileSet()
	parse := func(name string) *ast.File {
		t.Helper()
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return file
	}

	// Claim 1 — every human-text exemption really is human-only.
	for _, name := range registry.Contract.ProseAssertionExemptions {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("prose_assertion_exemptions names %s, which does not exist", name)
			continue
		}
		var jsonSites int
		ast.Inspect(parse(name), func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && contractMatrixIsJSONRun(call) {
				jsonSites++
			}
			return true
		})
		if jsonSites > 0 {
			t.Errorf("prose_assertion_exemptions names %s, but it drives --json at %d call "+
				"site(s); it is no longer outside CON-06's scope, so it cannot be exempt "+
				"as a human-text test", name, jsonSites)
		}
	}

	// Claim 2 — the substring-over-JSON backlog is exact.
	backlog := registry.Contract.JSONSubstringAssertionBacklog
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	offendingFiles := map[string]bool{}
	var undeclared []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, "_test.go") {
			continue
		}
		ast.Inspect(parse(name), func(node ast.Node) bool {
			fn, ok := node.(*ast.FuncDecl)
			if !ok {
				return true
			}
			hits := contractMatrixProseOffenders(fn)
			if len(hits) == 0 {
				return true
			}
			offendingFiles[name] = true
			if _, declared := backlog[name]; !declared {
				undeclared = append(undeclared,
					name+":"+fn.Name.Name+" — "+strings.Join(hits, ", "))
			}
			return true
		})
	}
	if len(undeclared) > 0 {
		sort.Strings(undeclared)
		t.Fatalf("%d test function(s) assert a substring over --json stdout without being "+
			"declared in contract.json_substring_assertion_backlog:\n  %s\n"+
			"Decode the document and assert on a field instead. A substring assertion over "+
			"an enveloped payload keeps passing after the shape changes, which is the same "+
			"defect the release harness carried with `len(d) > 0`.",
			len(undeclared), strings.Join(undeclared, "\n  "))
	}
	for name, reason := range backlog {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("json_substring_assertion_backlog entry %s has no reason", name)
		}
		if !offendingFiles[name] {
			t.Errorf("json_substring_assertion_backlog names %s, but nothing in it asserts a "+
				"substring over --json stdout any more; remove the entry", name)
		}
	}
	t.Logf("prose scope: %d human-text exemption(s), %d file(s) on the substring backlog",
		len(registry.Contract.ProseAssertionExemptions), len(backlog))
}

// contractMatrixProseOffenders returns the substring assertions inside fn that
// are applied to the STDOUT of a `--json` invocation.
//
// The analysis is deliberately narrow, because a coarse version is useless: an
// unscoped "does this function mention strings.Contains" check flags 30 healthy
// tests that happen to assert an error message or a human-mode rendering in the
// same function, and a guard that cries wolf gets an exemption list long enough
// to exempt everything.
//
// So it does real, local dataflow. Assignments whose right-hand side is a
// `run(t, ..., "--json")` or `runSplit(t, ..., "--json")` call bind their FIRST
// result — the machine document — to a name. A strings.Contains / HasPrefix /
// HasSuffix whose first argument is one of those names is asserting prose over a
// machine surface, and is reported. Anything else is left alone.
func contractMatrixProseOffenders(fn *ast.FuncDecl) []string {
	jsonStdout := map[string]bool{}
	ast.Inspect(fn, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 || len(assign.Lhs) == 0 {
			return true
		}
		rhs, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok || !contractMatrixIsJSONRun(rhs) {
			return true
		}
		// run returns the document; runSplit returns (stdout, stderr, err), and
		// only stdout carries it. Either way that is Lhs[0].
		if ident, ok := assign.Lhs[0].(*ast.Ident); ok && ident.Name != "_" {
			jsonStdout[ident.Name] = true
		}
		return true
	})
	if len(jsonStdout) == 0 {
		return nil
	}

	var offenders []string
	ast.Inspect(fn, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "strings" {
			return true
		}
		switch sel.Sel.Name {
		case "Contains", "HasPrefix", "HasSuffix":
		default:
			return true
		}
		subject, ok := call.Args[0].(*ast.Ident)
		if !ok || !jsonStdout[subject.Name] {
			return true
		}
		offenders = append(offenders, "strings."+sel.Sel.Name+"("+subject.Name+", ...)")
		return true
	})
	return offenders
}

// contractMatrixIsJSONRun reports whether expr is a call to the in-process CLI
// runner carrying the literal "--json".
func contractMatrixIsJSONRun(call *ast.CallExpr) bool {
	ident, ok := call.Fun.(*ast.Ident)
	if !ok || (ident.Name != "run" && ident.Name != "runSplit") {
		return false
	}
	for _, arg := range call.Args {
		if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING &&
			strings.Trim(lit.Value, `"`) == "--json" {
			return true
		}
	}
	return false
}
