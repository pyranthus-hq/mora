package mora

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestConnectFilesystemOneShot — `mora connect filesystem <path>` is the one-shot
// convenience that mirrors `connect google`/`connect imessage`: it registers the
// directory as an ENABLED filesystem source, indexes it, and rebuilds the index,
// so the folder's contents are immediately searchable. Unlike `sources add` (which
// lands DISABLED behind the consent gate), naming a folder here IS the consent.
func TestConnectFilesystemOneShot(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("ziggurat protocol meeting notes"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runErr(t, "connect", "filesystem", dir)
	if err != nil {
		t.Fatalf("connect filesystem should succeed: %v\n%s", err, out)
	}

	// The source is registered AND enabled in one shot.
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	sources, _ := loadSources(cfg)
	var fsSrc *Source
	for i := range sources {
		if sources[i].Type == "filesystem" {
			fsSrc = &sources[i]
		}
	}
	if fsSrc == nil {
		t.Fatalf("expected a filesystem source to be registered, got %+v", sources)
	}
	if !fsSrc.IsEnabled() {
		t.Fatalf("connect filesystem should ENABLE the source (one-shot convenience), got disabled")
	}
	if fsSrc.Path != dir {
		t.Fatalf("expected source path %q, got %q", dir, fsSrc.Path)
	}

	// The directory's contents were ingested and are searchable.
	res := run(t, "search", "ziggurat", "--json")
	var got []Memory
	if err := json.Unmarshal([]byte(res), &got); err != nil {
		t.Fatalf("search json: %v\n%s", err, res)
	}
	if len(got) == 0 {
		t.Fatalf("expected the indexed file to be searchable after connect filesystem, got 0 results")
	}
}

// TestConnectFilesystemMissingPathErrors — a typo'd / nonexistent path must fail
// loudly at connect time rather than silently registering a source that indexes
// zero files (the filesystem walk swallows a missing-root error to stay resumable,
// so the existence check belongs here).
func TestConnectFilesystemMissingPathErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	out, err := runErr(t, "connect", "filesystem", missing)
	if err == nil {
		t.Fatalf("connect filesystem on a nonexistent path should error, got success:\n%s", out)
	}

	// And it must NOT have registered a (broken) source.
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	sources, _ := loadSources(cfg)
	for _, s := range sources {
		if s.Type == "filesystem" {
			t.Fatalf("a failed connect must not register a filesystem source, got %+v", s)
		}
	}
}

// TestConnectFilesystemRequiresPath — invoked with no path at all, the command
// prints a usage error instead of doing anything.
func TestConnectFilesystemRequiresPath(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	if _, err := runErr(t, "connect", "filesystem"); err == nil {
		t.Fatal("connect filesystem with no path should error")
	}
}

// TestConnectFilesystemNameFlagAfterPath — flags must apply even when they follow
// the positional <path> (the documented form `connect filesystem ~/Documents
// --name docs`). Go's flag package stops at the first non-flag arg, so the command
// must pull the path out before parsing.
func TestConnectFilesystemNameFlagAfterPath(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	dir := filepath.Join(t.TempDir(), "rawname")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	run(t, "connect", "filesystem", dir, "--name", "custom")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	sources, _ := loadSources(cfg)
	var got *Source
	for i := range sources {
		if sources[i].Type == "filesystem" {
			got = &sources[i]
		}
	}
	if got == nil {
		t.Fatalf("expected a filesystem source, got %+v", sources)
	}
	if got.Name != "custom" {
		t.Fatalf("--name after the path should be honored, expected name %q, got %q", "custom", got.Name)
	}
}

// TestConnectFilesystemPreservesCorruptSources — if sources.json is unreadable,
// connect must refuse rather than overwrite it with only the new source (which
// would destroy every other registered source). The bad file is left for the user
// to repair.
func TestConnectFilesystemPreservesCorruptSources(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	dir := filepath.Join(t.TempDir(), "notes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cdir := configDirFor(t)
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		t.Fatal(err)
	}
	const corrupt = "{ this is not valid json"
	if err := os.WriteFile(filepath.Join(cdir, "sources.json"), []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runErr(t, "connect", "filesystem", dir)
	if err == nil {
		t.Fatalf("connect filesystem should refuse to run with an unreadable sources.json, got success:\n%s", out)
	}
	got, _ := os.ReadFile(filepath.Join(cdir, "sources.json"))
	if string(got) != corrupt {
		t.Fatalf("a failed connect must not overwrite sources.json; got %q", string(got))
	}
}

// TestConnectFilesystemStoresAbsolutePath — a relative path must be canonicalized
// to absolute before persisting, so the scheduled `ingest --all` job (which runs
// from launchd's cwd, not the user's shell) targets the right folder.
func TestConnectFilesystemStoresAbsolutePath(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	parent := t.TempDir()
	sub := filepath.Join(parent, "rel-notes")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "n.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	if err := os.Chdir(parent); err != nil {
		t.Fatal(err)
	}

	run(t, "connect", "filesystem", "rel-notes")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	sources, _ := loadSources(cfg)
	var got *Source
	for i := range sources {
		if sources[i].Type == "filesystem" {
			got = &sources[i]
		}
	}
	if got == nil {
		t.Fatalf("expected a filesystem source, got %+v", sources)
	}
	if !filepath.IsAbs(got.Path) {
		t.Fatalf("connect should persist an absolute path (relative paths break scheduled ingest), got %q", got.Path)
	}
}

