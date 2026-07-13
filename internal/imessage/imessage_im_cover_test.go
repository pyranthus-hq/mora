package imessage

// Coverage-focused tests for internal/imessage. Every test here asserts on real
// behavior/output/error — never a bare call to paint a line green.
//
// Merge-safety: every test is named TestIm_*, every top-level helper is im-prefixed,
// and this file introduces a self-contained fake database/sql driver (imFake*) used
// to exercise the query/scan/rows-iteration error branches that a real modernc sqlite
// DB cannot deterministically produce (PRAGMA scan errors, mid-iteration Next errors).

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Shared helpers (im-prefixed) — real-sqlite fixture builders + a fake driver.
// ---------------------------------------------------------------------------

// imMakeDB creates a real sqlite file at a temp path, execs the given statements,
// and returns the path. Used to build structurally-defective chat.db fixtures
// (missing tables/columns, untyped columns) that seedChatDB's fixed schema cannot.
func imMakeDB(t *testing.T, stmts ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
	return path
}

// imSeedRawAddressBook creates a Sources/<uuid>/AddressBook-v22.abcddb with an
// arbitrary schema (via raw statements) and returns the Sources root for NewResolver.
// Used to force the AddressBook scan/skip branches that the typed seedAddressBook
// helper cannot (untyped ZOWNER/Z_PK columns, NULL owners, empty handle values).
func imSeedRawAddressBook(t *testing.T, stmts ...string) string {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "IMSRC")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(src, addressBookFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
	return root
}

// imClosedDB returns a real sqlite *sql.DB that has been Ping'd (proving it was
// usable) and then Closed — so any subsequent Query/QueryRow returns
// "sql: database is closed", exercising the query-error branches.
func imClosedDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "closed.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return db
}

// --- fake database/sql driver (imFake*) --------------------------------------

const imFakeDriverName = "im_cover_fake"

// imFakeBehavior programs a single opened fake DB. rows are returned for every
// query; nextErr (when set) is returned by Rows.Next AFTER all rows are emitted so
// the caller's rows.Err() observes a mid-iteration failure. A row whose values do
// not convert to the caller's Scan targets forces a scan error.
type imFakeBehavior struct {
	cols    []string
	rows    [][]driver.Value
	nextErr error
}

var (
	imFakeOnce     sync.Once
	imFakeMu       sync.Mutex
	imFakeRegistry = map[string]*imFakeBehavior{}
	imFakeSeq      int
)

// imOpenFakeDB registers behavior under a unique DSN and opens a *sql.DB backed by
// the fake driver.
func imOpenFakeDB(t *testing.T, b *imFakeBehavior) *sql.DB {
	t.Helper()
	imFakeOnce.Do(func() { sql.Register(imFakeDriverName, imFakeDriver{}) })
	imFakeMu.Lock()
	imFakeSeq++
	key := "k" + strconv.Itoa(imFakeSeq)
	imFakeRegistry[key] = b
	imFakeMu.Unlock()
	db, err := sql.Open(imFakeDriverName, key)
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

type imFakeDriver struct{}

func (imFakeDriver) Open(name string) (driver.Conn, error) {
	imFakeMu.Lock()
	b := imFakeRegistry[name]
	imFakeMu.Unlock()
	if b == nil {
		b = &imFakeBehavior{}
	}
	return &imFakeConn{b: b}, nil
}

type imFakeConn struct{ b *imFakeBehavior }

func (c *imFakeConn) Prepare(query string) (driver.Stmt, error) { return &imFakeStmt{b: c.b}, nil }
func (c *imFakeConn) Close() error                              { return nil }
func (c *imFakeConn) Begin() (driver.Tx, error)                 { return nil, errors.New("im fake: no tx") }

// Query implements driver.Queryer so database/sql routes queries here directly
// (bypassing statement arg-count checks); args are intentionally ignored.
func (c *imFakeConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	return &imFakeRows{b: c.b}, nil
}

type imFakeStmt struct{ b *imFakeBehavior }

func (s *imFakeStmt) Close() error  { return nil }
func (s *imFakeStmt) NumInput() int { return -1 }
func (s *imFakeStmt) Exec(a []driver.Value) (driver.Result, error) {
	return nil, errors.New("im fake: no exec")
}
func (s *imFakeStmt) Query(a []driver.Value) (driver.Rows, error) { return &imFakeRows{b: s.b}, nil }

type imFakeRows struct {
	b *imFakeBehavior
	i int
}

func (r *imFakeRows) Columns() []string { return r.b.cols }
func (r *imFakeRows) Close() error      { return nil }
func (r *imFakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.b.rows) {
		if r.b.nextErr != nil {
			return r.b.nextErr
		}
		return io.EOF
	}
	row := r.b.rows[r.i]
	r.i++
	copy(dest, row)
	return nil
}

