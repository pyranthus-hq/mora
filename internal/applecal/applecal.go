// Package applecal is the macOS Apple Calendar connector: a read-only reader
// of the local Calendar store (Calendar.sqlitedb in the calendar group
// container), one memory per event. It mirrors the iMessage connector's
// constraints exactly: pure Go (modernc sqlite), NO network imports, NO
// internal/mora import (mora imports us — the connector seam), and a read-only
// live-WAL-aware open so we never write or checkpoint Apple's database. The real
// access gate is Full Disk Access (same TCC story as chat.db), surfaced by the
// caller's error text, not a login.
package applecal

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// KindAppleCalEvent is the connector's ItemKind. The init below registers its
// type/provider mapping so the shared memory.MapItem emits
// Type "event" / Provider "applecal" without internal/memory editing.
const KindAppleCalEvent memory.ItemKind = "applecal_event"

func init() {
	memory.RegisterKind(KindAppleCalEvent, "event", "applecal")
}

// appleEpoch is the Core Data reference date (2001-01-01T00:00:00Z). All
// Calendar.sqlitedb timestamps are seconds since this instant.
var appleEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// appleTime converts a Core Data timestamp to UTC time.
func appleTime(sec float64) time.Time {
	return appleEpoch.Add(time.Duration(sec * float64(time.Second))).UTC()
}

// DefaultDBPath returns the modern Calendar store location (the calendar group
// container). Older macOS kept ~/Library/Calendars/Calendar.sqlitedb — the
// caller may probe both; this returns the current default.
func DefaultDBPath(home string) string {
	return filepath.Join(home, "Library", "Group Containers", "group.com.apple.calendar", "Calendar.sqlitedb")
}

// LegacyDBPath is the pre-group-container location, kept as a fallback probe.
func LegacyDBPath(home string) string {
	return filepath.Join(home, "Library", "Calendars", "Calendar.sqlitedb")
}

// pageSize bounds one FetchPage. Events are small rows; 200 keeps the resume
// checkpoint granular without hammering the store.
const pageSize = 200

// LiveFetcher reads Calendar.sqlitedb read-only. It implements memory.Fetcher.
type LiveFetcher struct {
	db *sql.DB
}

// calendarDBDSN returns a hierarchical file URI, preserving path separators and
// escaping only path data such as the space in "Group Containers". PathEscape
// cannot be used on the whole filename: it escapes '/' and turns the absolute
// path into an opaque file URI, which the Darwin VFS can fail to open.
//
// Calendar.sqlitedb is a LIVE WAL database. immutable=1 is intentionally
// forbidden: SQLite documents that immutable disables change detection and may
// return incorrect results when another process changes the file. mode=ro plus
// query_only keeps Mora read-only while allowing SQLite to apply Calendar.app's
// readable WAL/SHM sidecars. busy_timeout bounds lock contention.
func calendarDBDSN(path string) string {
	uriPath := filepath.ToSlash(path)
	// url.URL treats a Windows drive path as a URI authority unless it starts
	// with '/'. SQLite needs file:///C:/... (hierarchical), never file://C:%5C...
	// (host = "C:%5C..."), which fails the first schema read cryptically.
	if filepath.VolumeName(path) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := &url.URL{Scheme: "file", Path: uriPath}
	return u.String() + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
}

// NewLiveFetcher opens the live store read-only and runs a schema probe so an
// unsupported store errors clearly instead of failing cryptically mid-query. A
// permission-denied open is the FDA-not-granted case; the caller wraps it with
// the doctor guidance.
func NewLiveFetcher(path string) (*LiveFetcher, error) {
	db, err := sql.Open("sqlite", calendarDBDSN(path))
	if err != nil {
		return nil, err
	}
	// Force a real read now so FDA denial surfaces at connect time.
	if err := probeSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &LiveFetcher{db: db}, nil
}

// Close releases the underlying DB handle.
func (f *LiveFetcher) Close() error {
	if f.db == nil {
		return nil
	}
	return f.db.Close()
}

