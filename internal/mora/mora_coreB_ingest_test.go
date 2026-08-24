package mora

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// coreBIngestInitCfg scaffolds a temp HOME + `mora init` vault and returns the
// loaded config. Mirrors the documented "real config with vault + index" recipe.
func coreBIngestInitCfg(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig after init: %v", err)
	}
	return cfg
}

// coreBIngestNear asserts got is within tol of want (for time.Now()-relative
// window bounds where the exact reference clock is captured inside the callee).
func coreBIngestNear(t *testing.T, got, want time.Time, label string) {
	t.Helper()
	const tol = 90 * time.Second
	d := got.Sub(want)
	if d < 0 {
		d = -d
	}
	if d > tol {
		t.Fatalf("%s: got %v, want ~%v (delta %v > %v)", label, got, want, d, tol)
	}
}

// coreBIngestWriteFile writes content to path, creating parent dirs.
func coreBIngestWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// coreBIngestMakeDocx writes a minimal but valid .docx (a zip with
// word/document.xml) whose visible text is body.
func coreBIngestMakeDocx(t *testing.T, path, body string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	xml := `<?xml version="1.0"?><w:document xmlns:w="x"><w:body><w:p><w:r><w:t>` +
		body + `</w:t></w:r></w:p></w:body></w:document>`
	if _, err := w.Write([]byte(xml)); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	coreBIngestWriteFile(t, path, buf.String())
}

// ---------------------------------------------------------------------------
// Path helpers (pure)
// ---------------------------------------------------------------------------

func TestCoreB_IngestStatusPaths(t *testing.T) {
	cfg := Config{StateDir: "/state", ConfigDir: "/cfg", VaultDir: "/vault"}
	if got := googleStatusPath(cfg, "work"); got != filepath.Join("/state", "sync", "google-work.json") {
		t.Fatalf("googleStatusPath = %q", got)
	}
	if got := imessageStatusPath(cfg, "main"); got != filepath.Join("/state", "sync", "imessage-main.json") {
		t.Fatalf("imessageStatusPath = %q", got)
	}
	if got := appleCalStatusPath(cfg, "cal"); got != filepath.Join("/state", "sync", "applecal-cal.json") {
		t.Fatalf("appleCalStatusPath = %q", got)
	}
}

func TestCoreB_IngestAppleCalDBPathLegacy(t *testing.T) {
	withTempHome(t)
	home, _ := os.UserHomeDir()
	// Only the legacy ~/Library/Calendars store exists => appleCalDBPath probes
	// the modern location, misses, and falls back to legacy.
	legacy := filepath.Join(home, "Library", "Calendars", "Calendar.sqlitedb")
	coreBIngestWriteFile(t, legacy, "db")
	if got := appleCalDBPath(); got != legacy {
		t.Fatalf("appleCalDBPath = %q, want legacy %q", got, legacy)
	}
}

func TestCoreB_IngestFileExists(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "here.txt")
	if err := os.WriteFile(present, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !genericutil.FileExists(present) {
		t.Fatalf("genericutil.FileExists(%q) = false, want true", present)
	}
	if genericutil.FileExists(filepath.Join(dir, "nope.txt")) {
		t.Fatalf("genericutil.FileExists(missing) = true, want false")
	}
}

func TestCoreB_IngestHomePaths(t *testing.T) {
	withTempHome(t)
	home, _ := os.UserHomeDir()

	if got := chatDBPath(); got != filepath.Join(home, "Library", "Messages", "chat.db") {
		t.Fatalf("chatDBPath = %q", got)
	}
	// No AddressBook/Calendar under the temp HOME, so both fall back to the
	// modern default rooted at HOME.
	ab := addressBookRoot()
	if !strings.HasPrefix(ab, home) || !strings.Contains(ab, filepath.Join("Library", "Application Support", "AddressBook", "Sources")) {
		t.Fatalf("addressBookRoot = %q, want under %q", ab, home)
	}
	db := appleCalDBPath()
	if !strings.HasPrefix(db, home) || !strings.Contains(db, "Calendar.sqlitedb") {
		t.Fatalf("appleCalDBPath = %q, want the modern default under %q", db, home)
	}
}

// ---------------------------------------------------------------------------
// windowFor* (pure)
// ---------------------------------------------------------------------------

