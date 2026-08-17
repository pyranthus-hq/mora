package mora

import (
	"bytes"
	"context"
	"encoding/json"
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
	"time"
)

type cliRegistry struct {
	SchemaVersion int `json:"schema_version"`
	Issue         int `json:"issue"`
	Contract      struct {
		RealDispatchTest     string `json:"real_dispatch_test"`
		DriftAndMutationTest string `json:"drift_and_mutation_test"`
		Mutation             string `json:"mutation"`
		PlatformPolicy       string `json:"platform_policy"`
		JSONContractPolicy   string `json:"json_contract_policy"`
	} `json:"contract"`
	Commands []cliRegistryRow `json:"commands"`
}

type cliRegistryRow struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	Platform     string `json:"platform"`
	JSONContract string `json:"json_contract,omitempty"`
	Payload      string `json:"payload,omitempty"`
}

type cliBehaviorEvidence struct {
	SchemaVersion int                       `json:"schema_version"`
	Issue         int                       `json:"issue"`
	Dimensions    []string                  `json:"dimensions"`
	Defaults      map[string]cliEvidenceRef `json:"defaults"`
	Groups        []cliEvidenceGroup        `json:"groups"`
}

type cliEvidenceGroup struct {
	Name     string                    `json:"name"`
	Paths    []string                  `json:"paths"`
	Evidence map[string]cliEvidenceRef `json:"evidence"`
}

type cliEvidenceRef struct {
	Status           string   `json:"status"`
	Tests            []string `json:"tests,omitempty"`
	Reason           string   `json:"reason,omitempty"`
	ProductionAnchor string   `json:"production_anchor,omitempty"`
}

