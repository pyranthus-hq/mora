package applecal

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestSw_DefaultAndLegacyDBPaths(t *testing.T) {
	home := filepath.Join("tmp", "home")
	if got, want := DefaultDBPath(home), filepath.Join(home, "Library", "Group Containers", "group.com.apple.calendar", "Calendar.sqlitedb"); got != want {
		t.Fatalf("DefaultDBPath = %q, want %q", got, want)
	}
	if got, want := LegacyDBPath(home), filepath.Join(home, "Library", "Calendars", "Calendar.sqlitedb"); got != want {
		t.Fatalf("LegacyDBPath = %q, want %q", got, want)
	}
}

func TestSw_CloseNilFetcher(t *testing.T) {
	if err := (&LiveFetcher{}).Close(); err != nil {
		t.Fatalf("nil DB close should be a no-op, got %v", err)
	}
}

func TestSw_ProbeSchemaQueryError(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "closed.sqlitedb"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := probeSchema(db); err == nil || !strings.Contains(err.Error(), "probe Calendar.sqlitedb schema") {
		t.Fatalf("expected closed DB probe error, got %v", err)
	}
}

func TestSw_ProbeSchemaMissingColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-column.sqlitedb")
	db := swOpenSQLite(t, path)
	defer db.Close()
	swCreateCalendarSchema(t, db, "hidden")

	err := probeSchema(db)
	if err == nil {
		t.Fatal("expected schema probe to reject a missing CalendarItem column")
	}
	if !strings.Contains(err.Error(), "missing CalendarItem columns: hidden") {
		t.Fatalf("expected missing-column schema error, got %v", err)
	}
}

func TestSw_ProbeSchemaScanAndRowsErrors(t *testing.T) {
	db := swOpenCoverDriverDB(t, "probe-scan")
	defer db.Close()
	if err := probeSchema(db); err == nil || !strings.Contains(err.Error(), "probe Calendar.sqlitedb schema") {
		t.Fatalf("expected probe scan error, got %v", err)
	}

	db = swOpenCoverDriverDB(t, "probe-rows-err")
	defer db.Close()
	if err := probeSchema(db); err == nil || !strings.Contains(err.Error(), "probe Calendar.sqlitedb schema") {
		t.Fatalf("expected probe rows error, got %v", err)
	}
}