func TestCoreB_IngestWindowForSourceGmail(t *testing.T) {
	now := time.Now()
	// Default 90-day lookback; label/calendar passthrough.
	w := windowForSource(Source{LabelIDs: []string{"L1"}, Calendar: "cal@x"}, google.KindGmailThread)
	coreBIngestNear(t, w.Since, now.AddDate(0, 0, -90), "gmail default Since")
	if !w.Until.IsZero() {
		t.Fatalf("gmail Until = %v, want zero (Since-only)", w.Until)
	}
	if len(w.Labels) != 1 || w.Labels[0] != "L1" || w.CalendarID != "cal@x" {
		t.Fatalf("gmail window passthrough: Labels=%v CalendarID=%q", w.Labels, w.CalendarID)
	}

	// Positive override.
	w = windowForSource(Source{SinceDays: 30}, google.KindGmailThread)
	coreBIngestNear(t, w.Since, now.AddDate(0, 0, -30), "gmail override Since")

	// Negative SinceDays for gmail does NOT mean all-time: Since = now-(-5) = now+5d.
	w = windowForSource(Source{SinceDays: -5}, google.KindGmailThread)
	coreBIngestNear(t, w.Since, now.AddDate(0, 0, 5), "gmail negative Since")
}

func TestCoreB_IngestWindowForSourceCalendar(t *testing.T) {
	now := time.Now()
	// SinceDays is ignored for calendar: fixed -6mo..+3mo.
	w := windowForSource(Source{SinceDays: 999}, google.KindCalEvent)
	coreBIngestNear(t, w.Since, now.AddDate(0, -6, 0), "calendar Since")
	coreBIngestNear(t, w.Until, now.AddDate(0, 3, 0), "calendar Until")
}

func TestCoreB_IMessageLookbackDays(t *testing.T) {
	cases := []struct {
		name string
		s    Source
		want int
	}{
		{"default", Source{}, 365},
		{"explicit override", Source{SinceDays: 30}, 30},
		{"all time", Source{SinceDays: -1}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := iMessageLookbackDays(tc.s); got != tc.want {
				t.Fatalf("iMessageLookbackDays(%+v) = %d, want %d", tc.s, got, tc.want)
			}
		})
	}
}

func TestCoreB_IngestWindowForIMessage(t *testing.T) {
	now := time.Now()
	coreBIngestNear(t, windowForIMessage(Source{}).Since, now.AddDate(0, 0, -365), "imsg default")
	coreBIngestNear(t, windowForIMessage(Source{SinceDays: 30}).Since, now.AddDate(0, 0, -30), "imsg override")
	// Negative => all-time (zero Since, no lower bound).
	w := windowForIMessage(Source{SinceDays: -1})
	if !w.Since.IsZero() || !w.Until.IsZero() {
		t.Fatalf("imsg all-time window = %+v, want zero bounds", w)
	}
}

func TestCoreB_IngestWindowForAppleCal(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	def := windowForAppleCal(Source{}, now)
	if !def.Since.Equal(now.AddDate(0, 0, -90)) {
		t.Fatalf("applecal default Since = %v", def.Since)
	}
	if !def.Until.Equal(now.AddDate(0, 0, 180)) {
		t.Fatalf("applecal default Until = %v", def.Until)
	}

	ov := windowForAppleCal(Source{SinceDays: 45}, now)
	if !ov.Since.Equal(now.AddDate(0, 0, -45)) {
		t.Fatalf("applecal override Since = %v", ov.Since)
	}
	if !ov.Until.Equal(now.AddDate(0, 0, 180)) {
		t.Fatalf("applecal override Until = %v", ov.Until)
	}

	neg := windowForAppleCal(Source{SinceDays: -1}, now)
	if !neg.Since.IsZero() {
		t.Fatalf("applecal all-time Since = %v, want zero", neg.Since)
	}
	if !neg.Until.Equal(now.AddDate(0, 0, 180)) {
		t.Fatalf("applecal all-time Until = %v", neg.Until)
	}
}

// ---------------------------------------------------------------------------
// sources registry: add / load / save
// ---------------------------------------------------------------------------

