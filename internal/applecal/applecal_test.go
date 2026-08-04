package applecal

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// seedDB builds a minimal Calendar.sqlitedb fixture with the columns the
// connector's probe requires plus realistic Core Data timestamps.
func seedDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Calendar.sqlitedb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE Calendar (ROWID INTEGER PRIMARY KEY, title TEXT)`,
		`CREATE TABLE Location (ROWID INTEGER PRIMARY KEY, title TEXT)`,
		`CREATE TABLE Participant (ROWID INTEGER PRIMARY KEY, owner_id INTEGER, email TEXT, role INTEGER, is_self INTEGER DEFAULT 0)`,
		`CREATE TABLE CalendarItem (ROWID INTEGER PRIMARY KEY, summary TEXT, description TEXT,
			start_date REAL, end_date REAL, all_day INTEGER DEFAULT 0, calendar_id INTEGER,
			location_id INTEGER, entity_type INTEGER, UUID TEXT, hidden INTEGER DEFAULT 0)`,
		`INSERT INTO Calendar VALUES (1, 'Work'), (2, 'US Holidays')`,
		`INSERT INTO Location VALUES (1, 'Blue Bottle SF')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	at := func(ts time.Time) float64 { return ts.Sub(appleEpoch).Seconds() }
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	rows := []struct {
		rowid              int64
		summary, desc      string
		start              time.Time
		cal, loc, uuid     string
		entityType, hidden int
	}{
		{10, "Coffee with Neil", "agenda: pilot next steps", now.Add(24 * time.Hour), "Work", "Blue Bottle SF", "UUID-NEIL", 2, 0},
		{11, "Old standup", "", now.Add(-200 * 24 * time.Hour), "Work", "", "UUID-OLD", 2, 0},             // outside Since
		{12, "Hanukkah 2029", "", now.Add(3 * 365 * 24 * time.Hour), "US Holidays", "", "UUID-FAR", 2, 0}, // outside Until
		{13, "Hidden phantom", "", now.Add(24 * time.Hour), "Work", "", "UUID-HID", 2, 1},                 // hidden
		{14, "A reminder", "", now.Add(24 * time.Hour), "Work", "", "UUID-REM", 1, 0},                     // not an event
	}
	for _, r := range rows {
		calID := 1
		if r.cal == "US Holidays" {
			calID = 2
		}
		locID := any(nil)
		if r.loc != "" {
			locID = 1
		}
		if _, err := db.Exec(`INSERT INTO CalendarItem (ROWID, summary, description, start_date, end_date, all_day, calendar_id, location_id, entity_type, UUID, hidden)
			VALUES (?,?,?,?,?,0,?,?,?,?,?)`,
			r.rowid, r.summary, r.desc, at(r.start), at(r.start.Add(time.Hour)), calID, locID, r.entityType, r.uuid, r.hidden); err != nil {
			t.Fatal(err)
		}
	}
	// Alex is the local user (is_self=1) AND the organizer, exactly as the real
	// Calendar.sqlitedb records it. The empty-email row is the participant the
	// attendee list legitimately drops.
	if _, err := db.Exec(`INSERT INTO Participant (owner_id, email, role, is_self) VALUES
		(10, 'neil@example.com', 0, 0), (10, 'mailto:Alex.Owner@gmail.com', 1, 1), (10, '', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCalendarDBDSNIsHierarchicalAndReadOnly pins the modern group-container
// path contract from #266. Escaping the whole filename with url.PathEscape
// encoded every slash and produced an opaque file URI; Darwin's VFS then failed
// at the first schema read even though the same database opened directly.
func TestCalendarDBDSNIsHierarchicalAndReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Group Containers", "group.com.apple.calendar", "Calendar ?#%.sqlitedb")
	dsn := calendarDBDSN(path)
	if !strings.HasPrefix(dsn, "file:///") {
		t.Fatalf("calendar DSN = %q, want a hierarchical absolute file URI", dsn)
	}
	if strings.Contains(strings.ToLower(dsn), "%2f") {
		t.Fatalf("calendar DSN escaped path separators: %q", dsn)
	}
	if strings.Contains(dsn, "%5C") {
		t.Fatalf("calendar DSN escaped Windows path separators: %q", dsn)
	}
	for _, want := range []string{"Group%20Containers", "Calendar%20%3F%23%25.sqlitedb", "mode=ro", "busy_timeout(5000)", "query_only(1)"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("calendar DSN %q missing %q", dsn, want)
		}
	}
	if strings.Contains(dsn, "immutable") {
		t.Fatalf("live Calendar database must not be opened immutable: %q", dsn)
	}
}

// TestLiveFetcherReadsTheLiveWAL reproduces the connector side of #266 without
// requiring a private Calendar database. Calendar.app owns a live WAL store;
// its schema and newest rows may exist only in readable WAL/SHM sidecars. An
// immutable reader ignores change detection and can miss that state. Mora must
// see the committed WAL while remaining unable to write.
func TestLiveFetcherReadsTheLiveWAL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Library", "Group Containers", "group.com.apple.calendar")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "Calendar.sqlitedb")
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE Calendar (ROWID INTEGER PRIMARY KEY, title TEXT)`,
		`CREATE TABLE Location (ROWID INTEGER PRIMARY KEY, title TEXT)`,
		`CREATE TABLE Participant (ROWID INTEGER PRIMARY KEY, owner_id INTEGER, email TEXT, role INTEGER)`,
		`CREATE TABLE CalendarItem (ROWID INTEGER PRIMARY KEY, summary TEXT, description TEXT, start_date REAL, end_date REAL, all_day INTEGER, calendar_id INTEGER, location_id INTEGER, entity_type INTEGER, UUID TEXT, hidden INTEGER)`,
		`INSERT INTO Calendar VALUES (1, 'Live')`,
	} {
		if _, err := writer.Exec(stmt); err != nil {
			t.Fatalf("seed live WAL with %q: %v", stmt, err)
		}
	}

	f, err := NewLiveFetcher(path)
	if err != nil {
		t.Fatalf("open live Calendar WAL read-only: %v", err)
	}
	defer f.Close()
	var title string
	if err := f.db.QueryRow(`SELECT title FROM Calendar WHERE ROWID = 1`).Scan(&title); err != nil {
		t.Fatalf("read row committed in live WAL: %v", err)
	}
	if title != "Live" {
		t.Fatalf("live WAL title = %q, want Live", title)
	}
	if _, err := f.db.Exec(`CREATE TABLE mora_must_not_write (x INTEGER)`); err == nil {
		t.Fatal("Apple Calendar connection accepted a write despite mode=ro + query_only")
	}
}