func TestSw_FetchPageRejectsUnsupportedKindAndBadCursor(t *testing.T) {
	f, err := NewLiveFetcher(seedDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if _, err := f.FetchPage(memory.ItemKind("gmail_thread"), memory.FetchWindow{}, ""); err == nil || !strings.Contains(err.Error(), "unsupported kind") {
		t.Fatalf("expected unsupported kind error, got %v", err)
	}
	if _, err := f.FetchPage(KindAppleCalEvent, memory.FetchWindow{}, "not-an-int"); err == nil || !strings.Contains(err.Error(), "bad cursor") {
		t.Fatalf("expected bad cursor error, got %v", err)
	}
}

func TestSw_FetchPageQueryErrorAfterClose(t *testing.T) {
	f, err := NewLiveFetcher(seedDB(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = f.FetchPage(KindAppleCalEvent, memory.FetchWindow{}, "")
	if err == nil || !strings.Contains(err.Error(), "query events") {
		t.Fatalf("expected query error after close, got %v", err)
	}
}

func TestSw_FetchPageRowsError(t *testing.T) {
	db := swOpenCoverDriverDB(t, "fetch-rows-err")
	defer db.Close()
	f := &LiveFetcher{db: db}

	_, err := f.FetchPage(KindAppleCalEvent, memory.FetchWindow{}, "")
	if err == nil || !strings.Contains(err.Error(), "read events") {
		t.Fatalf("expected read events error, got %v", err)
	}
}

func TestSw_FetchPageScanEventError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad-event.sqlitedb")
	db := swOpenSQLite(t, path)
	swCreateCalendarSchema(t, db, "")
	swMustExec(t, db, `INSERT INTO Calendar VALUES (1, 'Work')`)
	swMustExec(t, db, `INSERT INTO CalendarItem (ROWID, summary, description, start_date, end_date, all_day, calendar_id, entity_type, UUID, hidden)
		VALUES (1, 'Bad time', '', 'not-a-number', 42, 0, 1, 2, 'BAD-TIME', 0)`)
	db.Close()

	f, err := NewLiveFetcher(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	_, err = f.FetchPage(KindAppleCalEvent, memory.FetchWindow{}, "")
	if err == nil || !strings.Contains(err.Error(), "scan event") {
		t.Fatalf("expected scan event error, got %v", err)
	}
}

func TestSw_FetchPageFullPageCursorAndFallbackIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "full-page.sqlitedb")
	db := swOpenSQLite(t, path)
	swCreateCalendarSchema(t, db, "")
	swMustExec(t, db, `INSERT INTO Calendar VALUES (1, 'Work')`)
	start := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	for i := 1; i <= pageSize; i++ {
		uuid := fmt.Sprintf("UUID-%03d", i)
		if i == 1 {
			uuid = ""
		}
		swMustExec(t, db, `INSERT INTO CalendarItem (ROWID, summary, description, start_date, end_date, all_day, calendar_id, entity_type, UUID, hidden)
			VALUES (?, ?, '', ?, ?, 0, 1, 2, ?, 0)`,
			i, fmt.Sprintf("Event %03d", i), start.Sub(appleEpoch).Seconds(), start.Add(time.Hour).Sub(appleEpoch).Seconds(), uuid)
	}
	db.Close()

	f, err := NewLiveFetcher(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	page, err := f.FetchPage(KindAppleCalEvent, memory.FetchWindow{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != pageSize {
		t.Fatalf("expected a full page of %d events, got %d", pageSize, len(page.Items))
	}
	if page.NextCursor != fmt.Sprint(pageSize) {
		t.Fatalf("NextCursor = %q, want %q", page.NextCursor, fmt.Sprint(pageSize))
	}
	if page.Items[0].ProviderID != "1" {
		t.Fatalf("empty UUID should fall back to ROWID provider id, got %q", page.Items[0].ProviderID)
	}
}

func TestSw_ParticipantsDegradeOnQueryAndScanErrors(t *testing.T) {
	closedDB := swOpenSQLite(t, filepath.Join(t.TempDir(), "closed-participants.sqlitedb"))
	if err := closedDB.Close(); err != nil {
		t.Fatal(err)
	}
	if attendees, organizer := (&LiveFetcher{db: closedDB}).participants(1); len(attendees) != 0 || organizer != "" {
		t.Fatalf("query error should degrade to no participants, got attendees=%v organizer=%q", attendees, organizer)
	}

	db := swOpenSQLite(t, filepath.Join(t.TempDir(), "bad-participants.sqlitedb"))
	defer db.Close()
	swMustExec(t, db, `CREATE TABLE Participant (owner_id INTEGER, email TEXT, role TEXT)`)
	swMustExec(t, db, `INSERT INTO Participant (owner_id, email, role) VALUES (1, 'chair@example.com', 'chair')`)
	if attendees, organizer := (&LiveFetcher{db: db}).participants(1); len(attendees) != 0 || organizer != "" {
		t.Fatalf("scan error should degrade to no participants, got attendees=%v organizer=%q", attendees, organizer)
	}
}

func TestSw_EventItemAllDayFallbackAndSparseBody(t *testing.T) {
	start := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	it := eventItem(42, "Independence Day", "", "Personal", "", "", start, start, true, nil, "")

	if it.ProviderID != "42" {
		t.Fatalf("empty UUID should fall back to ROWID, got %q", it.ProviderID)
	}
	if !strings.Contains(it.Body, "When: 2026-07-04 (all day)") {
		t.Fatalf("all-day body missing date: %q", it.Body)
	}
	if strings.Contains(it.Body, "Location:") || strings.Contains(it.Body, "Attendees:") {
		t.Fatalf("sparse event body should omit empty location and attendees: %q", it.Body)
	}
	if _, ok := it.Meta["attendees"]; ok {
		t.Fatalf("meta should omit empty attendees: %+v", it.Meta)
	}
	if _, ok := it.Meta["organizer"]; ok {
		t.Fatalf("meta should omit empty organizer: %+v", it.Meta)
	}
}

func swOpenSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func swMustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func swCreateCalendarSchema(t *testing.T, db *sql.DB, omitCalendarItemColumn string) {
	t.Helper()
	swMustExec(t, db, `CREATE TABLE Calendar (ROWID INTEGER PRIMARY KEY, title TEXT)`)
	swMustExec(t, db, `CREATE TABLE Location (ROWID INTEGER PRIMARY KEY, title TEXT)`)
	swMustExec(t, db, `CREATE TABLE Participant (ROWID INTEGER PRIMARY KEY, owner_id INTEGER, email TEXT, role INTEGER)`)
	cols := []string{
		"ROWID INTEGER PRIMARY KEY",
		"summary TEXT",
		"description TEXT",
		"start_date REAL",
		"end_date REAL",
		"all_day INTEGER",
		"calendar_id INTEGER",
		"location_id INTEGER",
		"entity_type INTEGER",
		"UUID TEXT",
		"hidden INTEGER",
	}
	var kept []string
	for _, col := range cols {
		if strings.HasPrefix(col, omitCalendarItemColumn+" ") {
			continue
		}
		kept = append(kept, col)
	}
	swMustExec(t, db, `CREATE TABLE CalendarItem (`+strings.Join(kept, ", ")+`)`)
}

var swRegisterCoverDriverOnce sync.Once

func swOpenCoverDriverDB(t *testing.T, mode string) *sql.DB {
	t.Helper()
	swRegisterCoverDriverOnce.Do(func() {
		sql.Register("sw_applecal_cover", swCoverDriver{})
	})
	db, err := sql.Open("sw_applecal_cover", mode)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

type swCoverDriver struct{}

func (swCoverDriver) Open(name string) (driver.Conn, error) {
	return swCoverConn{mode: name}, nil
}

type swCoverConn struct {
	mode string
}

func (c swCoverConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not used by sw cover driver")
}

func (c swCoverConn) Close() error {
	return nil
}

func (c swCoverConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not used by sw cover driver")
}

func (c swCoverConn) Query(string, []driver.Value) (driver.Rows, error) {
	switch c.mode {
	case "probe-scan":
		return &swCoverRows{
			columns: []string{"cid", "name", "type", "notnull", "dflt_value", "pk"},
			rows: [][]driver.Value{
				{"not-an-int", "ROWID", "", int64(0), nil, int64(1)},
			},
		}, nil
	case "probe-rows-err":
		return &swCoverRows{
			columns: []string{"cid", "name", "type", "notnull", "dflt_value", "pk"},
			err:     errSwCoverRows,
		}, nil
	case "fetch-rows-err":
		return &swCoverRows{
			columns: []string{"ROWID", "summary", "description", "start_date", "end_date", "all_day", "UUID", "title", "title"},
			err:     errSwCoverRows,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported sw cover mode %q", c.mode)
	}
}

var errSwCoverRows = errors.New("sw cover rows failed")

type swCoverRows struct {
	columns []string
	rows    [][]driver.Value
	err     error
	idx     int
}

func (r *swCoverRows) Columns() []string {
	return r.columns
}

func (r *swCoverRows) Close() error {
	return nil
}

func (r *swCoverRows) Next(dest []driver.Value) error {
	if r.err != nil {
		return r.err
	}
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.idx])
	r.idx++
	return nil
}
