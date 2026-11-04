package rules

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The hook loads this package before every Bash call, so it must stay off
// SQLite, the index and MCP. The only internal import allowed is atomicio.
func TestImportsStayLight(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(path, ".") && path != "github.com/pyranthus-hq/mora/internal/atomicio" {
				t.Errorf("%s imports %s", f, path)
			}
		}
	}
}
