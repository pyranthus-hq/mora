package mora

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// contractEnvelopeArgs supplies the arguments each executable registry row needs
// to produce a REAL document. Without them a row measures as a usage error and
// the coverage assertion passes vacuously — which is how three separate plans
// each reported a different, wrong remaining scope for this phase.
//
// A row absent from this map is driven with `--json` alone.
var contractEnvelopeArgs = map[string][]string{
	"write":                   {"--title", "envelope probe", "--text", "envelope probe body"},
	"read":                    {contractEnvelopeSeedID},
	"search":                  {"envelope"},
	"think":                   {"what happened"},
	"tasks add":               {"envelope probe task"},
	"tasks done":              {"envelope probe task"},
	"loop register":           {"envelope-loop", "--cadence", "daily", "--command", "mora pulse"},
	"loop begin":              {"envelope-loop"},
	"loop status":             {"envelope-loop"},
	"loop done":               {"envelope-loop", "--ok"},
	"ingest run":              {"--all"},
	"sources add":             {"filesystem", "--name", "envelope-fs", "--path", "."},
	"connectors disable":      {"gmail"},
	"teach consent enable":    {"--yes"},
	"teach consent disable":   {"--yes"},
	"usage queries on":        nil,
	"usage queries off":       nil,
	"share storage-limit":     {"1000000"},
	"config context":          {"default"},
	"config embedder":         {"static"},
	"config mmr":              {"off"},
	"config mcp-write-policy": {"open"},
}

// contractEnvelopeSeedID is replaced with the seeded memory's real id at run
// time; a placeholder keeps the table readable.
const contractEnvelopeSeedID = "<seed-id>"

// contractEnvelopeOrder forces the rows that depend on each other to run in a
// working sequence (register before begin, begin before done, consent enable
// before examples). Every other row runs after these, in registry order.
var contractEnvelopeOrder = []string{
	"loop register", "loop begin", "loop status", "loop done",
	"teach consent enable", "teach examples",
	"tasks add", "tasks list", "tasks done",
}

// contractEnvelopeNonExecutable records rows this test must not drive, with the
// reason. It mirrors contractBaselineNonExecutable: the test may run in CI and
// must not block, open a browser, mutate the host, or replace its own binary.
// These rows still get the schema-shape assertion below — the
// `contract.platform_policy` the registry prescribes.
var contractEnvelopeNonExecutable = map[string]string{
	"delete":                 "destructive memory mutation",
	"forget":                 "destructive governance mutation",
	"unforget":               "governance mutation",
	"disconnect google":      "revokes a real Google credential",
	"connect github":         "network handshake",
	"connect imessage":       "reads the host Messages database",
	"serve http install":     "mutates host service configuration",
	"serve http uninstall":   "mutates host service configuration",
	"serve http status":      "reads host service configuration",
	"share push":             "network push to a real remote",
	"share subscribe":        "network fetch from a real remote",
	"share pull":             "network fetch from a real remote",
	"share gc":               "prunes a real share store",
	"share keygen":           "writes a durable age identity",
	"share init":             "requires a real remote (--remote or --github)",
	"share preview":          "requires an initialized share",
	"share remove":           "requires an initialized share",
	"share fingerprint":      "requires a durable age identity",
	"schedule run":           "runs the real scheduled pulse job",
	"reingest":               "re-fetches every enabled source",
	"connectors enable":      "may start an interactive OAuth consent flow",
	"mcp proposals":          "needs a staged propose-mode write to exist",
	"sync google":            "network fetch",
	"sync github":            "network fetch",
	"sync imessage":          "reads the host Messages database",
	"sync applecalendar":     "reads the host Calendar database",
	"sync git":               "runs git against a real remote",
	"connect filesystem":     "walks and indexes a real directory tree",
	"brief correct":          "needs a real memory id and field to correct",
	"merge confirm":          "needs a real pending identity pair",
	"merge reject":           "needs a real pending identity pair",
	"merge undo":             "needs a real merge ledger entry",
	"teach identity confirm": "needs a real pending identity pair",
	"teach identity reject":  "needs a real pending identity pair",
	"teach identity undo":    "needs a real identity ledger entry",
	"teach commitment":       "needs a real commitment memory",
	"teach memory":           "needs a real authored memory",
	"teach undo":             "needs a real Teach ledger entry",
	"--version":              "version alias measured statically",
	"-v":                     "version alias measured statically",
	"usage off":              "would disable usage tracking for the rest of the run",
	"usage on":               "paired with usage off",
	"index rebuild":          "covered by the index rebuild receipt tests",
	"loop heartbeat":         "needs a live run id from a begin in the same process",
	"share list":             "covered directly by the share tests",
	"retention decide":       "needs a persisted retention report and candidate id",
	"retention execute":      "destructive mutation requiring a reviewed report",
	"retention recover":      "needs an encrypted recovery manifest from an execution",
}