func TestCoreB_IngestAddSource(t *testing.T) {
	cfg := coreBIngestInitCfg(t)

	var out bytes.Buffer
	if err := addSource(cfg, []string{"filesystem", "--name", "notes", "--path", "/tmp/notes", "--scope", "work"}, &out); err != nil {
		t.Fatalf("addSource: %v", err)
	}
	var emitted Source
	if err := json.Unmarshal(out.Bytes(), &emitted); err != nil {
		t.Fatalf("addSource emit json: %v\n%s", err, out.String())
	}
	if emitted.Name != "notes" || emitted.Type != "filesystem" || emitted.Path != "/tmp/notes" || emitted.Scope != "work" {
		t.Fatalf("emitted source = %+v", emitted)
	}
	// Consent gate: a freshly added source is explicitly DISABLED (D-11).
	if emitted.Enabled == nil || *emitted.Enabled {
		t.Fatalf("new source Enabled = %v, want explicit false", emitted.Enabled)
	}

	got, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	var found *Source
	for i := range got {
		if got[i].Name == "notes" {
			found = &got[i]
		}
	}
	if found == nil || found.Path != "/tmp/notes" || found.IsEnabled() {
		t.Fatalf("persisted notes source = %+v", found)
	}

	// Re-add same name with a different path -> replace in place (no duplicate).
	if err := addSource(cfg, []string{"filesystem", "--name", "notes", "--path", "/tmp/other"}, &out); err != nil {
		t.Fatalf("addSource dedup: %v", err)
	}
	got, _ = loadSources(cfg)
	n := 0
	for _, s := range got {
		if s.Name == "notes" {
			n++
			if s.Path != "/tmp/other" {
				t.Fatalf("re-added notes path = %q, want /tmp/other", s.Path)
			}
		}
	}
	if n != 1 {
		t.Fatalf("notes appears %d times, want 1 (dedup)", n)
	}
}

func TestCoreB_IngestAddSourceErrors(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	var out bytes.Buffer

	if err := addSource(cfg, nil, &out); err == nil || !strings.Contains(err.Error(), "usage: mora sources add") {
		t.Fatalf("empty args err = %v", err)
	}
	if err := addSource(cfg, []string{"filesystem"}, &out); err == nil || !strings.Contains(err.Error(), "filesystem source requires --path") {
		t.Fatalf("missing --path err = %v", err)
	}
	if err := addSource(cfg, []string{"gmail", "--nope"}, &out); err == nil {
		t.Fatalf("unknown flag: want parse error, got nil")
	}
}

func TestCoreB_IngestLoadSourcesMissing(t *testing.T) {
	// A config whose ConfigDir has no sources.json => (nil, nil), never an error.
	cfg := Config{ConfigDir: t.TempDir()}
	got, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources missing err = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("loadSources missing = %v, want empty", got)
	}
}

func TestCoreB_IngestLoadSourcesCorrupt(t *testing.T) {
	cfg := Config{ConfigDir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "sources.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSources(cfg); err == nil {
		t.Fatalf("loadSources corrupt: want error, got nil")
	}
}

func TestCoreB_IngestSaveLoadRoundTrip(t *testing.T) {
	cfg := Config{ConfigDir: t.TempDir()}
	in := []Source{
		{Name: "a", Type: "filesystem", Scope: "personal", Path: "/x", Enabled: genericutil.Ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
		{Name: "b", Type: "gmail", Scope: "personal", Enabled: genericutil.Ptr(false), CreatedAt: "2026-01-02T00:00:00Z"},
	}
	if err := saveSources(cfg, in); err != nil {
		t.Fatalf("saveSources: %v", err)
	}
	got, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	if len(got) != 2 || got[0].Name != "a" || !got[0].IsEnabled() || got[1].Name != "b" || got[1].IsEnabled() {
		t.Fatalf("round-trip = %+v", got)
	}
}

func TestCoreB_IngestLoadSourcesGrandfather(t *testing.T) {
	// A legacy source with no `enabled` key normalizes nil => true (D-12).
	cfg := Config{ConfigDir: t.TempDir()}
	raw := `[{"name":"legacy","type":"filesystem","scope":"personal","created_at":"2026-01-01T00:00:00Z"}]`
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "sources.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	if len(got) != 1 || !got[0].IsEnabled() {
		t.Fatalf("grandfather migration: %+v, want Enabled=true", got)
	}
}

// ---------------------------------------------------------------------------
// ingestSource routing
// ---------------------------------------------------------------------------

func TestCoreB_IngestSourceRouting(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	var out bytes.Buffer

	// gdrive: deferred, no-op (0, nil).
	if n, err := ingestSource(cfg, Source{Type: "gdrive", Name: "g"}, &out); n != 0 || err != nil {
		t.Fatalf("gdrive route = (%d, %v), want (0, nil)", n, err)
	}

	// unknown type -> descriptive error.
	if _, err := ingestSource(cfg, Source{Type: "bogus", Name: "x"}, &out); err == nil || !strings.Contains(err.Error(), `unknown source type "bogus"`) {
		t.Fatalf("unknown route err = %v", err)
	}

	// filesystem -> delegates to ingestFilesystem (real count).
	dir := t.TempDir()
	coreBIngestWriteFile(t, filepath.Join(dir, "note.md"), "# hello")
	if n, err := ingestSource(cfg, Source{Type: "filesystem", Name: "fs", Path: dir, Scope: "personal"}, &out); err != nil || n != 1 {
		t.Fatalf("filesystem route = (%d, %v), want (1, nil)", n, err)
	}
}