func loadCLIRegistry(t *testing.T) cliRegistry {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("eval", "cli-command-registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry cliRegistry
	if err := json.Unmarshal(body, &registry); err != nil {
		t.Fatalf("parse CLI command registry: %v", err)
	}
	return registry
}

func loadCLIBehaviorEvidence(t *testing.T) cliBehaviorEvidence {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("eval", "cli-command-evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var evidence cliBehaviorEvidence
	if err := json.Unmarshal(body, &evidence); err != nil {
		t.Fatalf("parse CLI behavior evidence: %v", err)
	}
	return evidence
}

// TestCLIRegistryMatchesProductionDispatch is both the drift gate and the
// dispatch-site mutation witness for #205. Removing, renaming, or adding an
// exact production case token makes this test red until the machine-readable
// registry and its real-Run probe row move with it.
func TestCLIRegistryMatchesProductionDispatch(t *testing.T) {
	registry := loadCLIRegistry(t)
	if registry.SchemaVersion != 2 || registry.Issue != 205 {
		t.Fatalf("registry header = schema %d issue %d, want schema 2 issue 205", registry.SchemaVersion, registry.Issue)
	}
	if registry.Contract.RealDispatchTest == "" ||
		registry.Contract.DriftAndMutationTest == "" ||
		registry.Contract.Mutation == "" ||
		registry.Contract.PlatformPolicy == "" ||
		registry.Contract.JSONContractPolicy == "" {
		t.Fatal("registry evidence contract must name real dispatch, mutation, and platform policy")
	}

	seen := map[string]bool{}
	for _, row := range registry.Commands {
		if strings.TrimSpace(row.Path) != row.Path || row.Path == "" {
			t.Fatalf("non-canonical registry path %q", row.Path)
		}
		if seen[row.Path] {
			t.Fatalf("duplicate registry path %q", row.Path)
		}
		seen[row.Path] = true
		if row.Platform == "" {
			t.Fatalf("%s: missing platform evidence policy", row.Path)
		}
		switch row.Kind {
		case "verb", "alias", "subcommand":
		default:
			t.Fatalf("%s: unknown registry kind %q", row.Path, row.Kind)
		}
		if row.JSONContract != "" {
			switch row.JSONContract {
			case "result", "receipt", "exempt":
			default:
				t.Fatalf("%s: unknown json contract %q", row.Path, row.JSONContract)
			}
		}
	}

	funcs := parseProductionFunctions(t)
	assertDispatchSet(t, registry, funcs, "", "Run", "cmd")
	for _, family := range []struct {
		prefix        string
		function      string
		discriminants []string
	}{
		{"brief", "cmdBrief", []string{"args[0]"}},
		{"config", "cmdConfig", []string{"key"}},
		{"index", "cmdIndex", []string{"fs.Arg(0)"}},
		{"forget", "cmdForget", []string{"args[0]"}},
		{"merge", "cmdMerge", []string{"args[0]"}},
		{"teach", "cmdTeach", []string{"args[0]"}},
		{"tasks", "cmdTasks", []string{"args[0]"}},
		{"schedule", "cmdSchedule", []string{"args[0]"}},
		{"sources", "cmdSources", []string{"args[0]"}},
		{"connectors", "cmdConnectors", []string{"args[0]"}},
		{"ingest", "cmdIngest", []string{"args[0]"}},
		{"connect", "cmdConnect", []string{"args[0]"}},
		{"sync", "cmdSync", []string{"args[0]"}},
		{"share", "cmdShare", []string{"args[0]"}},
		{"usage", "cmdUsage", []string{"args[0]"}},
		{"disconnect", "cmdDisconnect", []string{"args[0]"}},
		{"mcp", "cmdMCP", []string{"args[0]"}},
		{"serve", "cmdServe", []string{"args[0]"}},
		{"hook", "cmdHook", []string{"args[0]"}},
		{"loop", "cmdLoop", []string{"sub"}},
	} {
		assertDispatchSet(t, registry, funcs, family.prefix, family.function, family.discriminants...)
	}
	for _, family := range []struct {
		prefix        string
		function      string
		discriminants []string
	}{
		{"teach identity", "cmdMerge", []string{"args[0]"}},
		{"teach consent", "teachConsent", []string{"args[0]"}},
		{"usage queries", "cmdUsage", []string{"args[1]"}},
		{"serve http", "cmdServe", []string{"rest[0]"}},
	} {
		assertNestedDispatchSet(t, registry, funcs, family.prefix, family.function, family.discriminants...)
	}
}

// TestCLIRegistryBehaviorEvidence makes the row-by-row behavior mapping
// load-bearing. Every production command path must resolve every acceptance
// dimension to a named, existing regression witness or an explicit N/A/platform
// rationale. Missing paths, duplicate paths, stale test names, and silent holes
// fail CI.
func TestCLIRegistryBehaviorEvidence(t *testing.T) {
	registry := loadCLIRegistry(t)
	evidence := loadCLIBehaviorEvidence(t)
	if evidence.SchemaVersion != 1 || evidence.Issue != 205 {
		t.Fatalf("evidence header = schema %d issue %d, want schema 1 issue 205", evidence.SchemaVersion, evidence.Issue)
	}
	requiredDimensions := []string{"success", "usage", "invalid", "json", "pipe", "state", "error", "refusal", "mutation"}
	if diff := stringSetDiff(requiredDimensions, evidence.Dimensions); diff != "" {
		t.Fatalf("behavior dimensions drift%s", diff)
	}

	tests := parseTestFunctions(t)
	registryPaths := map[string]bool{}
	for _, row := range registry.Commands {
		registryPaths[row.Path] = true
	}
	evidencePaths := map[string]bool{}
	for _, group := range evidence.Groups {
		if strings.TrimSpace(group.Name) == "" {
			t.Fatal("evidence group has no name")
		}
		if len(group.Paths) == 0 {
			t.Fatalf("evidence group %q has no paths", group.Name)
		}
		for _, path := range group.Paths {
			if evidencePaths[path] {
				t.Fatalf("duplicate behavior evidence path %q", path)
			}
			evidencePaths[path] = true
			for _, dimension := range requiredDimensions {
				ref, ok := group.Evidence[dimension]
				if !ok {
					ref, ok = evidence.Defaults[dimension]
				}
				if !ok {
					t.Fatalf("%s: missing %s evidence", path, dimension)
				}
				validateCLIEvidenceRef(t, tests, path, dimension, ref)
			}
		}
	}
	if diff := setDiff(registryPaths, evidencePaths); diff != "" {
		t.Fatalf("behavior evidence path drift%s", diff)
	}
}

func parseTestFunctions(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	tests := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && strings.HasPrefix(fn.Name.Name, "Test") {
				tests[fn.Name.Name] = true
			}
		}
	}
	return tests
}