// TestContractEveryPayloadIsVersioned is CON-01's proof: every non-exempt
// command registry row emits a payload carrying `schema` and `schema_version`,
// and the schema name equals the `payload` the registry assigns that path.
//
// Coverage is asserted, not assumed: covered + shape-only must equal the
// non-exempt row count, so a silently skipped row fails the test rather than
// shrinking the table.
func TestContractEveryPayloadIsVersioned(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	seed := run(t, "write", "--title", "envelope seed", "--text", "envelope probe seed", "--json")
	var seedDoc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(seed), &seedDoc); err != nil || seedDoc.ID == "" {
		t.Fatalf("seed write receipt: %v\n%s", err, seed)
	}

	registry := loadCLIRegistry(t)
	rows := map[string]cliRegistryRow{}
	nonExempt := 0
	for _, row := range registry.Commands {
		if row.JSONContract == "exempt" {
			// An exempt row must say why, so the exemption stays reviewable.
			if strings.TrimSpace(row.Reason) == "" {
				t.Errorf("registry row %q is exempt with no reason", row.Path)
			}
			continue
		}
		if row.Payload == "" {
			t.Errorf("registry row %q is non-exempt with no payload name", row.Path)
			continue
		}
		rows[row.Path] = row
		nonExempt++
	}

	declared := contractDeclaredSchemaNames(t)
	covered, shapeOnly := 0, 0

	assertShapeOnly := func(t *testing.T, row cliRegistryRow) {
		t.Helper()
		// A row this test must not execute still has to prove its schema name
		// exists in the tree, so the registry cannot name a payload no code
		// emits. Same go/ast style Plan 01-06's error-code sweep uses.
		if !contractSchemaNameCovered(declared, row.Payload) {
			t.Errorf("%s: registry names payload %q but no Go source emits that string", row.Path, row.Payload)
		}
	}

	drive := func(t *testing.T, row cliRegistryRow) {
		t.Helper()
		args := append(strings.Fields(row.Path), contractEnvelopeArgsFor(row.Path, seedDoc.ID)...)
		stdout, stderr, err := runSplit(t, append(args, "--json")...)
		if strings.TrimSpace(stdout) == "" {
			t.Fatalf("%s --json produced no document (err=%v)\nstderr: %s", row.Path, err, stderr)
		}
		var doc map[string]any
		if derr := json.Unmarshal([]byte(stdout), &doc); derr != nil {
			t.Fatalf("%s --json did not decode into an object: %v\n%s", row.Path, derr, stdout)
		}
		if got, _ := doc["schema"].(string); got != row.Payload {
			t.Fatalf("%s emitted schema %q, registry says %q", row.Path, got, row.Payload)
		}
		version, ok := doc["schema_version"].(float64)
		if !ok || version < 1 {
			t.Fatalf("%s emitted schema_version %v, want a positive integer", row.Path, doc["schema_version"])
		}
	}

	seen := map[string]bool{}
	runRow := func(path string) {
		row, ok := rows[path]
		if !ok || seen[path] {
			return
		}
		seen[path] = true
		if row.Platform != "all" {
			shapeOnly++
			subRun(t, path+" (platform)", func(t *testing.T) { assertShapeOnly(t, row) })
			return
		}
		if _, denied := contractEnvelopeNonExecutable[path]; denied {
			shapeOnly++
			subRun(t, path+" (shape-only)", func(t *testing.T) { assertShapeOnly(t, row) })
			return
		}
		covered++
		subRun(t, path, func(t *testing.T) { drive(t, row) })
	}

	for _, path := range contractEnvelopeOrder {
		runRow(path)
	}
	for _, row := range registry.Commands {
		runRow(row.Path)
	}

	if covered+shapeOnly != nonExempt {
		t.Fatalf("coverage gap: %d executed + %d shape-only != %d non-exempt registry rows", covered, shapeOnly, nonExempt)
	}
	if covered == 0 {
		t.Fatal("no row was executed; the table degenerated to shape-only assertions")
	}
	t.Logf("envelope coverage: %d executed, %d shape-only, %d non-exempt rows", covered, shapeOnly, nonExempt)
}

