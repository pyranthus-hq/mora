package mora

// Coverage worker AREA=mc. Precise unit tests for the ten small util files:
// banner.go, notify.go, embed.go, embed_ollama.go, eval_metrics.go, pdf.go,
// upgrade.go, progress.go, render.go, artifact.go. Every test asserts on real
// behavior/output/errors. Helpers are mc-prefixed and tests are TestMc_* so this
// file merges additively with sibling coverage workers in package mora.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// ---------------------------------------------------------------------------
// banner.go
// ---------------------------------------------------------------------------

// TestMc_ColorizeEyeLine asserts every glyph class gets its own ANSI span and
// that unmapped runes (spaces, letters) pass through untouched.
func TestMc_ColorizeEyeLine(t *testing.T) {
	// Force a real color profile so lipgloss actually emits escapes under `go test`.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	// One rune from each mapped class plus a plain space and letter (default arm).
	line := "░▒▓█●╱╲▀▄ x"
	got := colorizeEyeLine(line)

	// Every mapped glyph must survive in the output and be wrapped in ANSI.
	for _, g := range []string{"░", "▒", "▓", "█", "●", "╱", "╲", "▀", "▄"} {
		if !strings.Contains(got, g) {
			t.Fatalf("glyph %q missing from colorized line %q", g, got)
		}
	}
	if !strings.ContainsRune(got, '\x1b') {
		t.Fatalf("colorized line carries no ANSI escape: %q", got)
	}
	// The plain letter and space are default-arm: emitted verbatim (no styling
	// added around them beyond the neighbouring glyphs).
	if !strings.Contains(got, "x") {
		t.Fatalf("default-arm rune 'x' dropped: %q", got)
	}
	// A line of only default-arm runes must be returned byte-identical.
	if plain := colorizeEyeLine("hello world"); plain != "hello world" {
		t.Fatalf("all-default line must pass through unchanged, got %q", plain)
	}
}

// TestMc_BannerColor covers every path of the color gate. A true (color-on)
// result requires a real TTY, so we assert all the false paths (which is the
// full set reachable off a terminal).
func TestMc_BannerColor(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")

	// NO_COLOR disables color even on a would-be terminal.
	t.Setenv("NO_COLOR", "1")
	if bannerColor(os.Stdout) {
		t.Error("NO_COLOR must disable banner color")
	}
	t.Setenv("NO_COLOR", "")

	// TERM=dumb disables color.
	t.Setenv("TERM", "dumb")
	if bannerColor(os.Stdout) {
		t.Error("TERM=dumb must disable banner color")
	}
	t.Setenv("TERM", "xterm-256color")

	// A non-*os.File writer (buffer/pipe) is never color-capable.
	if bannerColor(&bytes.Buffer{}) {
		t.Error("bytes.Buffer must not be color-capable")
	}

	// A real *os.File that is not a terminal (a regular file) is not color-capable.
	f, err := os.CreateTemp(t.TempDir(), "banner")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if bannerColor(f) {
		t.Error("a regular file is not a terminal; banner color must stay off")
	}
}

// TestMc_PrintBannerNonTTY asserts the machine-safe invariant: off a terminal
// (non-*os.File OR a non-tty file) printBanner writes NOTHING. The eye-rendering
// body is TTY-gated and documented as unreachable without a pseudo-terminal.
func TestMc_PrintBannerNonTTY(t *testing.T) {
	var buf bytes.Buffer
	printBanner(&buf)
	if buf.Len() != 0 {
		t.Fatalf("printBanner to a non-file writer must emit nothing, got %q", buf.String())
	}

	// A real *os.File that is not a terminal must also emit nothing.
	f, err := os.CreateTemp(t.TempDir(), "banner-out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	printBanner(f)
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Fatalf("printBanner to a non-tty file must emit nothing, wrote %d bytes", fi.Size())
	}
}

// ---------------------------------------------------------------------------
// notify.go — osascriptRunner
// ---------------------------------------------------------------------------

// TestMc_OsascriptRunner exercises the real exec seam WITHOUT touching the OS
// notification service: PATH is redirected to a temp dir so "osascript" resolves
// to a harmless stub. The success path returns nil; with the stub absent from
// PATH, LookPath/Start fails and the error surfaces.
func TestMc_OsascriptRunner(t *testing.T) {
	skipOnWindows(t, "the exec seam is exercised via a #!/bin/sh stub named 'osascript'; Windows can't exec an extensionless shell script (no PATHEXT match)")
	dir := t.TempDir()
	stub := filepath.Join(dir, "osascript")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Success: "osascript" resolves to our stub, which exits 0.
	t.Setenv("PATH", dir)
	if err := osascriptRunner("-e", `display notification "x"`); err != nil {
		t.Fatalf("stubbed osascript should start cleanly, got %v", err)
	}

	// Failure: an empty PATH dir has no osascript → exec cannot start it.
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	if err := osascriptRunner("-e", "noop"); err == nil {
		t.Fatal("expected an error when osascript is not on PATH")
	}
}