func validateCLIEvidenceRef(t *testing.T, tests map[string]bool, path, dimension string, ref cliEvidenceRef) {
	t.Helper()
	switch ref.Status {
	case "test":
		if len(ref.Tests) == 0 {
			t.Fatalf("%s %s: test evidence has no named tests", path, dimension)
		}
	case "platform_seam":
		if len(ref.Tests) == 0 || strings.TrimSpace(ref.Reason) == "" {
			t.Fatalf("%s %s: platform seam needs tests and a reason", path, dimension)
		}
	case "not_supported", "not_applicable":
		if len(ref.Tests) != 0 || strings.TrimSpace(ref.Reason) == "" {
			t.Fatalf("%s %s: %s needs a reason and no tests", path, dimension, ref.Status)
		}
	default:
		t.Fatalf("%s %s: unknown evidence status %q", path, dimension, ref.Status)
	}
	if dimension == "mutation" && strings.TrimSpace(ref.ProductionAnchor) == "" {
		t.Fatalf("%s mutation: missing production anchor", path)
	}
	for _, witness := range ref.Tests {
		root := strings.SplitN(witness, "/", 2)[0]
		if !tests[root] {
			t.Fatalf("%s %s: named witness %q does not exist", path, dimension, witness)
		}
	}
}

func stringSetDiff(wantValues, gotValues []string) string {
	want, got := map[string]bool{}, map[string]bool{}
	for _, value := range wantValues {
		want[value] = true
	}
	for _, value := range gotValues {
		got[value] = true
	}
	return setDiff(want, got)
}

func parseProductionFunctions(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				funcs[fn.Name.Name] = fn
			}
		}
	}
	return funcs
}

func assertDispatchSet(t *testing.T, registry cliRegistry, funcs map[string]*ast.FuncDecl, prefix, function string, discriminants ...string) {
	t.Helper()
	fn := funcs[function]
	if fn == nil {
		t.Fatalf("production dispatcher %s not found", function)
	}
	want := map[string]bool{}
	for _, row := range registry.Commands {
		fields := strings.Fields(row.Path)
		if prefix == "" {
			if len(fields) == 1 {
				want[fields[0]] = true
			}
			continue
		}
		if len(fields) == 2 && fields[0] == prefix {
			want[fields[1]] = true
		}
	}
	got := dispatchTokens(fn, discriminants)
	if diff := setDiff(want, got); diff != "" {
		t.Fatalf("%s registry drift%s", function, diff)
	}
}

func assertNestedDispatchSet(t *testing.T, registry cliRegistry, funcs map[string]*ast.FuncDecl, prefix, function string, discriminants ...string) {
	t.Helper()
	fn := funcs[function]
	if fn == nil {
		t.Fatalf("production dispatcher %s not found", function)
	}
	prefixFields := strings.Fields(prefix)
	want := map[string]bool{}
	for _, row := range registry.Commands {
		fields := strings.Fields(row.Path)
		if len(fields) != len(prefixFields)+1 {
			continue
		}
		if strings.Join(fields[:len(prefixFields)], " ") == prefix {
			want[fields[len(fields)-1]] = true
		}
	}
	got := dispatchTokens(fn, discriminants)
	if diff := setDiff(want, got); diff != "" {
		t.Fatalf("%s registry drift for %s%s", function, prefix, diff)
	}
}

