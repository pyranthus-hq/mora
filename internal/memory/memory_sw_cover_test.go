package memory

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSw_IngestNilStatusUsesCustomMapper(t *testing.T) {
	f := &fakeFetcher{pages: map[string]Page{
		"": {Items: []Item{{Kind: kindGmailThread, ProviderID: "t-nil", Title: "raw", Body: "body"}}},
	}}
	var written []MappedMemory
	res, err := Ingest(IngestParams{
		Fetcher:    f,
		Kind:       kindGmailThread,
		Scope:      "sw-scope",
		BodyBudget: 7,
		Map: func(it Item, scope string, budget int) MappedMemory {
			if scope != "sw-scope" || budget != 7 {
				t.Fatalf("custom mapper got scope=%q budget=%d", scope, budget)
			}
			return MappedMemory{ProviderID: it.ProviderID, Title: "mapped"}
		},
		Write: func(m MappedMemory) error {
			written = append(written, m)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == nil {
		t.Fatal("nil input status should be initialized")
	}
	if res.Status.ItemCount != 1 || len(written) != 1 {
		t.Fatalf("expected one written item, status=%+v written=%+v", res.Status, written)
	}
	if written[0].ProviderID != "t-nil" || written[0].Title != "mapped" {
		t.Fatalf("custom mapper output not written: %+v", written[0])
	}
	if written[0].LastSynced == "" {
		t.Fatal("ingest should stamp LastSynced before Write")
	}
}

func TestSw_CanonicalMetaRejectsUnsupportedValues(t *testing.T) {
	got, err := CanonicalMeta(map[string]any{"bad": func() {}})
	if err == nil {
		t.Fatalf("expected unsupported meta value to error, got JSON %q", got)
	}
	if got != "" {
		t.Fatalf("failed meta canonicalization should return empty JSON, got %q", got)
	}
}

func TestSw_LoadStatusReadErrorForDirectory(t *testing.T) {
	got, err := LoadStatus(t.TempDir())
	if err == nil {
		t.Fatalf("reading a directory as status should error, got status %+v", got)
	}
}

func TestSw_SaveStatusMkdirErrorWhenParentIsFile(t *testing.T) {
	dir := t.TempDir()
	parentFile := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := SaveStatus(filepath.Join(parentFile, "status.json"), &SyncStatus{Source: "gmail"})
	if err == nil {
		t.Fatal("expected MkdirAll to fail when a path component is a file")
	}
}

func TestSw_SaveStatusWriteErrorWhenDirectoryNotWritable(t *testing.T) {
	skipOnWindows(t, "chmod 0500 does not make a directory unwritable to the owner on Windows (read-only attribute, not an ACL deny), so the temp-file write still succeeds")
	if os.Geteuid() == 0 {
		t.Skip("runs as root — the 0500 write bit is bypassed, so the write error can't be provoked")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o700)
	})

	err := SaveStatus(filepath.Join(dir, "status.json"), &SyncStatus{Source: "gmail"})
	if err == nil {
		t.Fatal("expected temp status write to fail in a non-writable directory")
	}
}

func TestSw_SaveStatusRenameErrorWhenTargetIsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	err := SaveStatus(path, &SyncStatus{Source: "gmail"})
	if err == nil {
		t.Fatal("expected renaming temp status file over a directory to fail")
	}
	if runtime.GOOS == "windows" {
		// MoveFileEx onto an existing directory returns ERROR_ACCESS_DENIED
		// ("Access is denied."), not a POSIX EISDIR/ENOTDIR — the rename still
		// fails (asserted above), only the error string differs.
		if !strings.Contains(err.Error(), "Access is denied") {
			t.Fatalf("expected directory rename error, got %v", err)
		}
	} else if !strings.Contains(err.Error(), "is a directory") && !strings.Contains(err.Error(), "not a directory") && !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("expected directory rename error, got %v", err)
	}
}