func contractEnvelopeArgsFor(path, seedID string) []string {
	args := contractEnvelopeArgs[path]
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == contractEnvelopeSeedID {
			a = seedID
		}
		out = append(out, a)
	}
	return out
}

// contractDeclaredSchemaNames collects every `mora.*` string literal in
// non-test package source, so a shape-only row can prove its schema name is
// actually spelled somewhere in the tree. Dotted prefixes are collected too,
// because several schema names are composed at run time
// ("mora.sync." + source, mergeSchemaNamespace(ctx) + ".list").
func contractDeclaredSchemaNames(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	literals := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || !strings.HasPrefix(value, "mora.") {
				return true
			}
			literals[value] = true
			return true
		})
	}
	return literals
}

// contractSchemaNameCovered reports whether a registry payload name is spelled
// in source either exactly or as a composed prefix.
func contractSchemaNameCovered(literals map[string]bool, payload string) bool {
	if literals[payload] {
		return true
	}
	for lit := range literals {
		if strings.HasSuffix(lit, ".") && strings.HasPrefix(payload, lit) {
			return true
		}
		if !strings.HasSuffix(lit, ".") && strings.HasPrefix(payload, lit+".") {
			return true
		}
	}
	return false
}

// contractDashLedPositionalPaths is the SWEEP for the bug class Plans 01-05 and
// 01-07 each rediscovered: `tasks add --json` created a task named "--json",
// `loop begin --json` started a run for a loop of that name, and
// `sources add --json` registered a live source typed and named "--json".
// Three independent discoveries of ONE defect.
//
// This table is every command path that consumes a positional argument. A
// dash-led token in that slot must be REFUSED — never stored, never used as a
// lookup key, never mutated on. Plan 01-10's phase-wide matrix should keep this
// table as the class-closed assertion rather than re-testing three instances.
var contractDashLedPositionalPaths = [][]string{
	{"read"}, {"delete"},
	{"tasks", "add"}, {"tasks", "done"},
	{"loop", "begin"}, {"loop", "register"}, {"loop", "status"}, {"loop", "done"},
	{"sources", "add"}, {"connectors", "enable"}, {"connectors", "disable"},
	{"merge", "undo"}, {"teach", "undo"}, {"teach", "identity", "undo"},
	{"share", "remove"}, {"share", "storage-limit"}, {"schedule", "run"},
	{"mcp", "proposals", "approve"}, {"mcp", "proposals", "reject"},
	{"unforget"},
}

// contractDashLedQuerySlots are the two paths deliberately NOT in the table
// above. `search` and `think` take FREE TEXT, and parseSearchArgs treats an
// unrecognized dash-led token as part of the query rather than as an error — so
// `mora search foo --limitt 5` silently searches for the literal string instead
// of reporting the typo. That is the same silent-acceptance family, but it
// mutates nothing, and changing it would break a legitimate search for a token
// that starts with a dash. Recorded in .planning deferred-items for 01-10 to
// decide rather than fixed here.
var contractDashLedQuerySlots = [][]string{{"search"}, {"think"}}

func TestContractDashLedQuerySlotsAreDocumentedExceptions(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	for _, path := range contractDashLedQuerySlots {
		name := strings.Join(path, " ")
		subRun(t, name, func(t *testing.T) {
			args := append(append([]string{}, path...), "--bogus-positional")
			if _, _, err := runSplit(t, args...); err != nil {
				t.Fatalf("%s now REFUSES a dash-led query token; move it into "+
					"contractDashLedPositionalPaths and delete this exception: %v", name, err)
			}
		})
	}
}

func TestContractDashLedPositionalsAreRefusedEverywhere(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	for _, path := range contractDashLedPositionalPaths {
		name := strings.Join(path, " ")
		subRun(t, name, func(t *testing.T) {
			// --yes is harmless where it is not a defined flag; it only matters
			// that a destructive path is not blocked by its own confirmation
			// gate BEFORE the positional is examined.
			args := append(append([]string{}, path...), "--bogus-positional", "--yes")
			stdout, _, err := runSplit(t, args...)
			if err == nil {
				t.Fatalf("%s accepted a dash-led positional instead of refusing it\nstdout: %s", name, stdout)
			}
			if strings.Contains(err.Error(), "--bogus-positional") &&
				!strings.Contains(err.Error(), "is a flag, not the") {
				// The error QUOTES the dash-led token as if it were a real
				// value, which is the signature of it having been stored or
				// used as a lookup key.
				t.Fatalf("%s treated a dash-led token as a value: %v", name, err)
			}
		})
	}
}
