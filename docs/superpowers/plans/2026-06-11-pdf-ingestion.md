# PDF Ingestion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extract text from PDFs in filesystem sources and iMessage attachments, each PDF becoming its own searchable memory.

**Architecture:** A shared `extractPDFText` in `internal/mora` (sibling of `extractDocxText`) using pinned `ledongthuc/pdf`, recover-wrapped with size/page caps. Filesystem ingest dispatches by extension; the iMessage connector threads the on-disk attachment path through a new `memory.Attachment.Path` field (still never opening the file), and the wiring boundary writes a derived memory per PDF.

**Tech Stack:** Go 1.25, `github.com/ledongthuc/pdf` pinned at `v0.0.0-20250511090121-5959a4027728`.

**Spec:** `docs/superpowers/specs/2026-06-11-pdf-ingestion-design.md`

## Supply-chain audit record (completed 2026-06-11, pre-adoption)

Audited `ledongthuc/pdf@5959a4027728` (2025-05-11) from the module cache:

| Check | Result |
|---|---|
| License | BSD-3-Clause (Go Authors — rsc.io/pdf lineage) |
| go.mod requires | **none** — module has zero dependencies |
| Transitive imports | Go stdlib only (`syscall`/`unsafe` reached only via stdlib `os`/`fmt`, never imported by the lib's own source) |
| Network / exec / env / file writes | none — `grep` clean for `net`, `os/exec`, `os.Getenv`, `os.Create`/`WriteFile`/`Remove` |
| Size | ~7,400 LOC across 7 source files — auditable |
| Checksum pin | `h1:QwWKgMY28TAXaDl+ExRDqGQltzXqN/xypdKP86niVn8=` (go.sum enforces) |

Residual risk: the library panics on malformed input by design and allocates from PDF-controlled metadata. Mitigation: `recover` wrapper + 20 MiB file cap + 500-page cap + the existing 512 KiB index bound. `recover` cannot contain a runtime OOM — accepted for local, user-owned files (this is not a hostile-input service).

**IMSG-07 amendment (conscious spec change):** IMSG-07's user-facing guarantee — attachment bytes and on-disk paths never appear in rendered vault output — is **unchanged**. What changes: the in-transit `Attachment` struct now carries `Path` so the wiring boundary (not the connector) can extract PDF bodies. Comments citing IMSG-07 are updated to say exactly that.

---

### Task 1: Pinned dependency + `extractPDFText`

**Files:**
- Create: `internal/mora/pdf.go`
- Create: `internal/mora/pdf_test.go`
- Modify: `go.mod` / `go.sum`

- [ ] **Step 1: Add the pinned dependency**

```bash
cd /Users/adit/Pyranthus/products/mora
go get github.com/ledongthuc/pdf@v0.0.0-20250511090121-5959a4027728
```

Verify `go.sum` contains `h1:QwWKgMY28TAXaDl+ExRDqGQltzXqN/xypdKP86niVn8=`.
**Do NOT run `go mod tidy`** before the import lands (CLAUDE.md gotcha: tidy prunes unimported requires).

- [ ] **Step 2: Write the failing tests**

Create `internal/mora/pdf_test.go`:

```go
package mora

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMinimalPDF hand-builds a tiny valid PDF (uncompressed content streams,
// Helvetica, correct xref offsets) with one page per entry in pageTexts. Texts
// must not contain parentheses or backslashes.
func writeMinimalPDF(t *testing.T, path string, pageTexts ...string) {
	t.Helper()
	n := len(pageTexts)
	// Objects: 1 Catalog, 2 Pages, 3 Font, 4..3+n Page, 4+n..3+2n Contents.
	objs := make([]string, 0, 3+2*n)
	kids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		kids = append(kids, fmt.Sprintf("%d 0 R", 4+i))
	}
	objs = append(objs, "<< /Type /Catalog /Pages 2 0 R >>")
	objs = append(objs, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	objs = append(objs, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i := 0; i < n; i++ {
		objs = append(objs, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>",
			4+n+i))
	}
	for i, text := range pageTexts {
		stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
		_ = i
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1) // 1-indexed
	for i, o := range objs {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractPDFText(t *testing.T) {
	p := filepath.Join(t.TempDir(), "doc.pdf")
	writeMinimalPDF(t, p, "alpha lease agreement", "bravo renewal terms")
	got, err := extractPDFText(p)
	if err != nil {
		t.Fatalf("extractPDFText: %v", err)
	}
	if !strings.Contains(got, "alpha lease agreement") || !strings.Contains(got, "bravo renewal terms") {
		t.Fatalf("expected both pages' text, got %q", got)
	}
}

func TestExtractPDFTextMalformedIsErrorNotPanic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.pdf")
	if err := os.WriteFile(p, []byte("%PDF-1.4 this is not a real pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractPDFText(p); err == nil {
		t.Fatal("expected an error for a malformed pdf, got nil")
	}
	// Reaching here at all proves no panic escaped.
}

func TestExtractPDFTextOversizedIsError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.pdf")
	writeMinimalPDF(t, p, "small but capped")
	old := pdfMaxFileSize
	pdfMaxFileSize = 8 // bytes — anything real exceeds this
	defer func() { pdfMaxFileSize = old }()
	if _, err := extractPDFText(p); err == nil {
		t.Fatal("expected an error past the size cap, got nil")
	}
}

func TestExtractPDFTextPageCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "many.pdf")
	writeMinimalPDF(t, p, "page one text", "page two text", "page three text")
	old := pdfMaxPages
	pdfMaxPages = 2
	defer func() { pdfMaxPages = old }()
	got, err := extractPDFText(p)
	if err != nil {
		t.Fatalf("extractPDFText: %v", err)
	}
	if !strings.Contains(got, "page one text") || !strings.Contains(got, "page two text") {
		t.Fatalf("expected first two pages, got %q", got)
	}
	if strings.Contains(got, "page three text") {
		t.Fatalf("page cap not enforced: %q", got)
	}
}

func TestExtractPDFTextEmptyIsNotError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.pdf")
	writeMinimalPDF(t, p) // zero pages — extracts to nothing, like a scanned PDF
	got, err := extractPDFText(p)
	if err != nil {
		t.Fatalf("empty pdf must not error (callers skip on empty): %v", err)
	}
	if strings.TrimSpace(got) != "" {
		t.Fatalf("expected empty text, got %q", got)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/mora/ -run TestExtractPDF -v`
Expected: FAIL — `undefined: extractPDFText`, `undefined: pdfMaxFileSize`, `undefined: pdfMaxPages`

- [ ] **Step 4: Implement the extractor**

Create `internal/mora/pdf.go`:

```go
package mora

import (
	"fmt"
	"os"
	"strings"

	"github.com/ledongthuc/pdf"
)

// PDF extraction caps. Package vars (not consts) so tests can lower them.
// pdfMaxFileSize rejects the file before parsing (a hostile PDF can demand large
// allocations from tiny inputs, so the parse itself is also recover-wrapped).
// pdfMaxPages bounds work on pathological page counts; extraction truncates to
// the first pdfMaxPages pages. Extracted text additionally obeys the existing
// 512 KiB index bound at every call site (over → the whole file is skipped,
// exactly like the .docx and raw-read paths).
var (
	pdfMaxFileSize int64 = 20 << 20 // 20 MiB
	pdfMaxPages          = 500
)

// extractPDFText returns the plain text of a PDF using the pinned, audited
// ledongthuc/pdf (see docs/superpowers/plans/2026-06-11-pdf-ingestion.md for the
// supply-chain record). The library panics on malformed input by design, so the
// whole parse is recover-wrapped: any panic becomes an error and the caller skips
// the file — a bad PDF must never crash a sync. Scanned/image-only PDFs extract
// to "" with a nil error; callers skip on empty (never index garbage). No OCR —
// that would break the no-CGO/single-binary constraint.
func extractPDFText(path string) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, err = "", fmt.Errorf("pdf parse panicked (malformed input): %v", r)
		}
	}()
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.Size() > pdfMaxFileSize {
		return "", fmt.Errorf("pdf too large: %d bytes (cap %d)", fi.Size(), pdfMaxFileSize)
	}
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var b strings.Builder
	n := r.NumPage()
	if n > pdfMaxPages {
		n = pdfMaxPages
	}
	for i := 1; i <= n; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		fonts := make(map[string]*pdf.Font)
		for _, name := range p.Fonts() {
			font := p.Font(name)
			fonts[name] = &font
		}
		s, perr := p.GetPlainText(fonts)
		if perr != nil {
			continue // one garbled page must not lose the rest of the document
		}
		b.WriteString(s)
		b.WriteString("\n")
		if b.Len() > 512*1024 {
			break // already past the index bound — the caller will skip the file
		}
	}
	return strings.TrimSpace(b.String()), nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/mora/ -run TestExtractPDF -v`
Expected: PASS (all 5). If `TestExtractPDFText` fails on text content, the fixture is the suspect — ledongthuc needs a well-formed xref; debug `writeMinimalPDF` offsets first, not the extractor.

- [ ] **Step 6: Vet + commit**

```bash
go vet ./... && gofmt -l . | (! grep .)
git add go.mod go.sum internal/mora/pdf.go internal/mora/pdf_test.go
git commit -m "feat: pure-Go PDF text extraction (pinned ledongthuc/pdf, recover-wrapped, capped)"
```

(No AI co-author trailers — standing rule.)

---

### Task 2: Filesystem `.pdf` ingestion

**Files:**
- Modify: `internal/mora/mora.go` (`curatedExtractExt` ~line 4548; extraction dispatch ~line 3562; PDF-exclusion comment ~line 4543)
- Modify: `internal/mora/pdf_test.go` (append)

- [ ] **Step 1: Write the failing test** (append to `internal/mora/pdf_test.go`)

```go
func TestIngestFilesystemPDF(t *testing.T) {
	dir := t.TempDir()
	writeMinimalPDF(t, filepath.Join(dir, "contract.pdf"), "signed widget contract clause")
	if err := os.WriteFile(filepath.Join(dir, "junk.pdf"), []byte("not a pdf at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t) // same helper the docx filesystem tests use
	s := fsSource("fsdocs", dir, "personal")
	n, err := ingestFilesystem(cfg, s)
	if err != nil {
		t.Fatalf("ingestFilesystem: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly the valid pdf ingested (junk skipped), got %d", n)
	}
	mems := readAllSourceMemories(t, cfg) // same helper the docx tests use
	found := false
	for _, m := range mems {
		if strings.Contains(m.Text, "signed widget contract clause") {
			found = true
		}
	}
	if !found {
		t.Fatal("extracted pdf text not found in ingested memories")
	}
}
```

(If `testConfig` / `readAllSourceMemories` are named differently in `mora_docx_fs_test.go`, reuse those exact helpers — do not invent new ones.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mora/ -run TestIngestFilesystemPDF -v`
Expected: FAIL — `n == 0` (`.pdf` not in `curatedExtractExt`)

- [ ] **Step 3: Implement**

In `mora.go`, replace `curatedExtractExt` and its stale comment:

```go
// curatedExtractExt reports whether ext is a non-plain-text format Mora ingests by
// EXTRACTING its text (vs reading raw bytes). Today: .docx (stdlib zip+xml) and
// .pdf (pinned ledongthuc/pdf — pure Go, recover-wrapped, capped; see pdf.go).
// PDF extraction is lossy on exotic font encodings and yields nothing on scanned
// documents (no OCR — that would break the no-CGO/single-binary constraint); such
// files are skipped, never indexed as garbage.
func curatedExtractExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".docx", ".pdf":
		return true
	default:
		return false
	}
}
```

In `ingestFilesystem`, replace the extraction branch:

```go
		if curatedExtractExt(ext) {
			// Non-plain-text (.docx/.pdf): extract the words rather than index raw bytes.
			var t string
			var derr error
			switch strings.ToLower(ext) {
			case ".pdf":
				t, derr = extractPDFText(path)
			default:
				t, derr = extractDocxText(path)
			}
			if derr != nil || t == "" {
				return nil // unreadable/empty/oversized — skip, never index garbage.
			}
			text = t
		} else {
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/mora/ -run 'TestIngestFilesystemPDF|TestExtractDocx|TestIngestFilesystem' -v`
Expected: PASS (new test + all existing docx/filesystem tests untouched)

- [ ] **Step 5: Commit**

```bash
git add internal/mora/mora.go internal/mora/pdf_test.go
git commit -m "feat: ingest .pdf files from filesystem sources via text extraction"
```

---

### Task 3: `Attachment.Path` threaded through the iMessage connector

**Files:**
- Modify: `internal/memory/types.go` (Attachment struct, ~line 17)
- Modify: `internal/imessage/chatdb.go` (attachment scan ~line 421; `baseName` comment ~line 464)
- Modify: `internal/imessage/map.go` (comment ~line 45)
- Test: `internal/imessage/chatdb_seed_test.go` (extend existing seed harness)

- [ ] **Step 1: Write the failing test** (append to `chatdb_seed_test.go`, using its existing seed helpers — seed an attachment row whose `filename` is `~/Library/Messages/Attachments/ab/cd/doc.pdf` with mime `application/pdf`)

```go
func TestAttachmentPathThreadedThrough(t *testing.T) {
	// Seed one conversation with one message carrying a PDF attachment whose
	// chat.db filename column holds a tilde-prefixed on-disk path, per the real
	// schema. Use the existing seed helpers in this file.
	db := seedChatDB(t /* ..., attachment filename: "~/Library/Messages/Attachments/ab/cd/doc.pdf", mime: "application/pdf" ... */)
	f := openSeededFetcher(t, db)
	convs := fetchAllConversations(t, f)
	att := firstAttachment(t, convs)
	if att.Filename != "doc.pdf" {
		t.Fatalf("Filename must stay the base name (rendered output is path-free): %q", att.Filename)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "Library/Messages/Attachments/ab/cd/doc.pdf")
	if att.Path != want {
		t.Fatalf("Path must carry the expanded on-disk location: got %q want %q", att.Path, want)
	}
}
```

(The seed/fetch helper names above are placeholders for whatever `chatdb_seed_test.go` actually defines — read that file first and use its real helpers; the assertions are the contract.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/imessage/ -run TestAttachmentPathThreadedThrough -v`
Expected: FAIL — `att.Path` undefined / empty

- [ ] **Step 3: Implement**

`internal/memory/types.go` — amend the struct and its comment:

```go
// Attachment is metadata-plus-location: filename/MIME/size, and — when the body
// already exists on local disk (iMessage) — the absolute Path to it. Connectors
// never open the file; Path is consumed at the wiring boundary (internal/mora)
// to extract text from supported formats (PDF). Bytes are never carried here,
// and neither Path nor bytes ever appear in rendered vault output (IMSG-07's
// user-facing guarantee is unchanged).
type Attachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
	Path     string `json:"path,omitempty"`
}
```

`internal/imessage/chatdb.go` — in the attachment scan (~line 421):

```go
		if attFile.Valid && attFile.String != "" {
			att := Attachment{Filename: baseName(attFile.String), Path: expandHome(attFile.String)}
```

and add next to `baseName` (updating baseName's comment to drop "never the on-disk path", citing the amendment instead):

```go
// expandHome resolves the leading "~" chat.db stores in attachment paths to the
// real home directory, yielding an absolute path the wiring boundary can read.
// The connector itself never opens the file (no-bytes invariant intact); a path
// that doesn't start with "~/" is returned as-is.
func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
```

(Add `os` and `path/filepath` to chatdb.go imports if absent.)

`internal/imessage/map.go` ~line 45 — update the defensive-copy comment: attachments carry filename/MIME/size **plus the on-disk Path for the wiring boundary**; bytes never; rendered output still path-free.

- [ ] **Step 4: Run the package's full tests**

Run: `go test -race ./internal/imessage/ ./internal/memory/ -v 2>&1 | tail -20`
Expected: PASS, including the existing render tests proving no path leaks into rendered bodies. If an existing test asserts `Path == ""` or forbids the field, update it to the amended contract (assert instead that *rendered output* contains no path).

- [ ] **Step 5: Commit**

```bash
git add internal/memory/types.go internal/imessage/chatdb.go internal/imessage/map.go internal/imessage/chatdb_seed_test.go
git commit -m "feat: thread iMessage attachment on-disk path through Attachment.Path (IMSG-07 amended: rendered output stays path-free)"
```

---

### Task 4: Derived memory per PDF at the wiring boundary

**Files:**
- Modify: `internal/mora/pdf.go` (add helper)
- Modify: `internal/mora/mora.go` (`ingestIMessage` write closure, ~line 3372)
- Modify: `internal/mora/pdf_test.go` (append)

- [ ] **Step 1: Write the failing tests** (append to `internal/mora/pdf_test.go`)

```go
func TestWriteAttachmentMemories(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "lease.pdf")
	writeMinimalPDF(t, pdfPath, "annual rent escalation clause")

	parent := memory.MappedMemory{
		StableID: "imessage_chat_ABC", Provider: "imessage", ProviderID: "ABC",
		Scope: "personal", Tags: []string{"imessage"},
		CreatedAt: "2026-06-01T00:00:00Z",
		Attachments: []memory.Attachment{
			{Filename: "lease.pdf", MimeType: "application/pdf", Path: pdfPath},
			{Filename: "photo.heic", MimeType: "image/heic", Path: filepath.Join(dir, "photo.heic")}, // non-PDF: ignored
			{Filename: "gone.pdf", MimeType: "application/pdf", Path: filepath.Join(dir, "missing.pdf")}, // missing file: skipped
		},
	}
	n, err := writeAttachmentMemories(cfg, parent)
	if err != nil {
		t.Fatalf("writeAttachmentMemories: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 derived memory, got %d", n)
	}

	id := "att_" + memory.ContentHash(parent.StableID+":"+pdfPath)
	out := filepath.Join(sourcesRoot(cfg), "imessage", memory.SafeFilename(id)+".md")
	m, err := parseMemory(out)
	if err != nil {
		t.Fatalf("derived memory not written at %s: %v", out, err)
	}
	if m.Title != "lease.pdf" || !strings.Contains(m.Text, "annual rent escalation clause") {
		t.Fatalf("derived memory wrong shape: title=%q", m.Title)
	}
	if m.CreatedAt != parent.CreatedAt {
		t.Fatalf("created_at must inherit the parent's: %q", m.CreatedAt)
	}
	hasTag := false
	for _, tag := range m.Tags {
		if tag == "attachment" {
			hasTag = true
		}
	}
	if !hasTag {
		t.Fatalf("derived memory must carry the attachment tag: %v", m.Tags)
	}

	// Idempotent: re-running with an unchanged file rewrites nothing.
	before, _ := os.Stat(out)
	if _, err := writeAttachmentMemories(cfg, parent); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(out)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("unchanged pdf must content-hash-skip the rewrite")
	}
}
```

(Add `"github.com/pyranthus-hq/mora/internal/memory"` to the test file's imports.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mora/ -run TestWriteAttachmentMemories -v`
Expected: FAIL — `undefined: writeAttachmentMemories`

- [ ] **Step 3: Implement** (append to `internal/mora/pdf.go`)

```go
// isPDFAttachment gates extraction on MIME or extension — chat.db rows sometimes
// carry one without the other.
func isPDFAttachment(a memory.Attachment) bool {
	if strings.EqualFold(a.MimeType, "application/pdf") {
		return true
	}
	return strings.EqualFold(filepath.Ext(a.Filename), ".pdf")
}

// writeAttachmentMemories writes one derived memory per extractable PDF attachment
// of parent (design: docs/superpowers/specs/2026-06-11-pdf-ingestion-design.md).
// Every extraction failure — missing file, malformed, encrypted, empty/scanned,
// past the caps — skips that attachment and keeps the metadata on the parent; a
// body we can't read is not a sync error. Only a vault write failure propagates.
// The stable ID hashes parent StableID + path, so re-syncs hit the content-hash
// skip in writeMappedMemory and an unchanged PDF is a no-op.
func writeAttachmentMemories(cfg Config, parent memory.MappedMemory) (int, error) {
	count := 0
	for _, a := range parent.Attachments {
		if a.Path == "" || !isPDFAttachment(a) {
			continue
		}
		text, err := extractPDFText(a.Path)
		if err != nil || text == "" || len(text) > 512*1024 {
			continue
		}
		title := a.Filename
		if title == "" {
			title = filepath.Base(a.Path)
		}
		mm := memory.MappedMemory{
			StableID:    "att_" + memory.ContentHash(parent.StableID+":"+a.Path),
			Type:        "source",
			Title:       title,
			Body:        text,
			Tags:        append(append([]string{}, parent.Tags...), "attachment"),
			Source:      a.Path,
			Provider:    parent.Provider,
			ProviderID:  parent.ProviderID,
			Account:     parent.Account,
			Scope:       parent.Scope,
			CreatedAt:   parent.CreatedAt,
			ContentHash: memory.ContentHash(title, text),
		}
		if err := writeMappedMemory(cfg, mm); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
```

(Add `"path/filepath"` and the `memory` import to pdf.go.)

- [ ] **Step 4: Wire into `ingestIMessage`** — in its `write` closure (mora.go ~line 3372):

```go
	write := func(mm memory.MappedMemory) error {
		if err := writeMappedMemory(cfg, mm); err != nil {
			return err
		}
		if _, err := writeAttachmentMemories(cfg, mm); err != nil {
			return err
		}
		prog.tick()
		return nil
	}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./internal/mora/ -run 'TestWriteAttachmentMemories|TestExtractPDF|TestIngestFilesystemPDF' -v`
Expected: PASS. Then the budget gate: `go test ./internal/mora/ -run TestMCPBudget -v` — derived memories are ordinary memories, so T0 must stay green.

- [ ] **Step 6: Commit**

```bash
git add internal/mora/pdf.go internal/mora/mora.go internal/mora/pdf_test.go
git commit -m "feat: derive a searchable memory per iMessage PDF attachment at the wiring boundary"
```

---

### Task 5: Docs, full gates, live verify, board

**Files:**
- Modify: `docs/architecture/05-connectors-imessage.md` (IMSG-07 paragraph, ~line 154)
- Modify: `docs/architecture/01-data-model-and-storage.md` (Attachment struct + derived memories)
- Modify: `README.md` (sources/limits section)

- [ ] **Step 1: Update architecture docs** (keep-reflective standing rule)
  - `05-connectors-imessage.md`: amend the IMSG-07 paragraph — rendered output remains path-free; the in-transit `Attachment.Path` feeds wiring-boundary PDF extraction; cite `chatdb.go` `expandHome` and `mora` `writeAttachmentMemories` with real line numbers from the final code.
  - `01-data-model-and-storage.md`: document `Attachment.Path`, the `att_` stable-ID scheme, derived-memory shape (type `source`, `attachment` tag, parent's provider/created_at), and the caps (20 MiB / 500 pages / 512 KiB).

- [ ] **Step 2: README** — one line in the sources/capabilities area, framed per the docs-honesty rule (capability + workaround, not a ceiling): PDFs in filesystem sources and iMessage attachments are text-extracted and indexed; scanned/image-only PDFs are skipped (no OCR — they index nothing rather than garbage).

- [ ] **Step 3: Full gates**

```bash
go build -o mora ./cmd/mora && go vet ./... && gofmt -l . | (! grep .) && go test -race ./...
```

Expected: all green.

- [ ] **Step 4: Codex review of the full diff** (house loop)

```bash
codex review
```

Address real findings; re-run gates.

- [ ] **Step 5: Live verify on the real vault** (Adit's machine)
  - `./mora ingest --source <imessage-source>` then `./mora index rebuild`
  - `./mora search "<a phrase from a known PDF someone texted>" --json` — derived memory surfaces.
  - Confirm a conversation with a non-PDF attachment ingests unchanged.

- [ ] **Step 6: Commit docs, reconcile board**

```bash
git add docs/architecture/05-connectors-imessage.md docs/architecture/01-data-model-and-storage.md README.md
git commit -m "docs: PDF ingestion (filesystem + iMessage attachments), IMSG-07 amendment"
```

Board #1 (standing rule): add a `[Mora]` card for PDF ingestion if absent and set it Done when merged; add a Backlog card "[Mora] Gmail attachment download → Path seam" for the deferred Gmail pass.

---

## Self-review notes

- Spec coverage: extractor+caps (Task 1), filesystem (Task 2), Path threading + IMSG-07 amendment (Task 3), derived memories + idempotency + missing-file skip (Task 4), docs/README/board (Task 5). Gmail deferral needs no code — the `Path` seam (Task 3) is the deliverable.
- Helper names in Task 3's test are explicitly marked as placeholders for the real seed-harness helpers; the assertions are the contract.
- `writeAttachmentMemories` is referenced only after its definition task; types match `memory.MappedMemory` fields verified against `mapped.go`.