func TestCoreB_IngestSourceConnectorRoutes(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "") // force placeholder creds for google routes
	var out bytes.Buffer

	type routeCase struct {
		typ  string
		want string
	}
	cases := []routeCase{
		{"gmail", "Google sign-in needs a one-time setup"},
		{"calendar", "Google sign-in needs a one-time setup"},
	}
	// Only darwin reaches the chat.db / Calendar readiness errors; elsewhere
	// these connectors skip with a note and no error (asserted below).
	if runtime.GOOS == "darwin" {
		cases = append(cases,
			routeCase{"imessage", "cannot read your Messages database"},
			routeCase{"applecalendar", "cannot read your Calendar database"})
	}
	for _, c := range cases {
		_, err := ingestSource(cfg, Source{Type: c.typ, Name: c.typ}, &out)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("ingestSource %q err = %v, want %q", c.typ, err, c.want)
		}
	}
	if runtime.GOOS != "darwin" {
		for _, typ := range []string{"imessage", "applecalendar"} {
			out.Reset()
			if n, err := ingestSource(cfg, Source{Type: typ, Name: typ}, &out); err != nil || n != 0 {
				t.Fatalf("ingestSource %q on %s = (%d, %v), want the (0, nil) skip", typ, runtime.GOOS, n, err)
			}
			if !strings.Contains(out.String(), "only runs on macOS") {
				t.Fatalf("ingestSource %q skip note missing:\n%s", typ, out.String())
			}
		}
	}
}

// ---------------------------------------------------------------------------
// ingestGoogle error branches
// ---------------------------------------------------------------------------

func TestCoreB_IngestGoogleNoCreds(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	// Force the embedded DEV_PLACEHOLDER creds (ResolveOAuthConfig fails first).
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	var out bytes.Buffer
	_, err := ingestGoogle(cfg, Source{Type: "gmail", Name: "g"}, google.KindGmailThread, &out)
	if err == nil || !strings.Contains(err.Error(), "Google sign-in needs a one-time setup") {
		t.Fatalf("no-creds err = %v", err)
	}
}

