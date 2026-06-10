package imessage

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestReadOnlyDSN asserts the chat.db DSN is read-only and WAL-safe: it MUST
// contain mode=ro and busy_timeout, and MUST NEVER contain immutable=1 (which
// would ignore the WAL and produce stale/torn reads — IMSG-09).
func TestReadOnlyDSN(t *testing.T) {
	dsn := chatDBDSN("/Users/neil/Library/Messages/chat.db")
	if !strings.Contains(dsn, "mode=ro") {
		t.Errorf("DSN %q missing mode=ro", dsn)
	}
	if !strings.Contains(dsn, "busy_timeout") {
		t.Errorf("DSN %q missing busy_timeout", dsn)
	}
	if strings.Contains(dsn, "immutable") {
		t.Errorf("DSN %q must NEVER contain immutable (IMSG-09)", dsn)
	}
}

// TestNoNetwork is an architectural guard: the connector reads only the local
// chat.db and makes ZERO network calls (IMSG-01, T-02-01). No non-test source file
// in the package may import a net package. It also enforces the hard no-import
// rules: never internal/mora, never internal/google.
func TestNoNetwork(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	// Exact-match a net package (net, net/http, net/url, ...) or a forbidden
	// internal connector/mora package by substring.
	exactNet := map[string]bool{"net": true, "net/http": true, "net/url": true, "net/textproto": true}
	forbiddenSub := []string{"internal/mora", "internal/google"}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if exactNet[path] || strings.HasPrefix(path, "net/") {
				t.Errorf("%s imports forbidden net package %q (IMSG-01: zero network)", name, path)
			}
			for _, bad := range forbiddenSub {
				if strings.Contains(path, bad) {
					t.Errorf("%s imports forbidden package %q (no-import hard rule)", name, path)
				}
			}
		}
	}
}
