package corrections

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This package sees raw commands, so it must stay off the network, SQLite and
// MCP.
func TestImportsNoNetwork(t *testing.T) {
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
			if path == "net" || strings.HasPrefix(path, "net/") || path == "database/sql" ||
				strings.Contains(path, "internal/mcp") || strings.Contains(path, "sqlite") {
				t.Errorf("%s imports %s", f, path)
			}
		}
	}
}