func TestCoreB_IngestGoogleNotConnected(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	// Supply real-looking BYO creds so ResolveOAuthConfig SUCCEEDS, then hit the
	// missing-token "not connected" branch.
	creds := filepath.Join(t.TempDir(), "client.json")
	coreBIngestWriteFile(t, creds, `{"installed":{"client_id":"real.apps.googleusercontent.com","client_secret":"s","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token"}}`)
	t.Setenv("MORA_GOOGLE_CREDENTIALS", creds)

	var out bytes.Buffer
	_, err := ingestGoogle(cfg, Source{Type: "gmail", Name: "g", Account: "work"}, google.KindGmailThread, &out)
	if err == nil || !strings.Contains(err.Error(), "not connected to google") {
		t.Fatalf("not-connected err = %v", err)
	}
	// Account-scoped connect hint.
	if !strings.Contains(err.Error(), "mora connect google --account work") {
		t.Fatalf("expected account-scoped connect hint, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// writeMappedMemory: create / idempotent-skip / rewrite / tombstone
// ---------------------------------------------------------------------------

func TestCoreB_IngestWriteMappedMemory(t *testing.T) {
	cfg := coreBIngestInitCfg(t)

	mm := memory.MappedMemory{
		StableID: "gmail_thread/abc123", Scope: "personal", Type: "email",
		Title: "First subject", Body: "hello world", Tags: []string{"gmail"},
		Source: "gmail", Provider: "gmail", ContentHash: "hash1",
		CreatedAt: "2020-01-01T00:00:00Z",
	}
	dest := filepath.Join(sourcesRoot(cfg), "gmail", memory.SafeFilename(mm.StableID)+".md")

	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatalf("first write: %v", err)
	}
	got, err := parseMemory(dest)
	if err != nil {
		t.Fatalf("parse first: %v", err)
	}
	if got.Title != "First subject" || got.ContentHash != "hash1" || got.CreatedAt != "2020-01-01T00:00:00Z" {
		t.Fatalf("first parsed = %+v", got)
	}

	// Same ContentHash + a changed title => idempotent SKIP (file unchanged).
	skip := mm
	skip.Title = "SHOULD NOT APPEAR"
	if err := writeMappedMemory(cfg, skip); err != nil {
		t.Fatalf("skip write: %v", err)
	}
	got, _ = parseMemory(dest)
	if got.Title != "First subject" {
		t.Fatalf("idempotent skip failed: title now %q", got.Title)
	}

	// Changed ContentHash => rewrite, but preserve the ORIGINAL created_at.
	changed := mm
	changed.Title = "Second subject"
	changed.ContentHash = "hash2"
	changed.CreatedAt = "2099-12-31T00:00:00Z"
	if err := writeMappedMemory(cfg, changed); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got, _ = parseMemory(dest)
	if got.Title != "Second subject" || got.ContentHash != "hash2" {
		t.Fatalf("rewrite parsed = %+v", got)
	}
	if got.CreatedAt != "2020-01-01T00:00:00Z" {
		t.Fatalf("rewrite clobbered created_at: %q, want the original", got.CreatedAt)
	}

	// Same hash but DeletedAt set => NOT skipped (tombstone is written).
	tomb := changed
	tomb.DeletedAt = "2026-06-30T00:00:00Z"
	if err := writeMappedMemory(cfg, tomb); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	got, _ = parseMemory(dest)
	if got.DeletedAt != "2026-06-30T00:00:00Z" {
		t.Fatalf("tombstone not written: deleted_at = %q", got.DeletedAt)
	}
}

// ---------------------------------------------------------------------------
// ingestFilesystem
// ---------------------------------------------------------------------------

func TestCoreB_IngestFilesystem(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	dir := t.TempDir()

	coreBIngestWriteFile(t, filepath.Join(dir, "a.md"), "# hi markdown")
	coreBIngestWriteFile(t, filepath.Join(dir, "b.txt"), "plain text body")
	coreBIngestWriteFile(t, filepath.Join(dir, "go.mod"), "module x")                    // curated metadata file
	coreBIngestMakeDocx(t, filepath.Join(dir, "notes.docx"), "Hello docx")               // extract path
	coreBIngestWriteFile(t, filepath.Join(dir, "bad.pdf"), "not really a pdf")           // extract fails -> skip
	coreBIngestWriteFile(t, filepath.Join(dir, "big.md"), strings.Repeat("a", 600*1024)) // oversized -> skip
	coreBIngestWriteFile(t, filepath.Join(dir, "empty.md"), "")                          // empty -> skip
	coreBIngestWriteFile(t, filepath.Join(dir, "image.png"), "PNGDATA")                  // non-curated ext -> skip
	coreBIngestWriteFile(t, filepath.Join(dir, "node_modules", "dep.md"), " dep")        // ignored dir -> skip

	var out bytes.Buffer
	src := Source{Type: "filesystem", Name: "fsn", Path: dir, Scope: "personal"}
	n, err := ingestFilesystem(cfg, src, &out)
	if err != nil {
		t.Fatalf("ingestFilesystem: %v", err)
	}
	// a.md + b.txt + go.mod + notes.docx = 4.
	if n != 4 {
		t.Fatalf("indexed %d files, want 4", n)
	}

	// A markdown memory landed under sources/filesystem/fsn with the right shape.
	id := "src_" + ContentHash(src.Name+":a.md")
	mp := filepath.Join(sourcesRoot(cfg), "filesystem", "fsn", id+".md")
	m, err := parseMemory(mp)
	if err != nil {
		t.Fatalf("parse a.md memory: %v", err)
	}
	if m.Title != "a.md" || m.Type != "source" || !strings.Contains(m.Text, "# hi markdown") {
		t.Fatalf("a.md memory = %+v", m)
	}
	if len(m.Tags) != 2 || m.Tags[0] != "filesystem" || m.Tags[1] != "fsn" {
		t.Fatalf("a.md tags = %v", m.Tags)
	}

	// The .docx was text-extracted, not indexed as raw zip bytes.
	docxID := "src_" + ContentHash(src.Name+":notes.docx")
	dm, err := parseMemory(filepath.Join(sourcesRoot(cfg), "filesystem", "fsn", docxID+".md"))
	if err != nil {
		t.Fatalf("parse docx memory: %v", err)
	}
	if dm.Text != "Hello docx" {
		t.Fatalf("docx extracted text = %q, want %q", dm.Text, "Hello docx")
	}

	// Freshness status persisted with the real count + a success stamp.
	st, err := memory.LoadStatus(syncStatusPathFor(cfg, src))
	if err != nil {
		t.Fatalf("load status: %v", err)
	}
	if st.ItemCount != 4 || st.Source != "fsn" || st.LastSuccessAt == "" {
		t.Fatalf("status = %+v, want ItemCount 4 + success stamp", st)
	}
}

func TestCoreB_IngestFilesystemMissingPath(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	var out bytes.Buffer
	// A missing root is a failed snapshot, not a clean zero-item walk. The error
	// must return and the status must record an attempt without claiming success.
	src := Source{Type: "filesystem", Name: "gone", Path: filepath.Join(t.TempDir(), "does-not-exist"), Scope: "personal"}
	n, err := ingestFilesystem(cfg, src, &out)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-path ingest err = %v, want not-exist", err)
	}
	if n != 0 {
		t.Fatalf("missing-path count = %d, want 0", n)
	}
	st, loadErr := memory.LoadStatus(syncStatusPathFor(cfg, src))
	if loadErr != nil {
		t.Fatalf("load missing-path status: %v", loadErr)
	}
	if st.ItemCount != 0 || st.LastAttemptAt == "" || st.LastError == "" || st.ErrorCount != 1 {
		t.Fatalf("missing-path failure status = %+v", st)
	}
	if st.LastSynced != "" || st.LastSuccessAt != "" {
		t.Fatalf("missing-path failure claimed a successful snapshot: %+v", st)
	}
}

func TestCoreB_IngestFilesystemWalkError(t *testing.T) {
	skipOnWindows(t, "chmod 0000 does not block WalkDir on Windows; the directory walk error cannot be provoked")
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; walk error unreachable")
	}
	cfg := coreBIngestInitCfg(t)
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	coreBIngestWriteFile(t, filepath.Join(locked, "hidden.md"), "must not be silently skipped")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	src := Source{Type: "filesystem", Name: "locked", Path: root, Scope: "personal"}
	if _, err := ingestFilesystem(cfg, src, io.Discard); err == nil || !strings.Contains(err.Error(), "walking filesystem source") {
		t.Fatalf("unreadable directory walk must fail loud, got %v", err)
	}
	st, err := memory.LoadStatus(syncStatusPathFor(cfg, src))
	if err != nil {
		t.Fatal(err)
	}
	if st.LastError == "" || st.LastAttemptAt == "" || st.LastSuccessAt != "" || st.LastSynced != "" {
		t.Fatalf("unreadable directory wrote a dishonest status: %+v", st)
	}
}