// TestFetchPageMapsEvents locks the connector contract: events only
// (entity_type=2), hidden rows skipped, Since/Until both enforced (an
// unbounded Until would flood the vault with subscribed-holiday events years
// out), Core Data epoch converted, participants normalized + sorted with the
// organizer split out, and Meta mirroring the Google Calendar conventions so
// the entity graph reads both calendars identically.
func TestFetchPageMapsEvents(t *testing.T) {
	f, err := NewLiveFetcher(seedDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	page, err := f.FetchPage(KindAppleCalEvent, memory.FetchWindow{
		Since: now.Add(-90 * 24 * time.Hour),
		Until: now.Add(180 * 24 * time.Hour),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("want exactly the Neil event, got %d items: %+v", len(page.Items), page.Items)
	}
	it := page.Items[0]
	if it.ProviderID != "UUID-NEIL" || it.Title != "Coffee with Neil" {
		t.Fatalf("wrong event: %+v", it)
	}
	if it.OccurredAt != now.Add(24*time.Hour) {
		t.Fatalf("Core Data epoch conversion wrong: %v", it.OccurredAt)
	}
	for _, want := range []string{"Calendar: Work", "Location: Blue Bottle SF", "agenda: pilot next steps"} {
		if !strings.Contains(it.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, it.Body)
		}
	}
	att, _ := it.Meta["attendees"].([]string)
	if len(att) != 2 || att[0] != "alex.owner@gmail.com" || att[1] != "neil@example.com" {
		t.Fatalf("attendees not normalized+sorted: %v", att)
	}
	if org, _ := it.Meta["organizer"].(string); org != "alex.owner@gmail.com" {
		t.Fatalf("organizer = %q", org)
	}
	if page.NextCursor != "" {
		t.Fatalf("short page must end pagination, got cursor %q", page.NextCursor)
	}

	// The shared MapItem must emit the registered type/provider.
	mm := memory.MapItem(it, "personal", 0)
	if mm.Provider != "applecal" || mm.Type != "event" {
		t.Fatalf("kind registration broken: provider=%q type=%q", mm.Provider, mm.Type)
	}
	if mm.StableID != "applecal_event/UUID-NEIL" {
		t.Fatalf("StableID = %q", mm.StableID)
	}
}

// TestFetchPageCursorPaging locks resume semantics: the cursor is the last
// ROWID and a full page hands back a non-empty cursor.
func TestFetchPageCursorPaging(t *testing.T) {
	f, err := NewLiveFetcher(seedDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// No window: all 3 non-hidden events visible. Page after ROWID 10 → 11, 12.
	page, err := f.FetchPage(KindAppleCalEvent, memory.FetchWindow{}, "10")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].ProviderID != "UUID-OLD" {
		t.Fatalf("cursor paging wrong: %+v", page.Items)
	}
}

// TestUnsupportedSchemaErrors locks the probe: a database missing the event
// tables must fail at open with a clear schema error, not mid-query.
func TestUnsupportedSchemaErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.sqlitedb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE x (y INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := NewLiveFetcher(path); err == nil || !strings.Contains(err.Error(), "unsupported Calendar.sqlitedb schema") {
		t.Fatalf("want schema error, got: %v", err)
	}
}

// TestFetchPageCapturesSelfParticipant pins the zero-config self signal for Apple
// Calendar. The local Participant table carries an is_self column marking the user's
// own row, and Mora was dropping it. Without it the brief cannot recognize the user
// among a meeting's invitees (the calendar routinely lists them under an iCloud/me
// alias the connected Google mailbox has never seen), admits them as an attendee of
// their own meeting, and cites their own records back as the counterparty's
// unfinished business — wrong-person attribution, severity-1. Mirrors the
// meta["self_email"] key the Google connector emits from Attendee.Self.
func TestFetchPageCapturesSelfParticipant(t *testing.T) {
	f, err := NewLiveFetcher(seedDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	page, err := f.FetchPage(KindAppleCalEvent, memory.FetchWindow{
		Since: now.Add(-90 * 24 * time.Hour),
		Until: now.Add(180 * 24 * time.Hour),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("want the seeded event, got %d", len(page.Items))
	}
	if got, _ := page.Items[0].Meta["self_email"].(string); got != "alex.owner@gmail.com" {
		t.Fatalf("meta[self_email] = %q, want the lowercased is_self participant %q", got, "alex.owner@gmail.com")
	}
}
