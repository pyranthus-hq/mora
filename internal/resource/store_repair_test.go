package resource

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

func TestResourceRecordIdentityMustMatchDirectory(t *testing.T) {
	root := t.TempDir()
	c := skill("", "# One")
	r, _, _, err := Import(root, c)
	if err != nil {
		t.Fatal(err)
	}
	id := r.StableID
	r.StableID = "foreign-record"
	body, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(root, "resources", id, "record.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	if found, err := existingID(root, c.Kind, r.CanonicalSource); err != nil || found != "" {
		t.Fatalf("wrong directory matched %q: %v", found, err)
	}
	c.StableID = id
	c.Pasted = []byte("# Two")
	if _, _, _, err := Import(root, c); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("mismatched stored identity: %v", err)
	}
}

func TestResourceReadRefusesLinkedRevision(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows junction covered by native test")
	}
	root := t.TempDir()
	r, v, _, err := Import(root, skill("", "# One"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "resources", r.StableID, "revisions", v.RevisionDigest)
	outside := filepath.Join(t.TempDir(), "revision")
	if err := os.Rename(path, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadRevision(root, r.StableID, v.RevisionDigest); err == nil {
		t.Fatal("followed linked revision")
	}
}

func TestResourceLargeEscapedManifestRoundTrip(t *testing.T) {
	root, source := t.TempDir(), t.TempDir()
	for i := 0; i < 900; i++ {
		name := fmt.Sprintf("%04d-%s.md", i, strings.Repeat("&", 190))
		if err := os.WriteFile(filepath.Join(source, name), []byte("evidence"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := Candidate{Kind: KindTool, Purpose: "large fixture", EffectiveScope: "project:alpha", Source: SourceFolder, Folder: source}
	r, v, _, err := Import(root, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(mustJSON(v)) <= 128<<10 {
		t.Fatal("fixture does not exercise old read cap")
	}
	if _, _, err := ReadRevision(root, r.StableID, v.RevisionDigest); err != nil {
		t.Fatal(err)
	}
	if _, _, result, err := Import(root, c); err != nil || !result.Duplicate {
		t.Fatalf("duplicate: %#v %v", result, err)
	}
}

func TestResourceRecordPublicationFailurePreservesRetryAndExistingBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	original := atomicio.MarkerSyncFn
	t.Cleanup(func() { atomicio.MarkerSyncFn = original })
	boom := errors.New("sync failure")
	atomicio.MarkerSyncFn = func(*os.File) error { return boom }
	if err := writeCreateExclusive(path, []byte("first")); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed write exposed record: %v", err)
	}
	atomicio.MarkerSyncFn = original
	if err := writeCreateExclusive(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := writeCreateExclusive(path, []byte("second")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrote immutable record: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "first" {
		t.Fatalf("existing bytes: %q %v", body, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging files remain: %v %v", entries, err)
	}
}