func dispatchTokens(fn *ast.FuncDecl, discriminants []string) map[string]bool {
	allowed := map[string]bool{}
	for _, d := range discriminants {
		allowed[d] = true
	}
	out := map[string]bool{}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SwitchStmt:
			if !allowed[dispatchExprKey(n.Tag)] {
				return true
			}
			for _, stmt := range n.Body.List {
				clause := stmt.(*ast.CaseClause)
				for _, expr := range clause.List {
					if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if value, err := strconv.Unquote(lit.Value); err == nil {
							out[value] = true
						}
					}
				}
			}
		case *ast.BinaryExpr:
			if n.Op != token.EQL && n.Op != token.NEQ {
				return true
			}
			for _, pair := range [][2]ast.Expr{{n.X, n.Y}, {n.Y, n.X}} {
				if !allowed[dispatchExprKey(pair[0])] {
					continue
				}
				if lit, ok := pair[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if value, err := strconv.Unquote(lit.Value); err == nil {
						out[value] = true
					}
				}
			}
		}
		return true
	})
	return out
}

func dispatchExprKey(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			if lit, ok := e.Index.(*ast.BasicLit); ok {
				return id.Name + "[" + lit.Value + "]"
			}
		}
	case *ast.CallExpr:
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && len(e.Args) == 1 {
			if id, ok := sel.X.(*ast.Ident); ok {
				if lit, ok := e.Args[0].(*ast.BasicLit); ok {
					return id.Name + "." + sel.Sel.Name + "(" + lit.Value + ")"
				}
			}
		}
	}
	return ""
}