// imPragmaCols mirrors the 6-column shape of PRAGMA table_info that the schema
// probes scan (cid, name, type, notnull, dflt, pk).
var imPragmaCols = []string{"cid", "name", "type", "notnull", "dflt", "pk"}

// imBadPragmaRow is a PRAGMA row whose first value ("x") cannot convert to the int
// `cid` scan target, forcing a Scan error in the probe loops.
func imBadPragmaRow() [][]driver.Value {
	return [][]driver.Value{{"x", "col", nil, int64(0), nil, int64(0)}}
}

// ---------------------------------------------------------------------------
// addressbook.go
// ---------------------------------------------------------------------------

// TestIm_DefaultAddressBookRoot pins the FDA-gated Sources path layout the wiring
// boundary passes to NewResolver.
func TestIm_DefaultAddressBookRoot(t *testing.T) {
	got := DefaultAddressBookRoot("/Users/neil")
	want := filepath.Join("/Users/neil", "Library", "Application Support", "AddressBook", "Sources")
	if got != want {
		t.Fatalf("DefaultAddressBookRoot = %q, want %q", got, want)
	}
}

// TestIm_NormalizeHandleWhitespace proves a whitespace-only handle normalizes to the
// empty string (so it can never false-match a seeded key).
func TestIm_NormalizeHandleWhitespace(t *testing.T) {
	if got := normalizeHandle("   "); got != "" {
		t.Fatalf("normalizeHandle(spaces) = %q, want empty", got)
	}
	if got := normalizeHandle("\t \n"); got != "" {
		t.Fatalf("normalizeHandle(tabs/newlines) = %q, want empty", got)
	}
}

