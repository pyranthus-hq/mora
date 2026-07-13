package imessage

import (
	"database/sql"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
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

// TestProbeReadable asserts that the FDA probe correctly identifies a readable
// database, a missing file, and a corrupt/garbage file.
func TestProbeReadable(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Valid database
	validDBPath := filepath.Join(tmpDir, "valid.db")
	db, err := sql.Open("sqlite", "file:"+validDBPath+"?mode=rwc")
	if err != nil {
		t.Fatalf("failed to create valid db: %v", err)
	}
	// Needs to have sqlite_master populated or at least be a valid db.
	// Creating a table ensures it's a valid sqlite db file.
	if _, err := db.Exec("CREATE TABLE test (id INTEGER)"); err != nil {
		t.Fatalf("failed to init valid db: %v", err)
	}
	db.Close()

	readable, err := ProbeReadable(validDBPath)
	if err != nil {
		t.Errorf("ProbeReadable(valid) returned error: %v", err)
	}
	if !readable {
		t.Errorf("ProbeReadable(valid) = false, want true")
	}

	// 2. Missing file
	missingPath := filepath.Join(tmpDir, "missing.db")
	readable, err = ProbeReadable(missingPath)
	if err == nil {
		t.Errorf("ProbeReadable(missing) expected error, got nil")
	}
	if readable {
		t.Errorf("ProbeReadable(missing) = true, want false")
	}

	// 3. Corrupt / Garbage file
	garbagePath := filepath.Join(tmpDir, "garbage.db")
	if err := os.WriteFile(garbagePath, []byte("not a sqlite database"), 0644); err != nil {
		t.Fatalf("failed to create garbage file: %v", err)
	}
	readable, err = ProbeReadable(garbagePath)
	if err == nil {
		t.Errorf("ProbeReadable(garbage) expected error, got nil")
	}
	if readable {
		t.Errorf("ProbeReadable(garbage) = true, want false")
	}

	// 4. Unreadable permissions (simulating FDA denial)
	// Skip on Windows because os.Chmod(0000) does not reliably prevent file reading.
	if runtime.GOOS != "windows" {
		unreadablePath := filepath.Join(tmpDir, "unreadable.db")
		// Make it a valid DB first
		dbUnreadable, err := sql.Open("sqlite", "file:"+unreadablePath+"?mode=rwc")
		if err != nil {
			t.Fatalf("failed to create unreadable db: %v", err)
		}
		if _, err := dbUnreadable.Exec("CREATE TABLE test2 (id INTEGER)"); err != nil {
			t.Fatalf("failed to init unreadable db: %v", err)
		}
		dbUnreadable.Close()

		// Strip read permissions
		if err := os.Chmod(unreadablePath, 0000); err != nil {
			t.Fatalf("failed to chmod unreadable db: %v", err)
		}

		readable, err = ProbeReadable(unreadablePath)
		if err == nil {
			t.Errorf("ProbeReadable(unreadable) expected error, got nil")
		}
		if readable {
			t.Errorf("ProbeReadable(unreadable) = true, want false")
		}
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