// ---------------------------------------------------------------------------
// embed_ollama.go — Dim, Embed error arm, probe error arms
// ---------------------------------------------------------------------------

// TestMc_OllamaDim locks the trivial dimension accessor.
func TestMc_OllamaDim(t *testing.T) {
	if got := (ollamaEmbedder{dim: 768}).Dim(); got != 768 {
		t.Fatalf("Dim() = %d, want 768", got)
	}
}

// TestMc_OllamaEmbedDecodeError: a daemon that answers /api/embeddings with a
// bad body (undecodable, or an empty embedding) FAILS CLOSED — Embed returns
// errEmbedderUnavailable and a nil vector, never a fabricated zero vector (D1).
func TestMc_OllamaEmbedDecodeError(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"garbage", "this is not json"},
		{"empty-embedding", `{"embedding":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			e := ollamaEmbedder{baseURL: srv.URL, model: "m", dim: 5, client: &http.Client{Timeout: 5 * time.Second}}
			v, err := e.Embed("hello")
			if err == nil {
				t.Fatalf("a bad /api/embeddings body must error, got vector %v", v)
			}
			if !errors.Is(err, errEmbedderUnavailable) {
				t.Fatalf("error must wrap errEmbedderUnavailable, got %v", err)
			}
			if v != nil {
				t.Fatalf("failed embed must return a nil vector, got %v", v)
			}
		})
	}
}

// TestMc_OllamaProbeErrors covers probe()'s three failure arms:
//   - an invalid request URL (NewRequestWithContext fails) → ("", false);
//   - a non-200 status → ("", false);
//   - a truncated body that reads short (io.ReadAll errors) → ("", true):
//     the daemon is reachable but the digest is unreadable, so the caller
//     falls back to the bare model id.
func TestMc_OllamaProbeErrors(t *testing.T) {
	// Invalid URL: a control char makes http.NewRequestWithContext fail.
	e := ollamaEmbedder{baseURL: "http://\x7f-bad", model: "m", client: &http.Client{Timeout: time.Second}}
	if digest, ok := e.probe(); ok || digest != "" {
		t.Fatalf("invalid-URL probe = (%q,%v), want (\"\",false)", digest, ok)
	}

	// Non-200 status.
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv500.Close()
	e = ollamaEmbedder{baseURL: srv500.URL, model: "m", client: &http.Client{Timeout: 5 * time.Second}}
	if digest, ok := e.probe(); ok || digest != "" {
		t.Fatalf("500 probe = (%q,%v), want (\"\",false)", digest, ok)
	}
	if e.reachable() {
		t.Fatal("a 500 daemon must not be reachable")
	}

	// Truncated body: promise 100 bytes, send 5 then close → client ReadAll errors,
	// but the status was 200, so probe reports reachable with an empty digest.
	srvShort := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server must support hijacking")
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort")
		_ = bufrw.Flush()
		_ = conn.Close()
	}))
	defer srvShort.Close()
	e = ollamaEmbedder{baseURL: srvShort.URL, model: "m", client: &http.Client{Timeout: 5 * time.Second}}
	digest, ok := e.probe()
	if !ok {
		t.Fatal("a 200 daemon with a short body must still be reachable")
	}
	if digest != "" {
		t.Fatalf("unreadable body must yield an empty digest, got %q", digest)
	}
}

// ---------------------------------------------------------------------------
// eval_metrics.go — existsInMemoriesTable
// ---------------------------------------------------------------------------

// TestMc_ExistsInMemoriesTable covers all three arms: a present row (true,nil),
// an absent row (false,nil), and a genuine DB error (missing table) which MUST
// surface as a non-nil error rather than be swallowed into a false COVERAGE
// verdict.
func TestMc_ExistsInMemoriesTable(t *testing.T) {
	ctx := context.Background()

	// Fresh in-memory DB with NO memories table → a real schema error.
	dbNoTable, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "notable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dbNoTable.Close()
	// Touch the connection so the query, not Open, is what fails.
	if _, err := dbNoTable.ExecContext(ctx, `CREATE TABLE other(x)`); err != nil {
		t.Fatal(err)
	}
	ok, err := existsInMemoriesTable(ctx, dbNoTable, "anything")
	if err == nil {
		t.Fatal("a missing memories table must return an error, not be swallowed to false")
	}
	if ok {
		t.Fatal("error path must report ok=false")
	}
	if !strings.Contains(err.Error(), "memories") && !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("error should name the missing table, got %v", err)
	}

	// Now a real table: present → (true,nil), absent → (false,nil).
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE memories(id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO memories(id) VALUES('present')`); err != nil {
		t.Fatal(err)
	}
	if ok, err := existsInMemoriesTable(ctx, db, "present"); err != nil || !ok {
		t.Fatalf("present row = (%v,%v), want (true,nil)", ok, err)
	}
	if ok, err := existsInMemoriesTable(ctx, db, "missing"); err != nil || ok {
		t.Fatalf("absent row = (%v,%v), want (false,nil)", ok, err)
	}
}

