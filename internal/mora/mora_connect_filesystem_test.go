package mora

import (
	"github.com/pyranthus-hq/mora/internal/genericutil"
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
	wantPath, _ := filepath.EvalSymlinks(dir)
	if fsSrc.Path != wantPath {
		t.Fatalf("expected source path %q, got %q", wantPath, fsSrc.Path)
	}

	// The directory's contents were ingested and are searchable.
	res := run(t, "search", "ziggurat", "--json")
	// Plan 01-07: `search --json` carries its array under `memories`.
	got, err := decodeMemoriesJSON(t, res)
	if err != nil {
		t.Fatalf("search json: %v\n%s", err, res)
	}
	if len(got) == 0 {
		t.Fatalf("expected the indexed file to be searchable after connect filesystem, got 0 results")
	}
}

// TestConnectFilesystemMissingPathErrors — a typo'd / nonexistent path must fail
// loudly at connect time rather than registering a broken source. The ingest walk
// also fails closed if a previously valid root later disappears; this check keeps
// the invalid source out of the registry in the first place.
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
	want, _ := filepath.EvalSymlinks(filepath.Join(parent, "rel-notes"))
	if got.Path != want {
		t.Fatalf("connect should persist the resolved absolute path (relative paths break scheduled ingest), expected %q, got %q", want, got.Path)
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
	wantA, _ := filepath.EvalSymlinks(dirA)
	if shared.Path != wantA {
		t.Fatalf("collision must not overwrite the first folder; expected path %q, got %q", wantA, shared.Path)
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
		// Plan 01-07: `search --json` carries its array under `memories`.
		got, err := decodeMemoriesJSON(t, res)
		if err != nil {
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
	// Plant the symlink-resolved path so it matches what connect canonicalizes to.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	const planted = "2020-01-01T00:00:00Z"
	if err := saveSources(cfg, []Source{{Name: "myfolder", Type: "filesystem", Scope: "personal", Path: realDir, Enabled: genericutil.Ptr(true), CreatedAt: planted}}); err != nil {
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

// TestConnectFilesystemFollowsSymlinkedDir — a symlinked directory (common on macOS
// with iCloud "Desktop & Documents", Google Drive for Desktop, or Dropbox) must
// index its contents. filepath.WalkDir does NOT descend a symlinked root, so connect
// resolves the link before persisting; without that the source is enabled but indexes
// zero files (the exact silent no-op the existence guard claims to prevent).
func TestConnectFilesystemFollowsSymlinkedDir(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "note.md"), []byte("ziggurat protocol"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	out, err := runErr(t, "connect", "filesystem", link)
	if err != nil {
		t.Fatalf("connect filesystem on a symlinked dir should succeed: %v\n%s", err, out)
	}

	// The file inside the symlinked dir must actually be searchable.
	res := run(t, "search", "ziggurat", "--json")
	// Plan 01-07: `search --json` carries its array under `memories`.
	got, err := decodeMemoriesJSON(t, res)
	if err != nil {
		t.Fatalf("search json: %v\n%s", err, res)
	}
	if len(got) == 0 {
		t.Fatalf("connect must follow a symlinked root and index its contents, got 0 results")
	}
}

// TestConnectFilesystemRejectsFile — connect filesystem takes a folder; a file path
// must error rather than register a "filesystem" source pointing at a single file.
func TestConnectFilesystemRejectsFile(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	file := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runErr(t, "connect", "filesystem", file)
	if err == nil {
		t.Fatalf("connect filesystem on a file path should error (it takes a folder):\n%s", out)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	sources, _ := loadSources(cfg)
	for _, s := range sources {
		if s.Type == "filesystem" {
			t.Fatalf("a rejected file path must not register a source, got %+v", s)
		}
	}
}

// TestConnectFilesystemScopeFlag — --scope is honored and written through to the
// source, so a project folder lands in the project namespace rather than silently
// defaulting to personal.
func TestConnectFilesystemScopeFlag(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	run(t, "connect", "filesystem", dir, "--scope", "project:foo")

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
	if got.Scope != "project:foo" {
		t.Fatalf("--scope should be honored, expected %q, got %q", "project:foo", got.Scope)
	}
}

// TestConnectFilesystemNameFlagBeforePath — the leading-flag form
// `connect filesystem --name x <path>` must resolve the path via fs.Arg(0) and honor
// the name (the code claims this works; lock it down against a flag-parsing refactor).
func TestConnectFilesystemNameFlagBeforePath(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	dir := filepath.Join(t.TempDir(), "rawname")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	run(t, "connect", "filesystem", "--name", "custom", dir)

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
		t.Fatalf("--name before the path should be honored, expected %q, got %q", "custom", got.Name)
	}
	if filepath.Base(got.Path) != "rawname" {
		t.Fatalf("leading-flag form must still resolve the positional path, got %q", got.Path)
	}
}

// TestConnectFilesystemPathFlag — the path may be supplied via --path instead of
// positionally.
func TestConnectFilesystemPathFlag(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	dir := filepath.Join(t.TempDir(), "viaflag")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	run(t, "connect", "filesystem", "--path", dir)

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
		t.Fatalf("expected a filesystem source from --path, got %+v", sources)
	}
	if filepath.Base(got.Path) != "viaflag" {
		t.Fatalf("--path should set the source path, got %q", got.Path)
	}
}

// TestDefaultFilesystemSourceName — the default name is the folder's base name, with
// a "filesystem" fallback for degenerate paths.
func TestDefaultFilesystemSourceName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/Users/x/notes", "notes"},
		{"/Users/x/notes/", "notes"},
		{".", "filesystem"},
		{string(filepath.Separator), "filesystem"},
		{"", "filesystem"},
	}
	for _, c := range cases {
		if got := defaultFilesystemSourceName(c.in); got != c.want {
			t.Errorf("defaultFilesystemSourceName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