// probeSchema confirms the tables/columns the event query needs exist, so an
// OS schema change yields "unsupported Calendar.sqlitedb schema: …" rather than
// a cryptic query error (the imessage Pitfall-9 lesson).
func probeSchema(db *sql.DB) error {
	required := map[string][]string{
		"CalendarItem": {"ROWID", "summary", "description", "start_date", "end_date", "all_day", "calendar_id", "entity_type", "UUID", "hidden"},
		"Calendar":     {"ROWID", "title"},
		"Location":     {"ROWID", "title"},
		"Participant":  {"owner_id", "email", "role"},
	}
	for table, cols := range required {
		rows, err := pragmaTableInfo(db, table)
		if err != nil {
			return fmt.Errorf("probe Calendar.sqlitedb schema: %w", err)
		}
		have := map[string]bool{}
		for rows.Next() {
			var (
				cid             int
				name            string
				ctype, dflt     sql.NullString
				notNull, isPKey int
			)
			if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &isPKey); err != nil {
				rows.Close()
				return fmt.Errorf("probe Calendar.sqlitedb schema: %w", err)
			}
			have[name] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("probe Calendar.sqlitedb schema: %w", err)
		}
		rows.Close()
		if len(have) == 0 {
			return fmt.Errorf("unsupported Calendar.sqlitedb schema: no `%s` table found", table)
		}
		var missing []string
		for _, c := range cols {
			if !have[c] {
				missing = append(missing, c)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("unsupported Calendar.sqlitedb schema (missing %s columns: %s)", table, strings.Join(missing, ", "))
		}
	}
	return nil
}

// FetchPage pages events ordered by CalendarItem ROWID; the cursor is the last
// ROWID seen ("" = first page, "" returned at the end). entity_type=2 selects
// EVENTS (tasks/reminders use other types), hidden rows (recurrence phantoms)
// are skipped, and the window bounds start_date — Since AND Until, because an
// unbounded Until would flood the vault with every subscribed-holiday event
// years out.
func (f *LiveFetcher) FetchPage(kind memory.ItemKind, w memory.FetchWindow, cursor string) (memory.Page, error) {
	if kind != KindAppleCalEvent {
		return memory.Page{}, fmt.Errorf("applecal: unsupported kind %q", kind)
	}
	after := int64(0)
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil {
			return memory.Page{}, fmt.Errorf("applecal: bad cursor %q: %w", cursor, err)
		}
		after = n
	}
	q := `SELECT ci.ROWID, ci.summary, COALESCE(ci.description, ''), ci.start_date,
	             COALESCE(ci.end_date, ci.start_date), ci.all_day, COALESCE(ci.UUID, ''),
	             COALESCE(c.title, ''), COALESCE(l.title, '')
	      FROM CalendarItem ci
	      JOIN Calendar c ON c.ROWID = ci.calendar_id
	      LEFT JOIN Location l ON l.ROWID = ci.location_id
	      WHERE ci.entity_type = 2 AND ci.hidden = 0 AND ci.summary IS NOT NULL
	        AND ci.start_date IS NOT NULL AND ci.ROWID > ?`
	args := []any{after}
	if !w.Since.IsZero() {
		q += " AND ci.start_date >= ?"
		args = append(args, w.Since.Sub(appleEpoch).Seconds())
	}
	if !w.Until.IsZero() {
		q += " AND ci.start_date <= ?"
		args = append(args, w.Until.Sub(appleEpoch).Seconds())
	}
	q += " ORDER BY ci.ROWID LIMIT ?"
	args = append(args, pageSize)

	rows, err := f.db.Query(q, args...)
	if err != nil {
		return memory.Page{}, fmt.Errorf("applecal: query events: %w", err)
	}
	defer rows.Close()

	type evRow struct {
		rowid             int64
		summary, desc     string
		start, end        float64
		allDay            int
		uuid, cal, locStr string
	}
	var evs []evRow
	for rows.Next() {
		var e evRow
		if err := rows.Scan(&e.rowid, &e.summary, &e.desc, &e.start, &e.end, &e.allDay, &e.uuid, &e.cal, &e.locStr); err != nil {
			return memory.Page{}, fmt.Errorf("applecal: scan event: %w", err)
		}
		evs = append(evs, e)
	}
	if err := rows.Err(); err != nil {
		return memory.Page{}, fmt.Errorf("applecal: read events: %w", err)
	}

	var items []memory.Item
	last := after
	for _, e := range evs {
		last = e.rowid
		attendees, organizer, self := f.participants(e.rowid)
		items = append(items, eventItem(e.rowid, e.summary, e.desc, e.cal, e.locStr, e.uuid,
			appleTime(e.start), appleTime(e.end), e.allDay != 0, attendees, organizer, self))
	}
	next := ""
	if len(evs) == pageSize {
		next = strconv.FormatInt(last, 10)
	}
	return memory.Page{Items: items, NextCursor: next}, nil
}