// TestIm_NewResolverSkipsNonDirAndMissingDB proves NewResolver walks the Sources root
// defensively: a plain file entry is skipped (not a source dir), and a source dir with
// no AddressBook DB file is skipped — yielding a usable empty resolver (raw handles).
func TestIm_NewResolverSkipsNonDirAndMissingDB(t *testing.T) {
	root := t.TempDir()
	// A plain file directly under the root (not a directory) → skipped at !IsDir.
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory with NO AddressBook-v22.abcddb inside → os.Stat fails → skipped.
	if err := os.MkdirAll(filepath.Join(root, "EMPTYSRC"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if got := r.Resolve("+14155551234"); got != "+14155551234" {
		t.Fatalf("empty resolver should fall back to raw handle, got %q", got)
	}
}

// TestIm_LoadAddressBookPingFails proves a source whose DB path cannot be opened is
// skipped at Ping (never fatal): the resolver degrades to raw handles. The AddressBook
// path is a DIRECTORY, so os.Stat succeeds (the source is attempted) but the sqlite
// open/Ping fails — the honest degrade path for an unreadable source.
func TestIm_LoadAddressBookPingFails(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "GARBAGESRC")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory where the DB file is expected: os.Stat succeeds, but Ping fails when
	// loadAddressBookSource forces a read (a directory is not a sqlite database).
	if err := os.MkdirAll(filepath.Join(src, addressBookFilename), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if got := r.Resolve("+14155551234"); got != "+14155551234" {
		t.Fatalf("garbage AddressBook source should degrade to raw handles, got %q", got)
	}
}

// TestIm_LoadAddressBookColumnsMissing proves a valid sqlite DB lacking the required
// ZABCD* tables/columns is skipped (schema-defensive, A4): addressBookColumnsPresent
// returns false and the source contributes nothing.
func TestIm_LoadAddressBookColumnsMissing(t *testing.T) {
	root := imSeedRawAddressBook(t,
		// A valid DB, but with an unrelated schema — none of the ZABCD* tables exist.
		`CREATE TABLE UNRELATED (a TEXT, b TEXT)`,
		`INSERT INTO UNRELATED VALUES ('x','y')`,
	)
	r, err := NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if got := r.Resolve("+14155551234"); got != "+14155551234" {
		t.Fatalf("schema-mismatch source must add nothing (raw fallback), got %q", got)
	}
}

// TestIm_LoadAddressBookEmptyNames proves a source whose only records compose to an
// empty name (no first/last/org/nick) short-circuits before the phone/email joins —
// nothing is resolvable.
func TestIm_LoadAddressBookEmptyNames(t *testing.T) {
	root := imSeedRawAddressBook(t,
		`CREATE TABLE ZABCDRECORD (Z_PK INTEGER PRIMARY KEY, ZFIRSTNAME TEXT, ZLASTNAME TEXT, ZORGANIZATION TEXT, ZNICKNAME TEXT)`,
		`CREATE TABLE ZABCDPHONENUMBER (ZOWNER INTEGER, ZFULLNUMBER TEXT)`,
		`CREATE TABLE ZABCDEMAILADDRESS (ZOWNER INTEGER, ZADDRESS TEXT)`,
		// Record with every name column NULL → composeName returns "" → names stays empty.
		`INSERT INTO ZABCDRECORD (Z_PK) VALUES (1)`,
		`INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES (1, '+14155551234')`,
	)
	r, err := NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if got := r.Resolve("+14155551234"); got != "+14155551234" {
		t.Fatalf("nameless record must not resolve; got %q", got)
	}
}

// TestIm_AddressBookRecordScanError proves a malformed ZABCDRECORD row (a non-integer
// Z_PK that fails the int64 scan) is tolerated: the row is skipped and the source
// degrades to raw handles rather than aborting the build.
func TestIm_AddressBookRecordScanError(t *testing.T) {
	root := imSeedRawAddressBook(t,
		// Untyped columns let us insert a text Z_PK that fails Scan(&pk int64).
		`CREATE TABLE ZABCDRECORD (Z_PK, ZFIRSTNAME, ZLASTNAME, ZORGANIZATION, ZNICKNAME)`,
		`CREATE TABLE ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER)`,
		`CREATE TABLE ZABCDEMAILADDRESS (ZOWNER, ZADDRESS)`,
		`INSERT INTO ZABCDRECORD (Z_PK, ZFIRSTNAME) VALUES ('not-an-int', 'Ghost')`,
	)
	r, err := NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if got := r.Resolve("+14155551234"); got != "+14155551234" {
		t.Fatalf("record with a bad PK must be skipped; got %q", got)
	}
}

// TestIm_AddressBookEmailResolves exercises the email join path (never hit by the
// existing phone-only seeds): a contact's email address resolves to its name.
func TestIm_AddressBookEmailResolves(t *testing.T) {
	root := seedAddressBook(t, abRecord{first: "Ada", last: "Lovelace", email: "Ada@Example.com"})
	r, err := NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	// Case-insensitive email normalization: the seeded mixed-case address resolves.
	if got := r.Resolve("ada@example.com"); got != "Ada Lovelace" {
		t.Fatalf("email should resolve to the contact name, got %q", got)
	}
}

// TestIm_AddressBookPhoneAndEmailScanErrors proves malformed phone AND email rows
// (non-integer ZOWNER failing the NullInt64 scan) are individually skipped while a
// well-formed contact still resolves.
func TestIm_AddressBookPhoneAndEmailScanErrors(t *testing.T) {
	root := imSeedRawAddressBook(t,
		`CREATE TABLE ZABCDRECORD (Z_PK INTEGER PRIMARY KEY, ZFIRSTNAME TEXT, ZLASTNAME TEXT, ZORGANIZATION TEXT, ZNICKNAME TEXT)`,
		// Untyped ZOWNER so a text owner value fails Scan(&owner sql.NullInt64).
		`CREATE TABLE ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER)`,
		`CREATE TABLE ZABCDEMAILADDRESS (ZOWNER, ZADDRESS)`,
		`INSERT INTO ZABCDRECORD (Z_PK, ZFIRSTNAME, ZLASTNAME) VALUES (1, 'Real', 'Name')`,
		`INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES ('bad-owner', '+14155551234')`,
		`INSERT INTO ZABCDEMAILADDRESS (ZOWNER, ZADDRESS) VALUES ('bad-owner', 'x@y.com')`,
	)
	r, err := NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if got := r.Resolve("+14155551234"); got != "+14155551234" {
		t.Fatalf("phone row with a bad owner must be skipped (raw fallback), got %q", got)
	}
	if got := r.Resolve("x@y.com"); got != "x@y.com" {
		t.Fatalf("email row with a bad owner must be skipped (raw fallback), got %q", got)
	}
}

// TestIm_AddResolvedSkips proves addResolved's three guards, each against a well-formed
// control handle that DOES resolve: a NULL owner/value, an owner pointing at no named
// record, and a handle value that normalizes to empty are all dropped.
func TestIm_AddResolvedSkips(t *testing.T) {
	root := imSeedRawAddressBook(t,
		`CREATE TABLE ZABCDRECORD (Z_PK INTEGER PRIMARY KEY, ZFIRSTNAME TEXT, ZLASTNAME TEXT, ZORGANIZATION TEXT, ZNICKNAME TEXT)`,
		`CREATE TABLE ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER)`,
		`CREATE TABLE ZABCDEMAILADDRESS (ZOWNER, ZADDRESS)`,
		`INSERT INTO ZABCDRECORD (Z_PK, ZFIRSTNAME, ZLASTNAME) VALUES (1, 'Kept', 'Contact')`,
		// Control: a normal phone owned by the named record → resolves.
		`INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES (1, '+14150000001')`,
		// NULL owner → addResolved returns at the validity guard.
		`INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES (NULL, '+14150000002')`,
		// Owner points at a record that has no composed name (pk 999 absent).
		`INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES (999, '+14150000003')`,
		// Value normalizes to "" (whitespace, no digits/@) → empty-key guard.
		`INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES (1, '   ')`,
	)
	r, err := NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if got := r.Resolve("+14150000001"); got != "Kept Contact" {
		t.Fatalf("control phone must resolve, got %q", got)
	}
	for _, h := range []string{"+14150000002", "+14150000003"} {
		if got := r.Resolve(h); got != h {
			t.Fatalf("Resolve(%q) = %q, want raw handle (row should have been skipped)", h, got)
		}
	}
}

// TestIm_AddressBookHasColumnClosedDB proves addressBookHasColumn reports "absent"
// (false) when the PRAGMA query itself errors — here because the DB is closed.
func TestIm_AddressBookHasColumnClosedDB(t *testing.T) {
	if addressBookHasColumn(imClosedDB(t), "ZABCDRECORD", "ZNICKNAME") {
		t.Fatal("addressBookHasColumn on a closed DB must be false (query error → absent)")
	}
}

// TestIm_AddressBookColumnsPresentClosedDB proves addressBookColumnsPresent degrades
// to false when the PRAGMA query errors (closed DB) rather than panicking.
func TestIm_AddressBookColumnsPresentClosedDB(t *testing.T) {
	if addressBookColumnsPresent(imClosedDB(t)) {
		t.Fatal("addressBookColumnsPresent on a closed DB must be false (query error)")
	}
}

// TestIm_AddressBookPragmaScanErrors proves both schema probes treat a PRAGMA row that
// fails to scan as "columns absent" (false), using the fake driver to emit a
// non-conforming table_info row that a real sqlite PRAGMA never produces.
func TestIm_AddressBookPragmaScanErrors(t *testing.T) {
	if addressBookHasColumn(imOpenFakeDB(t, &imFakeBehavior{cols: imPragmaCols, rows: imBadPragmaRow()}), "T", "C") {
		t.Fatal("addressBookHasColumn must be false when a PRAGMA row fails to scan")
	}
	if addressBookColumnsPresent(imOpenFakeDB(t, &imFakeBehavior{cols: imPragmaCols, rows: imBadPragmaRow()})) {
		t.Fatal("addressBookColumnsPresent must be false when a PRAGMA row fails to scan")
	}
}

// ---------------------------------------------------------------------------
// fda.go
// ---------------------------------------------------------------------------

// TestIm_ProbeReadable proves the doctor readability signal across its three outcomes:
// a real valid chat.db reports readable (true, nil); a nonexistent path fails at the
// read-only open (false, err); and a file with non-database contents opens but fails
// the sqlite_master read (false, err) — the second failure surfaces only at the real
// query, which is exactly the branch that guards against a corrupt-but-openable DB.
func TestIm_ProbeReadable(t *testing.T) {
	// (1) A real, valid chat.db built by the shared seed helper → readable.
	good := seedChatDB(t,
		[]seedChat{{rowid: 1, guid: "g", identifier: "+14155551234", participants: []string{"+14155551234"}}},
		nil,
	)
	if ok, err := ProbeReadable(good); !ok || err != nil {
		t.Fatalf("ProbeReadable(valid chat.db) = (%v, %v), want (true, nil)", ok, err)
	}

	// (2) A nonexistent path: the read-only open fails (mode=ro cannot create it).
	missing := filepath.Join(t.TempDir(), "does-not-exist.db")
	if ok, err := ProbeReadable(missing); ok || err == nil {
		t.Fatalf("ProbeReadable(missing) = (%v, %v), want (false, non-nil err)", ok, err)
	}

	// (3) A file with junk contents: the open/Ping succeeds lazily, but the forced
	// sqlite_master read fails — the corrupt-but-openable guard.
	junk := filepath.Join(t.TempDir(), "chat.db")
	if err := os.WriteFile(junk, []byte("definitely not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := ProbeReadable(junk); ok || err == nil {
		t.Fatalf("ProbeReadable(junk) = (%v, %v), want (false, non-nil err)", ok, err)
	}
}

// ---------------------------------------------------------------------------

// TestIm_ParticipantHandles proves the other-party handle selection: explicit group
// participants win; else the 1:1 identifier stands in; else nil (never fabricated).
func TestIm_ParticipantHandles(t *testing.T) {
	t.Run("group participants returned (defensive copy)", func(t *testing.T) {
		got := participantHandles(conversation{participants: []string{"+14155551234", "+19998887777"}})
		if len(got) != 2 || got[0] != "+14155551234" || got[1] != "+19998887777" {
			t.Fatalf("group handles = %v, want the two participants", got)
		}
	})
	t.Run("no roster falls back to the 1:1 identifier", func(t *testing.T) {
		got := participantHandles(conversation{identifier: "+14155551234"})
		if len(got) != 1 || got[0] != "+14155551234" {
			t.Fatalf("identifier-only handles = %v, want [+14155551234]", got)
		}
	})
	t.Run("neither participants nor identifier → nil", func(t *testing.T) {
		if got := participantHandles(conversation{}); got != nil {
			t.Fatalf("empty conversation handles = %v, want nil", got)
		}
	})
}

// ---------------------------------------------------------------------------
// render.go
// ---------------------------------------------------------------------------

// TestIm_RenderTitle1to1FromParticipant proves a 1:1 with NO explicit identifier falls
// back to its single participant handle for the title (resolved to a name).
func TestIm_RenderTitle1to1FromParticipant(t *testing.T) {
	c := conversation{participants: []string{"+14155551234"}} // identifier empty, not a group
	if got := renderTitle(c, resolver1to1()); got != "Neil Patel" {
		t.Fatalf("1:1 without identifier should title from the participant, got %q", got)
	}
}

// TestIm_RenderLineEmptySystem proves a system event with only-whitespace text renders
// to nothing (no empty italic line, no day header downstream).
func TestIm_RenderLineEmptySystem(t *testing.T) {
	if got := renderLine(renderMessage{kind: msgSystem, text: "   "}, resolver1to1()); got != "" {
		t.Fatalf("empty system event should render \"\", got %q", got)
	}
}

// ---------------------------------------------------------------------------
// typedstream.go — decoder edge/error branches
// ---------------------------------------------------------------------------

// TestIm_DecodeAttributedBodyBoundsBranches drives the remaining decoder guards: the
// content marker at the blob's end, multi-byte length prefixes whose length bytes run
// past the blob (0x82/0x83), an unrecognized prefix byte, a negative decoded length
// (0x83 high bit set), and a single-byte length whose payload is entirely absent
// (clamped to zero). Every anomaly must return "" without panicking.
func TestIm_DecodeAttributedBodyBoundsBranches(t *testing.T) {
	nsstr := func(tail ...byte) []byte { return append([]byte("NSString"), tail...) }

	t.Run("0x2b content marker at end of blob", func(t *testing.T) {
		// realPreamble ends in 0x2b; nothing follows → p advances past the blob.
		if got := decodeAttributedBody(nsstr(0x01, 0x94, 0x84, 0x01, 0x2b)); got != "" {
			t.Fatalf("got %q, want \"\" when nothing follows the content marker", got)
		}
	})
	t.Run("0x82 length bytes run past the blob", func(t *testing.T) {
		// 0x82 needs 4 following length bytes; only 1 present.
		if got := decodeAttributedBody(nsstr(0x01, 0x94, 0x84, 0x01, 0x2b, 0x82, 0x01)); got != "" {
			t.Fatalf("got %q, want \"\" when the 0x82 length bytes are truncated", got)
		}
	})
	t.Run("0x83 length bytes run past the blob", func(t *testing.T) {
		// 0x83 needs 8 following length bytes; only 2 present.
		if got := decodeAttributedBody(nsstr(0x01, 0x94, 0x84, 0x01, 0x2b, 0x83, 0x01, 0x02)); got != "" {
			t.Fatalf("got %q, want \"\" when the 0x83 length bytes are truncated", got)
		}
	})
	t.Run("unrecognized length-prefix byte (0x80)", func(t *testing.T) {
		// 0x80 is neither <0x80 nor 0x81/0x82/0x83 → the default branch returns "".
		if got := decodeAttributedBody(nsstr(0x01, 0x94, 0x84, 0x01, 0x2b, 0x80, 0x41, 0x41)); got != "" {
			t.Fatalf("got %q, want \"\" for an unrecognized length prefix", got)
		}
	})
	t.Run("0x83 length with the high bit set decodes to a negative int", func(t *testing.T) {
		// uint64 0x8000000000000000 → int is negative → the n<0 guard returns "".
		blob := buildBlob([]byte("tail"), []byte{0x83, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x80})
		if got := decodeAttributedBody(blob); got != "" {
			t.Fatalf("got %q, want \"\" for a negative decoded length", got)
		}
	})
	t.Run("single-byte length with no payload bytes clamps to empty", func(t *testing.T) {
		// prefix 0x05 claims 5 bytes but none follow → clamp to 0 → n<=0 → "".
		if got := decodeAttributedBody(nsstr(0x01, 0x94, 0x84, 0x01, 0x2b, 0x05)); got != "" {
			t.Fatalf("got %q, want \"\" when the claimed payload is entirely absent", got)
		}
	})
}
