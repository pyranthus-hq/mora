package mora

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/mora/exam"
)

const examV2ValidationStatus = "# obligations-v2 validation round — CLOSED: VALIDATED (one documented deviation)"

// TestExamIntegrityExit codifies the independent trust legs required to close
// Gate 1. A product score is actionable only while every leg stays green.
func TestExamIntegrityExit(t *testing.T) {
	corpora := []struct {
		name   string
		root   string
		schema int
	}{
		{name: "obligations-v1", root: examFixtureRoot, schema: exam.SchemaV1},
		{name: "obligations-v2", root: examFixtureV2Root, schema: exam.SchemaV2},
	}
	for _, corpus := range corpora {
		t.Run(corpus.name, func(t *testing.T) {
			ledger, err := exam.Load(filepath.Join(corpus.root, "ledger.json"))
			if err != nil {
				t.Fatalf("ledger trust leg broke; Gate 1 cannot close without loadable %s ground truth: %v", corpus.name, err)
			}
			if ledger.Version != corpus.schema {
				t.Fatalf("schema trust leg broke; Gate 1 cannot close with %s at schema %d, want %d", corpus.name, ledger.Version, corpus.schema)
			}
			for _, check := range []struct {
				name string
				run  func(exam.Ledger) error
			}{
				{name: "validation", run: exam.Validate},
				{name: "identity lint", run: exam.Lint},
				{name: "leakage lint", run: exam.LintLeakage},
				{name: "date-fingerprint lint", run: exam.LintDateFingerprint},
				{name: "title-fingerprint lint", run: exam.LintTitleFingerprint},
			} {
				if err := check.run(ledger); err != nil {
					t.Fatalf("%s trust leg broke; Gate 1 cannot close while %s fails its schema-%d contract: %v", check.name, corpus.name, corpus.schema, err)
				}
			}
			rendered, err := exam.Render(ledger)
			if err != nil {
				t.Fatalf("renderer trust leg broke; Gate 1 cannot close without renderable %s ground truth: %v", corpus.name, err)
			}
			if err := exam.LintCorpus(rendered); err != nil {
				t.Fatalf("corpus lint trust leg broke; Gate 1 cannot close while rendered %s bytes fail identity lint: %v", corpus.name, err)
			}
			if err := verifyExamHashes(corpus.root, ledger, rendered); err != nil {
				t.Fatalf("hash trust leg broke; Gate 1 cannot close unless %s CORPUS.sha256 matches rendered and checked-out bytes: %v", corpus.name, err)
			}
		})
	}

	if err := exam.ValidateMetricManifest(); err != nil {
		t.Fatalf("red-team manifest trust leg broke; Gate 1 cannot close unless every scorecard metric names a registered sabotage case: %v", err)
	}
	if err := determinismGuardCovers("exam_corpus_v2_test.go"); err != nil {
		t.Fatalf("determinism trust leg broke; Gate 1 cannot close while the v2 scoring adapter can escape the structural guard: %v", err)
	}
	if err := validateExamV2HumanRecord(); err != nil {
		t.Fatalf("human-validation trust leg broke; Gate 1 cannot close without the parsed VALIDATED record for obligations-v2: %v", err)
	}
	if err := validateMutationAnchors(); err != nil {
		t.Fatalf("mutation-anchor trust leg broke; Gate 1 cannot close while a dated audit can silently target dead source or test anchors: %v", err)
	}
}

func determinismGuardCovers(want string) error {
	path := filepath.Join("exam", "determinism_guard_test.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	covered := false
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && value == want {
			covered = true
		}
		return true
	})
	if !covered {
		return fmt.Errorf("%s does not list %q", path, want)
	}
	return nil
}

func validateExamV2HumanRecord() error {
	path := filepath.Join(examFixtureV2Root, "VALIDATION-2026-07-23.md")
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) == 0 || lines[0] != examV2ValidationStatus {
		return fmt.Errorf("status line = %q, want literal %q", firstLine(lines), examV2ValidationStatus)
	}
	return nil
}

func firstLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

type mutationAnchor struct {
	name        string
	file        string
	old         string
	pkg         string
	testPattern string
}

func validateMutationAnchors() error {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	scriptPath := filepath.Join(repoRoot, "scripts", "eval", "exam-mutation-matrix.sh")
	body, err := os.ReadFile(scriptPath)
	if err != nil {
		return err
	}
	anchors, err := parseMutationAnchors(body)
	if err != nil {
		return err
	}
	if len(anchors) == 0 {
		return fmt.Errorf("%s contains no kill_mutant anchors", scriptPath)
	}
	testNamePattern := regexp.MustCompile(`Test[A-Za-z0-9_]+`)
	for _, anchor := range anchors {
		target := filepath.Join(repoRoot, filepath.FromSlash(anchor.file))
		source, err := os.ReadFile(target)
		if err != nil {
			return fmt.Errorf("%s source target %s: %w", anchor.name, anchor.file, err)
		}
		if count := bytes.Count(source, []byte(anchor.old)); count != 1 {
			return fmt.Errorf("%s source anchor occurs %d times in %s, want exactly 1", anchor.name, count, anchor.file)
		}
		testName := testNamePattern.FindString(anchor.testPattern)
		if testName == "" {
			return fmt.Errorf("%s test pattern %q names no Go test", anchor.name, anchor.testPattern)
		}
		testDir := filepath.Join(repoRoot, strings.TrimPrefix(anchor.pkg, "./"))
		found, err := packageHasTest(testDir, testName)
		if err != nil {
			return fmt.Errorf("%s test target %s: %w", anchor.name, anchor.pkg, err)
		}
		if !found {
			return fmt.Errorf("%s test anchor %q does not occur in %s", anchor.name, testName, anchor.pkg)
		}
	}
	return nil
}

func parseMutationAnchors(script []byte) ([]mutationAnchor, error) {
	var anchors []mutationAnchor
	for _, block := range strings.Split(string(script), "\n\n") {
		block = strings.TrimSpace(block)
		if !strings.HasPrefix(block, "kill_mutant ") {
			continue
		}
		command := "kill_mutant() { printf '%s\\0' \"$@\"; }\n" + block
		out, err := exec.Command("bash", "-c", command).Output()
		if err != nil {
			return nil, fmt.Errorf("parse mutation command %q: %w", firstLine(strings.Split(block, "\n")), err)
		}
		parts := bytes.Split(out, []byte{0})
		if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
			parts = parts[:len(parts)-1]
		}
		if len(parts) != 6 {
			return nil, fmt.Errorf("mutation command %q has %d arguments, want 6", firstLine(strings.Split(block, "\n")), len(parts))
		}
		anchors = append(anchors, mutationAnchor{
			name:        string(parts[0]),
			file:        string(parts[1]),
			old:         string(parts[2]),
			pkg:         string(parts[4]),
			testPattern: string(parts[5]),
		})
	}
	return anchors, nil
}

func packageHasTest(dir, want string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return false, err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name.Name == want {
				return true, nil
			}
		}
	}
	return false, nil
}