func setDiff(want, got map[string]bool) string {
	var missing, extra []string
	for value := range want {
		if !got[value] {
			missing = append(missing, value)
		}
	}
	for value := range got {
		if !want[value] {
			extra = append(extra, value)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	return fmt.Sprintf("\nmissing from production: %v\nmissing from registry: %v", missing, extra)
}

// TestCLIRegistryRealRunDispatch drives every registry row through Run. The
// deliberately invalid final flag keeps connector/network/native rows at their
// parser or refusal boundary while still executing the real production route.
func TestCLIRegistryRealRunDispatch(t *testing.T) {
	registry := loadCLIRegistry(t)
	for _, row := range registry.Commands {
		row := row
		t.Run(strings.ReplaceAll(row.Path, " ", "/"), func(t *testing.T) {
			if strings.HasPrefix(row.Path, "serve http ") {
				t.Setenv("MORA_PORT", "65534")
				stubGOOS(t, "linux")
				stubPortFree(t, true)
				stubScheduleRunner(t)
			}

			fields := strings.Fields(row.Path)
			probeTail := []string{"--__issue205_probe__"}
			switch row.Path {
			case "index rebuild":
				probeTail = []string{"--force"}
			case "share keygen", "share fingerprint", "mcp serve":
				probeTail = nil
			}
			currentArgs := append(append([]string(nil), fields...), probeTail...)
			unknownArgs := append([]string(nil), fields...)
			unknownArgs[len(unknownArgs)-1] = "__issue205_unknown__"
			unknownArgs = append(unknownArgs, probeTail...)
			normalizers := append(append([]string(nil), fields...), "__issue205_unknown__")

			current := normalizeCLIProbe(runCLIRegistryProbe(t, currentArgs), normalizers...)
			unknown := normalizeCLIProbe(runCLIRegistryProbe(t, unknownArgs), normalizers...)
			if want := map[string]string{
				"serve http install":   "wrote systemd user unit",
				"serve http uninstall": "removed ",
				"serve http status":    "service installed:",
			}[row.Path]; want != "" && !strings.Contains(current.Stdout, want) {
				t.Fatalf("registered token did not reach its production behavior: stdout %q does not contain %q", current.Stdout, want)
			}
			if current == unknown {
				t.Fatalf("registered token is behaviorally indistinguishable from an unknown token:\ncurrent=%+v\nunknown=%+v", current, unknown)
			}
			if row.Kind == "alias" && current.HasError {
				t.Fatalf("documented alias failed: %s", current.Error)
			}
		})
	}
}

type cliProbeSignature struct {
	HasError bool
	Error    string
	Stdout   string
	Stderr   string
}

func runCLIRegistryProbe(t *testing.T, args []string) cliProbeSignature {
	t.Helper()
	root := t.TempDir()
	setTestHome(t, root)
	t.Setenv("MORA_CONFIG_DIR", filepath.Join(root, "config"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	err := Run(ctx, args, &stdout, &stderr, strings.NewReader(""))
	for name, body := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String()} {
		if strings.Contains(body, "\x1b[") {
			t.Fatalf("%s leaked ANSI on a pipe: %q", name, body)
		}
	}
	sig := cliProbeSignature{
		HasError: err != nil,
		Stdout:   strings.ReplaceAll(stdout.String(), root, "<ROOT>"),
		Stderr:   strings.ReplaceAll(stderr.String(), root, "<ROOT>"),
	}
	if err != nil {
		sig.Error = strings.ReplaceAll(err.Error(), root, "<ROOT>")
	}
	return sig
}

func normalizeCLIProbe(sig cliProbeSignature, tokens ...string) cliProbeSignature {
	normalize := func(value string) string {
		for _, token := range tokens {
			value = strings.ReplaceAll(value, strconv.Quote(token), `"<TOKEN>"`)
			value = strings.ReplaceAll(value, "`"+token+"`", "`<TOKEN>`")
		}
		return value
	}
	sig.Error = normalize(sig.Error)
	sig.Stdout = normalize(sig.Stdout)
	sig.Stderr = normalize(sig.Stderr)
	return sig
}

func TestCLIRegistryPriorityGapsUseRealRun(t *testing.T) {
	t.Run("serve", func(t *testing.T) {
		root := t.TempDir()
		setTestHome(t, root)
		t.Setenv("MORA_CONFIG_DIR", filepath.Join(root, "config"))
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"serve"}, &out, &out, strings.NewReader("")); err == nil ||
			!strings.Contains(err.Error(), "mora serve http") {
			t.Fatalf("Run(serve) = %v, want documented usage", err)
		}
		out.Reset()
		if err := Run(context.Background(), []string{"serve", "http", "--port", "0"}, &out, &out, strings.NewReader("")); err == nil ||
			!strings.Contains(err.Error(), "invalid --port") {
			t.Fatalf("Run(serve http --port 0) = %v, want port refusal", err)
		}
	})

	t.Run("upgrade", func(t *testing.T) {
		oldVersion := BuildVersion
		t.Cleanup(func() { BuildVersion = oldVersion })
		BuildVersion = "dev"
		var out bytes.Buffer
		err := Run(context.Background(), []string{"upgrade", "--check"}, &out, &out, strings.NewReader(""))
		if err == nil || !strings.Contains(err.Error(), "source build") {
			t.Fatalf("Run(upgrade --check) = %v, want source-build refusal", err)
		}
	})

	t.Run("connect routes", func(t *testing.T) {
		root := t.TempDir()
		setTestHome(t, root)
		t.Setenv("MORA_CONFIG_DIR", filepath.Join(root, "config"))
		for _, args := range [][]string{
			{"connect", "google", "--__issue205_probe__"},
			{"connect", "github", "--repo", "invalid"},
			{"connect", "imessage", "--__issue205_probe__"},
			{"connect", "filesystem"},
		} {
			var out bytes.Buffer
			err := Run(context.Background(), args, &out, &out, strings.NewReader(""))
			if err == nil {
				t.Fatalf("Run(%v) unexpectedly succeeded", args)
			}
			if strings.Contains(err.Error(), "usage: mora connect google [--since-days") && args[1] != "google" {
				t.Fatalf("Run(%v) fell through to the google/default arm: %v", args, err)
			}
		}
	})

	t.Run("loop json", func(t *testing.T) {
		root := t.TempDir()
		setTestHome(t, root)
		t.Setenv("MORA_CONFIG_DIR", filepath.Join(root, "config"))
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"loop", "list", "--json"}, &out, &out, strings.NewReader("")); err != nil {
			t.Fatal(err)
		}
		var payload any
		if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
			t.Fatalf("loop list --json was not byte-clean JSON: %v\n%s", err, out.String())
		}
	})

	t.Run("unforget refusal", func(t *testing.T) {
		var out bytes.Buffer
		err := Run(context.Background(), []string{"unforget", "gov_example"}, &out, &out, strings.NewReader(""))
		if err == nil || !strings.Contains(err.Error(), "without --yes") {
			t.Fatalf("Run(unforget) = %v, want confirmation refusal", err)
		}
	})
}