// TestConnectFilesystemNameCollisionErrors — two *different* folders that derive
// the same default name must not silently clobber each other; the second errors
// and suggests --name, and the first source is preserved.
func TestConnectFilesystemNameCollisionErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	dirA := filepath.Join(t.TempDir(), "shared")
	dirB := filepath.Join(t.TempDir(), "shared") // same base name, different parent
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dirA, "a.md"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "b.md"), []byte("bravo"), 0o644); err != nil {
		t.Fatal(err)
	}

	run(t, "connect", "filesystem", dirA) // name defaults to "shared"
	out, err := runErr(t, "connect", "filesystem", dirB)
	if err == nil {
		t.Fatalf("a different folder with the same base name should error, not silently clobber:\n%s", out)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	sources, _ := loadSources(cfg)
	var shared *Source
	for i := range sources {
		if sources[i].Name == "shared" {
			shared = &sources[i]
		}
	}
	if shared == nil {
		t.Fatalf("first 'shared' source should be preserved, got %+v", sources)
	}
	if shared.Path != dirA {
		t.Fatalf("collision must not overwrite the first folder; expected path %q, got %q", dirA, shared.Path)
	}
}

// TestConnectFilesystemTwoFoldersCoexist — the default source name is the folder's
// base name, so connecting two different folders registers two distinct sources
// (no --name needed) and both are searchable.
func TestConnectFilesystemTwoFoldersCoexist(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	dirA := filepath.Join(t.TempDir(), "alpha-notes")
	dirB := filepath.Join(t.TempDir(), "bravo-notes")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dirA, "a.md"), []byte("xylophone in folder alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "b.md"), []byte("kumquat in folder bravo"), 0o644); err != nil {
		t.Fatal(err)
	}

	run(t, "connect", "filesystem", dirA)
	run(t, "connect", "filesystem", dirB)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	sources, _ := loadSources(cfg)
	fsCount := 0
	for _, s := range sources {
		if s.Type == "filesystem" {
			fsCount++
		}
	}
	if fsCount != 2 {
		t.Fatalf("expected 2 coexisting filesystem sources, got %d: %+v", fsCount, sources)
	}
	for _, term := range []string{"xylophone", "kumquat"} {
		res := run(t, "search", term, "--json")
		var got []Memory
		if err := json.Unmarshal([]byte(res), &got); err != nil {
			t.Fatalf("search json: %v\n%s", err, res)
		}
		if len(got) == 0 {
			t.Fatalf("expected %q searchable across both folders, got 0 results", term)
		}
	}
}

// TestConnectFilesystemReconnectRefreshes — re-connecting a folder under an
// existing name refreshes it in place (no duplicate row) and preserves the
// original created_at rather than resetting it.
func TestConnectFilesystemReconnectRefreshes(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	dir := filepath.Join(t.TempDir(), "myfolder")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("first content marigold"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// Plant the source with a distinctive old created_at; its base name "myfolder"
	// is exactly the default name connect will derive, so the reconnect targets it.
	const planted = "2020-01-01T00:00:00Z"
	if err := saveSources(cfg, []Source{{Name: "myfolder", Type: "filesystem", Scope: "personal", Path: dir, Enabled: ptr(true), CreatedAt: planted}}); err != nil {
		t.Fatalf("saveSources: %v", err)
	}

	run(t, "connect", "filesystem", dir)

	sources, _ := loadSources(cfg)
	fsCount := 0
	var got Source
	for _, s := range sources {
		if s.Type == "filesystem" {
			fsCount++
			got = s
		}
	}
	if fsCount != 1 {
		t.Fatalf("reconnect should refresh in place, expected 1 filesystem source, got %d", fsCount)
	}
	if got.CreatedAt != planted {
		t.Fatalf("reconnect should preserve created_at %q, got %q", planted, got.CreatedAt)
	}
}