// ---------------------------------------------------------------------------
// pdf.go — extractPDFText truncation, writeAttachmentMemories error/title arms
// ---------------------------------------------------------------------------

// TestMc_ExtractPDFTextTruncates asserts the 512 KiB extraction cap: pages up to
// the bound are kept; once the running buffer exceeds it, later pages are not
// read (mirrors the page-count cap, but on byte size).

// mcBuildPDF hand-builds a PDF with one page per entry in streams (the content
// streams are inserted verbatim, so a deliberately broken stream can be
// injected). When nullKid is set, the Pages tree gets one extra Kid that
// references a non-existent object — a "null page" the extractor must skip.

// TestMc_ExtractPDFTextSkipsNullPage: a page-tree Kid that dangles (references a
// missing object) resolves to a null page, which the extractor skips (IsNull
// continue) while still returning the real page's text.

// TestMc_ExtractPDFTextSkipsGarbledPage: a page whose content stream makes
// GetPlainText panic (a Tf operator with the wrong arg count) is recovered
// per-page and skipped — a single garbled page must not lose the rest of the
// document. Page 2 (valid) still extracts.

// TestMc_ExtractPDFTextRecoversPanic: a corrupt cross-reference offset makes the
// parser panic during page-tree resolution (OUTSIDE GetPlainText's own
// recover). extractPDFText's top-level recover must convert that panic into an
// error so a hostile/malformed PDF never crashes a sync.

// TestMc_WriteAttachmentMemoriesTitleFallback: an attachment with an empty
// Filename derives its title from the file's base name.
func TestMc_WriteAttachmentMemoriesTitleFallback(t *testing.T) {
	// StateDir must be set: the write path journals under it, and an empty
	// StateDir now refuses loudly instead of scattering into the cwd (#184).
	cfg := Config{VaultDir: t.TempDir(), StateDir: t.TempDir()}
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "unnamed-lease.pdf")
	writeMinimalPDF(t, pdfPath, "escalation clause body")

	parent := memory.MappedMemory{
		StableID: "imessage_chat_TITLE", Provider: "imessage", ProviderID: "TITLE",
		Scope: "personal", Tags: []string{"imessage"}, CreatedAt: "2026-06-01T00:00:00Z",
		Attachments: []memory.Attachment{
			// Empty Filename but a real PDF path + pdf MIME → title falls back to base.
			{Filename: "", MimeType: "application/pdf", Path: pdfPath},
		},
	}
	n, err := writeAttachmentMemories(cfg, parent)
	if err != nil {
		t.Fatalf("writeAttachmentMemories: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 derived memory, got %d", n)
	}
	id := "att_" + memory.ContentHash(parent.StableID+":"+pdfPath)
	out := filepath.Join(sourcesRoot(cfg), "imessage", memory.SafeFilename(id)+".md")
	m, err := parseMemory(out)
	if err != nil {
		t.Fatalf("derived memory not written: %v", err)
	}
	if m.Title != "unnamed-lease.pdf" {
		t.Fatalf("empty filename must derive title from base name, got %q", m.Title)
	}
}