func TestCLIRegistryJSONSurfacesAreByteClean(t *testing.T) {
	for _, args := range [][]string{
		{"config", "--json"},
		{"connectors", "list", "--json"},
		{"loop", "list", "--json"},
	} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			root := t.TempDir()
			setTestHome(t, root)
			t.Setenv("MORA_CONFIG_DIR", filepath.Join(root, "config"))
			var out bytes.Buffer
			if err := Run(context.Background(), args, &out, &out, strings.NewReader("")); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(out.Bytes(), []byte("\x1b[")) {
				t.Fatalf("ANSI leaked into JSON: %q", out.String())
			}
			var payload any
			if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
				t.Fatalf("invalid JSON: %v\n%s", err, out.String())
			}
		})
	}
}

func TestCLIRegistryHookIOThroughRun(t *testing.T) {
	t.Run("session start", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		restore := stubHookBrief(t, "today's local brief")
		defer restore()
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"hook", "session-start"}, &out, &out,
			strings.NewReader(`{"hook_event_name":"SessionStart"}`)); err != nil {
			t.Fatal(err)
		}
		got := decodeHookOutput(t, out.String())
		if got.HookSpecificOutput.AdditionalContext != "today's local brief" {
			t.Fatalf("session-start context = %q", got.HookSpecificOutput.AdditionalContext)
		}
	})

	t.Run("recall", func(t *testing.T) {
		cfg := seedRecallMemories(t, 1)
		if _, err := rebuildIndex(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		input := `{"prompt":"What did we decide about eelpout recall token alpha?"}`
		if err := Run(context.Background(), []string{"hook", "recall"}, &out, &out, strings.NewReader(input)); err != nil {
			t.Fatal(err)
		}
		got := decodeHookOutput(t, out.String())
		if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "eelpout recall token alpha") {
			t.Fatalf("recall omitted seeded memory: %q", got.HookSpecificOutput.AdditionalContext)
		}
	})
}

func TestCLIRegistryGraphThroughRun(t *testing.T) {
	grSeedGraphVault(t)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"graph", "--json", "--top", "2"}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	var payload any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("graph --json was not byte-clean JSON: %v\n%s", err, out.String())
	}
}

func TestCLIRegistryLoopLifecycleThroughRun(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	originalClock := loopClock
	t.Cleanup(func() { loopClock = originalClock })
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	loopClock = func() time.Time { return now }

	run(t, "loop", "register", "issue-205", "--cadence", "daily", "--command", "mora pulse")
	var listOut bytes.Buffer
	if err := Run(context.Background(), []string{"loop", "list", "--json"}, &listOut, &listOut, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	var registrations []map[string]any
	if err := json.Unmarshal(listOut.Bytes(), &registrations); err != nil || len(registrations) != 1 {
		t.Fatalf("loop list JSON = %q, decode=%v", listOut.String(), err)
	}

	var beginOut bytes.Buffer
	if err := Run(context.Background(), []string{"loop", "begin", "issue-205", "--json"}, &beginOut, &beginOut, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	var begun map[string]any
	if err := json.Unmarshal(beginOut.Bytes(), &begun); err != nil {
		t.Fatalf("loop begin JSON: %v\n%s", err, beginOut.String())
	}
	runID, _ := begun["run_id"].(string)
	if runID == "" {
		t.Fatalf("loop begin omitted run_id: %s", beginOut.String())
	}

	now = now.Add(time.Minute)
	run(t, "loop", "heartbeat", "issue-205", "--run", runID, "--json")
	var statusOut bytes.Buffer
	if err := Run(context.Background(), []string{"loop", "status", "issue-205", "--json"}, &statusOut, &statusOut, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	var status loopHealth
	if err := json.Unmarshal(statusOut.Bytes(), &status); err != nil || status.State != "running" {
		t.Fatalf("loop status = %q, decode=%v state=%q", statusOut.String(), err, status.State)
	}
	run(t, "loop", "done", "issue-205", "--run", runID, "--ok")
}
