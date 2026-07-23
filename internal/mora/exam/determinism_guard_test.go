package exam

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The scoring paths must be STRUCTURALLY free of the clock and the PRNG, not free of
// them by convention. A grep is a habit; this is a gate. It is what lets the
// invariance rows promise a byte-stable scorecard and a byte-stable report across
// runs, across machines, and across the wall clock.
//
// The inverse rule matters just as much: rapid's whole job is generating values, so
// the moment someone imports it into score.go the ban becomes a fiction. It may
// appear only in a *_prop_test.go file.
var (
	bannedImports  = []string{"math/rand", "math/rand/v2"}
	bannedSelector = map[string][]string{"time": {"Now", "Since", "Until"}}
	propertyOnly   = "pgregory.net/rapid"
)

// guardedFiles is every file that can reach a score, a baseline, a mutant, an
// adapter or a gate. The package-mora exam files are named explicitly because that
// is where the adapters live, and an adapter that read the wall clock would make the
// report drift for reasons that have nothing to do with the product.
func guardedFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			out = append(out, entry.Name())
		}
	}
	for _, name := range []string{
		"exam_corpus_test.go",
		"exam_corpus_v2_test.go",
		"exam_integrity_exit_test.go",
		"exam_score_test.go",
		"exam_surfaces_test.go",
		"exam_gate_test.go",
		"exam_flywheel_test.go",
		"exam_roundtrip_prop_test.go",
	} {
		path := filepath.Join("..", name)
		if _, err := os.Stat(path); err == nil {
			out = append(out, path)
		}
	}
	return out
}

func TestExamDeterminismGuard(t *testing.T) {
	files := guardedFiles(t)
	if len(files) == 0 {
		t.Fatal("the determinism guard is guarding nothing")
	}
	scored := 0
	for _, path := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scored++
		aliases := map[string]string{}
		for _, spec := range file.Imports {
			route, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			for _, banned := range bannedImports {
				if route == banned {
					t.Errorf("%s imports %q: a scoring path may not carry a PRNG", path, banned)
				}
			}
			if route == propertyOnly && !strings.HasSuffix(filepath.Base(path), "_prop_test.go") {
				t.Errorf("%s imports %q, which may appear ONLY in a *_prop_test.go file — generation stays outside the scorer", path, propertyOnly)
			}
			name := route[strings.LastIndex(route, "/")+1:]
			if spec.Name != nil {
				name = spec.Name.Name
			}
			aliases[name] = route
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			route := aliases[pkg.Name]
			for _, banned := range bannedSelector[route] {
				if sel.Sel.Name == banned {
					t.Errorf("%s calls %s.%s at %s: a scoring path may not read the clock (time.Parse and formatting are fine)",
						path, pkg.Name, banned, fset.Position(sel.Pos()))
				}
			}
			return true
		})
	}
	// The guard must not silently guard an empty set — that is how a guard rots.
	if scored < len(files) {
		t.Fatalf("guard parsed %d of %d files", scored, len(files))
	}
	for _, must := range []string{"score.go", "baseline.go", "mutate.go"} {
		found := false
		for _, path := range files {
			found = found || filepath.Base(path) == must
		}
		if !found {
			t.Errorf("the determinism guard is not covering %s", must)
		}
	}
}