func TestCoreB_IngestFilesystemFailurePreservesPriorSuccess(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "source")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	coreBIngestWriteFile(t, filepath.Join(root, "note.md"), "last known good snapshot")
	src := Source{Type: "filesystem", Name: "docs", Path: root, Scope: "personal"}
	if _, err := ingestFilesystem(cfg, src, io.Discard); err != nil {
		t.Fatal(err)
	}
	before, err := memory.LoadStatus(syncStatusPathFor(cfg, src))
	if err != nil || before.LastSuccessAt == "" || before.LastSynced == "" {
		t.Fatalf("initial success status = %+v, err %v", before, err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	if _, err := ingestFilesystem(cfg, src, io.Discard); err == nil {
		t.Fatal("missing source after a prior success must fail")
	}
	after, err := memory.LoadStatus(syncStatusPathFor(cfg, src))
	if err != nil {
		t.Fatal(err)
	}
	if after.LastSuccessAt != before.LastSuccessAt || after.LastSynced != before.LastSynced {
		t.Fatalf("failed attempt advanced the prior success: before=%+v after=%+v", before, after)
	}
	if after.LastAttemptAt == "" || after.LastError == "" || after.ErrorCount != 1 {
		t.Fatalf("failed attempt was not recorded honestly: %+v", after)
	}
}

// ---------------------------------------------------------------------------
// connectFilesystem
// ---------------------------------------------------------------------------

func TestCoreB_IngestConnectFilesystemHappy(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	dir := t.TempDir()
	coreBIngestWriteFile(t, filepath.Join(dir, "readme.md"), "# hello")

	var out bytes.Buffer
	if err := connectFilesystem(context.Background(), []string{dir}, &out, testStderr); err != nil {
		t.Fatalf("connectFilesystem: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Enabled filesystem and indexed 1 file(s) from") {
		t.Fatalf("connect output missing success line:\n%s", out.String())
	}
	// The source was registered ENABLED under the folder's base name.
	cfg, _ := loadConfig()
	sources, _ := loadSources(cfg)
	want := defaultFilesystemSourceName(dir)
	var found *Source
	for i := range sources {
		if sources[i].Name == want {
			found = &sources[i]
		}
	}
	if found == nil || found.Type != "filesystem" || !found.IsEnabled() {
		t.Fatalf("connected source = %+v (want enabled filesystem %q)", found, want)
	}
}

func TestCoreB_IngestConnectFilesystemReconnectAndConflict(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	dir1 := t.TempDir()
	coreBIngestWriteFile(t, filepath.Join(dir1, "a.md"), "# a")

	var out bytes.Buffer
	if err := connectFilesystem(context.Background(), []string{dir1, "--name", "shared"}, &out, testStderr); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	cfg, _ := loadConfig()
	sources, _ := loadSources(cfg)
	var created string
	for _, s := range sources {
		if s.Name == "shared" {
			created = s.CreatedAt
		}
	}
	if created == "" {
		t.Fatalf("shared source not created")
	}

	// Re-connect the SAME name + SAME path => refresh in place, CreatedAt preserved.
	out.Reset()
	if err := connectFilesystem(context.Background(), []string{dir1, "--name", "shared"}, &out, testStderr); err != nil {
		t.Fatalf("re-connect: %v", err)
	}
	sources, _ = loadSources(cfg)
	n := 0
	for _, s := range sources {
		if s.Name == "shared" {
			n++
			if s.CreatedAt != created {
				t.Fatalf("re-connect changed CreatedAt: %q -> %q", created, s.CreatedAt)
			}
		}
	}
	if n != 1 {
		t.Fatalf("re-connect stacked duplicates: shared appears %d times", n)
	}

	// Same name, DIFFERENT path => refuse (would clobber the first).
	dir2 := t.TempDir()
	coreBIngestWriteFile(t, filepath.Join(dir2, "b.md"), "# b")
	out.Reset()
	err := connectFilesystem(context.Background(), []string{dir2, "--name", "shared"}, &out, testStderr)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("name-collision err = %v, want 'already exists'", err)
	}
}

func TestCoreB_IngestConnectFilesystemCorruptRegistry(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, _ := loadConfig()
	// A corrupt sources.json must NOT be silently overwritten (that would destroy
	// every other registered connector).
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "sources.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	coreBIngestWriteFile(t, filepath.Join(dir, "a.md"), "# a")
	var out bytes.Buffer
	err := connectFilesystem(context.Background(), []string{dir}, &out, testStderr)
	if err == nil || !strings.Contains(err.Error(), "cannot read existing sources") {
		t.Fatalf("corrupt-registry err = %v", err)
	}
}

func TestCoreB_IngestConnectFilesystemErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	var out bytes.Buffer

	// No path at all -> usage error.
	if err := connectFilesystem(context.Background(), nil, &out, testStderr); err == nil || !strings.Contains(err.Error(), "usage: mora connect filesystem") {
		t.Fatalf("no-path err = %v", err)
	}
	// A path that does not exist -> cannot read.
	missing := filepath.Join(t.TempDir(), "nope")
	if err := connectFilesystem(context.Background(), []string{missing}, &out, testStderr); err == nil || !strings.Contains(err.Error(), "cannot read") {
		t.Fatalf("missing-path err = %v", err)
	}
	// A file (not a directory) -> is not a directory.
	f := filepath.Join(t.TempDir(), "file.txt")
	coreBIngestWriteFile(t, f, "x")
	if err := connectFilesystem(context.Background(), []string{f}, &out, testStderr); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("file-path err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// backfillEnabledIMessage / connectIMessage (darwin-gated fetcher failures)
// ---------------------------------------------------------------------------

func TestCoreB_IngestBackfillEnabledIMessageEmpty(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	var out bytes.Buffer
	// No enabled imessage source => nothing to ingest; rebuild succeeds; (0, nil).
	total, err := backfillEnabledIMessage(context.Background(), cfg, &out)
	if err != nil {
		t.Fatalf("backfill empty err = %v", err)
	}
	if total != 0 {
		t.Fatalf("backfill empty total = %d, want 0", total)
	}
}

func TestCoreB_IngestBackfillEnabledIMessageFailure(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("failure surfacing needs the darwin chat.db readiness path; non-darwin skips imessage ingest entirely")
	}
	cfg := coreBIngestInitCfg(t)
	// Enable an imessage source; the temp-HOME chat.db is missing, so its
	// ingest fails and the failure is surfaced (never swallowed).
	if err := setSourceEnabled(cfg, "imessage", true); err != nil {
		t.Fatalf("setSourceEnabled: %v", err)
	}
	var out bytes.Buffer
	total, err := backfillEnabledIMessage(context.Background(), cfg, &out)
	if err == nil || !strings.Contains(err.Error(), "source(s) failed to sync") {
		t.Fatalf("backfill failure err = %v, want a failed-sync summary", err)
	}
	if total != 0 {
		t.Fatalf("backfill failure total = %d, want 0", total)
	}
	if !strings.Contains(out.String(), "sync incomplete") {
		t.Fatalf("expected a per-source warning, got:\n%s", out.String())
	}
}

func TestCoreB_IngestConnectIMessageSinceDays(t *testing.T) {
	asDarwinOnWindows(t)
	withTempHome(t)
	run(t, "init")
	var out bytes.Buffer
	// --since-days -1 persists an all-time override before readiness stops us.
	if err := connectIMessage(context.Background(), []string{"--since-days", "-1"}, &out); err != nil {
		t.Fatalf("connectIMessage: %v", err)
	}
	cfg, _ := loadConfig()
	sources, _ := loadSources(cfg)
	var im *Source
	for i := range sources {
		if sources[i].Type == "imessage" {
			im = &sources[i]
		}
	}
	if im == nil || im.SinceDays != -1 {
		t.Fatalf("imessage source = %+v, want SinceDays -1 persisted", im)
	}
}

func TestCoreB_IngestConnectIMessageBadFlag(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	var out bytes.Buffer
	if err := connectIMessage(context.Background(), []string{"--nope"}, &out); err == nil {
		t.Fatalf("connectIMessage bad flag: want parse error, got nil")
	}
}

func TestCoreB_IngestConnectIMessageStopsWithoutFDA(t *testing.T) {
	asDarwinOnWindows(t)
	withTempHome(t)
	run(t, "init")
	var out bytes.Buffer
	// Temp HOME has no ~/Library/Messages/chat.db, so readiness fails and connect
	// stops at the honest guidance (returns nil, no false backfill). On non-darwin
	// the readiness check stops earlier with the macOS-only note instead.
	if err := connectIMessage(context.Background(), nil, &out); err != nil {
		t.Fatalf("connectIMessage err = %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "enabled imessage") {
		t.Fatalf("connectIMessage output missing enable line:\n%s", s)
	}
	if !strings.Contains(s, "last 365 days; use --since-days") {
		t.Fatalf("connectIMessage output must disclose default lookback:\n%s", s)
	}
	guidance := "No Messages database found"
	// Read the SAME injectable seam the source uses (runtimeGOOS), so the
	// expectation stays in sync when asDarwinOnWindows injects darwin on Windows.
	// On native Linux/macOS no injection is active, so runtimeGOOS()==runtime.GOOS.
	if runtimeGOOS() != "darwin" {
		guidance = "only runs on macOS"
	}
	if !strings.Contains(s, guidance) {
		t.Fatalf("connectIMessage output missing readiness guidance:\n%s", s)
	}
	// The imessage source row was created + enabled.
	cfg, _ := loadConfig()
	sources, _ := loadSources(cfg)
	var im *Source
	for i := range sources {
		if sources[i].Type == "imessage" {
			im = &sources[i]
		}
	}
	if im == nil || !im.IsEnabled() {
		t.Fatalf("imessage source = %+v, want enabled", im)
	}
}

func TestCoreB_IngestIMessageFDADenied(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	var out bytes.Buffer
	// Temp HOME => chat.db missing/unreadable => NewLiveFetcher fails => the FDA
	// guidance error. On non-darwin the connector skips with (0, nil) + a note.
	_, err := ingestIMessage(cfg, Source{Type: "imessage", Name: "im"}, &out)
	if runtime.GOOS != "darwin" {
		if err != nil || !strings.Contains(out.String(), "only runs on macOS") {
			t.Fatalf("ingestIMessage on %s = %v, want nil + macOS-only note:\n%s", runtime.GOOS, err, out.String())
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "cannot read your Messages database") {
		t.Fatalf("ingestIMessage err = %v, want FDA guidance", err)
	}
}

func TestCoreB_IngestAppleCalFDADenied(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	var out bytes.Buffer
	_, err := ingestAppleCal(cfg, Source{Type: "applecalendar", Name: "cal"}, &out)
	if runtime.GOOS != "darwin" {
		if err != nil || !strings.Contains(out.String(), "only runs on macOS") {
			t.Fatalf("ingestAppleCal on %s = %v, want nil + macOS-only note:\n%s", runtime.GOOS, err, out.String())
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "cannot read your Calendar database") {
		t.Fatalf("ingestAppleCal err = %v, want FDA guidance", err)
	}
}