// participants returns the sorted attendee emails + the organizer email for an
// event. Best-effort: a query error degrades to no participants rather than
// failing the event (the entity graph loses an edge, the memory survives).
// participants reads an event's invitees, its organizer, and — critically — which
// row is the LOCAL USER. Calendar.sqlitedb marks the user's own Participant row with
// is_self=1. Mora used to drop that bit, leaving the brief unable to recognize the
// user among their own meeting's invitees: the calendar lists them under whichever
// alias the invite used (an iCloud/me.com address the connected Google mailbox has
// never seen), so self-exclusion missed them, they were admitted as an attendee of
// their own meeting, and their own records were cited back to them as the
// counterparty's unfinished business — wrong-person attribution, severity-1.
//
// is_self is read defensively: an older/foreign schema without the column must keep
// working (attendees + organizer as before, no self signal) rather than fail the sync.
func (f *LiveFetcher) participants(eventROWID int64) (attendees []string, organizer, self string) {
	selfExpr := "0"
	if f.hasSelfColumn() {
		selfExpr = "COALESCE(is_self, 0)"
	}
	rows, err := f.db.Query(
		`SELECT COALESCE(email, ''), COALESCE(role, 0), `+selfExpr+` FROM Participant WHERE owner_id = ?`, eventROWID)
	if err != nil {
		return nil, "", ""
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var email string
		var role, isSelf int
		if err := rows.Scan(&email, &role, &isSelf); err != nil {
			return nil, "", ""
		}
		email = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(email, "mailto:")))
		if email == "" || seen[email] {
			continue
		}
		seen[email] = true
		// EKParticipantRole: 1 == chair/organizer in the local store.
		if role == 1 && organizer == "" {
			organizer = email
		}
		if isSelf == 1 && self == "" {
			self = email
		}
		attendees = append(attendees, email)
	}
	sort.Strings(attendees)
	return attendees, organizer, self
}

// hasSelfColumn reports whether this Calendar.sqlitedb exposes Participant.is_self.
func (f *LiveFetcher) hasSelfColumn() bool {
	rows, err := f.db.Query(`SELECT 1 FROM pragma_table_info('Participant') WHERE name = 'is_self'`)
	if err != nil {
		return false
	}
	defer rows.Close()
	return rows.Next()
}

// eventItem renders one event as a provider-agnostic Item. Meta mirrors the
// Google Calendar conventions ("attendees", "organizer", "occurred_at") so the
// entity graph's connector-capture path reads both calendars identically.
func eventItem(rowid int64, summary, desc, calTitle, locTitle, uuid string, start, end time.Time, allDay bool, attendees []string, organizer, self string) memory.Item {
	providerID := uuid
	if providerID == "" {
		providerID = strconv.FormatInt(rowid, 10)
	}
	var b strings.Builder
	if allDay {
		fmt.Fprintf(&b, "When: %s (all day)\n", start.Format("2006-01-02"))
	} else {
		fmt.Fprintf(&b, "When: %s → %s\n", start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "Calendar: %s\n", calTitle)
	if locTitle != "" {
		fmt.Fprintf(&b, "Location: %s\n", locTitle)
	}
	if len(attendees) > 0 {
		fmt.Fprintf(&b, "Attendees: %s\n", strings.Join(attendees, ", "))
	}
	if desc != "" {
		fmt.Fprintf(&b, "\n%s\n", desc)
	}
	meta := map[string]any{"occurred_at": start.Format(time.RFC3339), "calendar": calTitle}
	if len(attendees) > 0 {
		meta["attendees"] = attendees
	}
	if organizer != "" {
		meta["organizer"] = organizer
	}
	// The local user's own address on THIS event (Participant.is_self). Mirrors the
	// key the Google connector emits from Attendee.Self, so the meeting brief can
	// exclude the user from their own meeting regardless of which calendar it came
	// from and which alias the invite used.
	if self != "" {
		meta["self_email"] = self
	}
	return memory.Item{
		Kind:       KindAppleCalEvent,
		ProviderID: providerID,
		Title:      summary,
		Body:       b.String(),
		OccurredAt: start,
		Tags:       []string{"calendar:" + strings.ToLower(calTitle)},
		Meta:       meta,
	}
}
