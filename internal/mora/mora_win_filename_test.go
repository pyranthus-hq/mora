package mora

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A memory ID carrying characters that are illegal in a Windows path component
// but legal on Linux must still write, be found by id, and update in place — on
// every OS. This is the keystone that unblocks a Windows release (#56): before
// the fix, atomicWrite → os.CreateTemp failed on Windows with "The filename,
// directory name, or volume label syntax is incorrect."
func TestWriteFindMemoryWithReservedCharsID(t *testing.T) {
	cfg := Config{VaultDir: t.TempDir()}
	// No '/' or ':' so the id stays a single filename component (those map to
	// scope directories in memoryPath); the rest are Windows-reserved yet legal
	// on Linux, so this same case exercises both the sanitize and identity paths.
	id := `note ? * " < > |`
	m := Memory{ID: id, Scope: "global", Type: "note", Title: "Reserved", Source: "manual", CreatedAt: "2026-07-01T00:00:00Z", Text: "first body"}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatalf("writeMemory: %v", err)
	}

	got, err := findMemory(cfg, id)
	if err != nil {
		t.Fatalf("findMemory: %v", err)
	}
	if got.ID != id || got.Text != "first body" {
		t.Fatalf("round-trip mismatch: id=%q text=%q", got.ID, got.Text)
	}

	base := filepath.Base(got.Path)
	if runtime.GOOS == "windows" {
		if strings.ContainsAny(base, `<>:"/\|?*`) {
			t.Fatalf("windows on-disk filename still holds reserved chars: %q", base)
		}
	} else if base != id+".md" {
		t.Fatalf("off-Windows filename must equal the raw id: got %q want %q", base, id+".md")
	}

	// Updating the same id must overwrite the same file, never mint a duplicate.
	m.Text = "second body"
	if err := writeMemory(cfg, m); err != nil {
		t.Fatalf("writeMemory (update): %v", err)
	}
	files, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatalf("allMemoryFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected exactly 1 memory file after update, got %d: %v", len(files), files)
	}
	got2, err := findMemory(cfg, id)
	if err != nil {
		t.Fatalf("findMemory after update: %v", err)
	}
	if got2.Text != "second body" {
		t.Fatalf("update did not take, text=%q", got2.Text)
	}
}

func TestOSSafeBaseDeterministicAndOSGated(t *testing.T) {
	id := `x?y*z`
	// Compare two separate calls via vars: repeated calls on the same id must
	// agree (on Windows the sanitized name carries a deterministic hash suffix).
	// Assigning to vars also keeps staticcheck from flagging f(x) != f(x) (SA4000).
	if first, second := osSafeBase(id), osSafeBase(id); first != second {
		t.Fatalf("osSafeBase must be deterministic: %q vs %q", first, second)
	}
	if runtime.GOOS != "windows" && osSafeBase(id) != id {
		t.Fatalf("osSafeBase must be the identity off Windows, got %q", osSafeBase(id))
	}
	if runtime.GOOS == "windows" && strings.ContainsAny(osSafeBase(id), `<>:"/\|?*`) {
		t.Fatalf("osSafeBase left reserved chars on Windows: %q", osSafeBase(id))
	}
}