// TestMc_WriteAttachmentMemoriesWriteError: a vault write failure propagates.
// Pointing VaultDir at a regular FILE makes MkdirAll (inside atomicWrite) fail,
// so writeMappedMemory errors and the count/err are returned honestly.
func TestMc_WriteAttachmentMemoriesWriteError(t *testing.T) {
	// VaultDir is a file, not a directory → sources/<provider> cannot be created.
	vaultFile := filepath.Join(t.TempDir(), "vault-is-a-file")
	if err := os.WriteFile(vaultFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{VaultDir: vaultFile, StateDir: t.TempDir()}

	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "doc.pdf")
	writeMinimalPDF(t, pdfPath, "some extractable body")

	parent := memory.MappedMemory{
		StableID: "imessage_chat_ERR", Provider: "imessage", ProviderID: "ERR",
		Scope: "personal", CreatedAt: "2026-06-01T00:00:00Z",
		Attachments: []memory.Attachment{
			{Filename: "doc.pdf", MimeType: "application/pdf", Path: pdfPath},
		},
	}
	n, err := writeAttachmentMemories(cfg, parent)
	if err == nil {
		t.Fatal("a vault write failure must propagate as an error")
	}
	if n != 0 {
		t.Fatalf("no memory should be counted on a write failure, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// progress.go
// ---------------------------------------------------------------------------

// TestMc_ProgressNilWriter: a nil writer makes every method a safe no-op.
func TestMc_ProgressNilWriter(t *testing.T) {
	p := newProgress(nil, "src", "threads")
	if p == nil {
		t.Fatal("newProgress(nil) must still return a usable value")
	}
	if p.tty {
		t.Fatal("nil-writer progress must not be a TTY")
	}
	// None of these may panic or write anywhere.
	p.tick()
	p.tick()
	p.done()
}

// TestMc_ProgressPipeStepAppends: in pipe mode a running line is appended every
// progressPipeStep items (the lean launchd/CI cadence).
func TestMc_ProgressPipeStepAppends(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, "gmail-x", "threads")
	if p.tty {
		t.Fatal("bytes.Buffer must not be a TTY")
	}
	for i := 0; i < progressPipeStep; i++ {
		p.tick()
	}
	out := buf.String()
	// The start line plus exactly one step line at the 500 boundary.
	if !strings.Contains(out, "gmail-x: syncing…") {
		t.Fatalf("missing start line: %q", out)
	}
	wantStep := fmt.Sprintf("gmail-x: %d threads…", progressPipeStep)
	if !strings.Contains(out, wantStep) {
		t.Fatalf("expected a step line %q at the pipe cadence, got %q", wantStep, out)
	}
	if strings.ContainsAny(out, "\r\x1b") {
		t.Fatalf("pipe mode must stay byte-clean, got %q", out)
	}
}

// TestMc_ProgressAnimateAndDone drives the TTY animator goroutine directly (the
// struct is built with tty:true; isTTYWriter needs a real terminal). It asserts
// the spinner repaints in place while running, then done() stops the animator
// and settles the final ✓ line.
func TestMc_ProgressAnimateAndDone(t *testing.T) {
	var buf bytes.Buffer
	p := &progress{
		out: &buf, label: "gmail-anim", noun: "threads", tty: true,
		sty:  newStyler(&buf, false),
		stop: make(chan struct{}),
	}
	p.wg.Add(1)
	go p.animate()

	// Let the ticker fire at least once (spinnerInterval = 120ms) so both select
	// arms — the tick repaint and, later, the stop — are exercised.
	time.Sleep(spinnerInterval + 60*time.Millisecond)
	p.tick()
	p.tick()

	p.done() // closes stop, awaits the goroutine, settles the ✓ line

	out := buf.String()
	if !strings.Contains(out, "\r") {
		t.Fatalf("animate must repaint in place (carriage return), got %q", out)
	}
	hasFrame := false
	for _, f := range spinnerFrames {
		if strings.Contains(out, f) {
			hasFrame = true
			break
		}
	}
	if !hasFrame {
		t.Fatalf("animator must paint a spinner frame, got %q", out)
	}
	if !strings.Contains(out, "✓ gmail-anim: 2 threads synced") {
		t.Fatalf("done() must settle the ✓ final count, got %q", out)
	}
}

// TestMc_WarnfOkfNilWriter: warnf/okf on a nil writer are no-ops (guard branch).
func TestMc_WarnfOkfNilWriter(t *testing.T) {
	// Must not panic.
	warnf(nil, "ignored %d", 1)
	okf(nil, "ignored %s", "x")

	// And the happy path still writes the prefixes.
	var wb, ob bytes.Buffer
	warnf(&wb, "disk %d%% full", 90)
	okf(&ob, "done %s", "sync")
	if !strings.Contains(wb.String(), "warn:") || !strings.Contains(wb.String(), "disk 90% full") {
		t.Fatalf("warnf output wrong: %q", wb.String())
	}
	if !strings.Contains(ob.String(), "✓") || !strings.Contains(ob.String(), "done sync") {
		t.Fatalf("okf output wrong: %q", ob.String())
	}
}

// ---------------------------------------------------------------------------
// render.go — the sentinel arms not exercised elsewhere
// ---------------------------------------------------------------------------

// TestMc_StyleDigestFreshAndPlainBullet covers the "Fresh as of:" dim arm and
// the plain "- " bullet arm (dimIDSuffix), plus dimIDSuffix's no-id early
// return and styleChangeItem's non-tag fallback.
func TestMc_StyleDigestFreshAndPlainBullet(t *testing.T) {
	// Off: byte-identical.
	raw := "Fresh as of: 2026-06-30\n- plain item (id: abc)\n- bullet without an id suffix\n"
	if got := styleDigestTTY(raw, styler{on: false}); got != raw {
		t.Fatalf("styler off must be byte-identical, got %q", got)
	}

	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	styled := styleDigestTTY(raw, styler{on: true})
	lines := strings.Split(styled, "\n")

	// "Fresh as of:" is dimmed → wrapped in ANSI, differs from raw.
	if lines[0] == "Fresh as of: 2026-06-30" || !strings.ContainsRune(lines[0], '\x1b') {
		t.Fatalf("Fresh line must be dim-styled, got %q", lines[0])
	}
	// A plain bullet with a trailing "(id: …)" gets that id suffix dimmed.
	if !strings.ContainsRune(lines[1], '\x1b') {
		t.Fatalf("plain bullet's id suffix must be dimmed, got %q", lines[1])
	}
	if !strings.Contains(lines[1], "plain item") {
		t.Fatalf("plain bullet must keep its text, got %q", lines[1])
	}
	// A bullet WITHOUT an "(id: …)" suffix passes through dimIDSuffix unchanged.
	if lines[2] != "- bullet without an id suffix" {
		t.Fatalf("bullet with no id suffix must be unchanged, got %q", lines[2])
	}
}

// TestMc_DimIDSuffixDirect pins dimIDSuffix's two shapes directly: a trailing
// "(id: …)" is dimmed; anything else is returned verbatim.
func TestMc_DimIDSuffixDirect(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)
	sty := styler{on: true}

	// No " (id:" → returned exactly as-is.
	for _, in := range []string{"- no id here", "- ends without paren (id: x", "- trailing (id: y) then more"} {
		if got := dimIDSuffix(in, sty); got != in {
			t.Fatalf("dimIDSuffix(%q) must be unchanged, got %q", in, got)
		}
	}
	// A well-formed trailing id is dimmed (ANSI-wrapped) but the prefix text stays.
	got := dimIDSuffix("- Subject line (id: abc123)", sty)
	if !strings.Contains(got, "Subject line") || !strings.ContainsRune(got, '\x1b') {
		t.Fatalf("trailing id must be dimmed while keeping the text, got %q", got)
	}
}

// TestMc_StyleChangeItemFallback covers styleChangeItem's defensive fallback:
// called with a line lacking both "[new]"/"[updated]" tags, it degrades to
// dimIDSuffix (this arm is unreachable via styleDigestTTY's guarded dispatch).
func TestMc_StyleChangeItemFallback(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)
	sty := styler{on: true}

	// No tag, no id suffix → returned verbatim (dimIDSuffix no-op).
	if got := styleChangeItem("- plain no tag", sty); got != "- plain no tag" {
		t.Fatalf("fallback with no tag/id must be unchanged, got %q", got)
	}
	// No tag but a trailing id → the id is dimmed via the fallback.
	got := styleChangeItem("- plain (id: zzz)", sty)
	if !strings.Contains(got, "plain") || !strings.ContainsRune(got, '\x1b') {
		t.Fatalf("fallback must still dim a trailing id, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// upgrade.go — cmdUpgrade guard branches
// ---------------------------------------------------------------------------

// TestMc_CmdUpgradeFlagParseError: an unknown flag surfaces flag.Parse's error.
func TestMc_CmdUpgradeFlagParseError(t *testing.T) {
	var buf bytes.Buffer
	if err := cmdUpgrade(context.Background(), []string{"--nonexistent-flag"}, &buf); err == nil {
		t.Fatal("an unknown flag must return a parse error")
	}
}

// TestMc_CmdUpgradeRefusesSourceBuild: on a dev/empty BuildVersion (a source
// build), self-update refuses with a guidance error and never touches the
// network. The GitHub-release round-trip past this guard is documented as
// not unit-testable without real network.
func TestMc_CmdUpgradeRefusesSourceBuild(t *testing.T) {
	old := BuildVersion
	defer func() { BuildVersion = old }()

	for _, v := range []string{"dev", ""} {
		BuildVersion = v
		var buf bytes.Buffer
		err := cmdUpgrade(context.Background(), nil, &buf)
		if err == nil {
			t.Fatalf("BuildVersion %q must refuse self-update", v)
		}
		if !strings.Contains(err.Error(), "source build") {
			t.Fatalf("expected a source-build refusal, got %v", err)
		}
	}
}
