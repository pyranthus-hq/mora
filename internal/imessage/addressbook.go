package imessage

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
)

// Resolver maps an iMessage handle (phone number or email) to a human-readable
// contact name, built ONCE from the macOS AddressBook (IMSG-04). On no match it
// returns the RAW handle verbatim — never a fabricated placeholder label (D-09). A missing
// or unreadable AddressBook degrades to an empty map: every handle then falls back
// to its raw form, which is correct-by-design, not a failure.
//
// The map is keyed by a NORMALIZED handle (phone: digits only; email: lowercased)
// so equivalent handle spellings ("+1 (415) 555-1234" vs "+14155551234") resolve to
// the same contact and email case differences collapse.
type Resolver struct {
	byHandle map[string]string // normalized handle → contact name
}

// addressBookFilename is the per-source AddressBook SQLite database file. There may
// be several Sources/<UUID>/ directories; the resolver iterates all of them.
const addressBookFilename = "AddressBook-v22.abcddb"

// DefaultAddressBookRoot is the macOS location of the per-source AddressBook DBs,
// FDA-gated exactly like chat.db. The wiring boundary expands ~ before passing it in.
func DefaultAddressBookRoot(home string) string {
	return filepath.Join(home, "Library", "Application Support", "AddressBook", "Sources")
}

// NewResolver builds the handle→name map ONCE by walking every
// Sources/<UUID>/AddressBook-v22.abcddb under abRoot and reading its contact
// phone/email rows read-only. It NEVER returns a fatal error for a missing or
// unreadable AddressBook: it returns a usable (possibly empty) resolver so ingest
// proceeds with all-raw-handles (D-09). A genuinely usable resolver is always
// returned; err is reserved for truly unexpected conditions and currently always nil.
func NewResolver(abRoot string) (*Resolver, error) {
	r := &Resolver{byHandle: map[string]string{}}

	entries, err := os.ReadDir(abRoot)
	if err != nil {
		// Missing/unreadable root (no AddressBook, FDA denied, non-macOS): degrade to
		// an empty resolver — every handle falls back to its raw form (D-09).
		return r, nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dbPath := filepath.Join(abRoot, e.Name(), addressBookFilename)
		if _, statErr := os.Stat(dbPath); statErr != nil {
			continue
		}
		// A per-source DB that cannot be opened/queried is skipped, never fatal — the
		// remaining sources (and the raw-handle fallback) still produce a correct run.
		loadAddressBookSource(dbPath, r.byHandle)
	}
	return r, nil
}

// newResolverFromMap builds a resolver directly from a known handle→name map,
// normalizing each seeded key. Used by the unit gate (no live DB) and as the shared
// construction path so seeded and DB-backed resolvers normalize identically.
func newResolverFromMap(byHandle map[string]string) *Resolver {
	r := &Resolver{byHandle: map[string]string{}}
	for h, name := range byHandle {
		if k := normalizeHandle(h); k != "" {
			r.byHandle[k] = name
		}
	}
	return r
}

// Lookup returns the resolved contact name and whether Address Book supplied it.
// A nil resolver, empty handle, absent mapping, and empty mapping value are unresolved.
// The lookup is O(1): the map was built once at construction.
func (r *Resolver) Lookup(handle string) (name string, ok bool) {
	if r == nil || handle == "" {
		return "", false
	}
	name, ok = r.byHandle[normalizeHandle(handle)]
	return name, ok && name != ""
}

// Resolve returns the contact name for a handle, or the raw handle when there is no
// match (D-09 — honest, traceable, never a fabricated placeholder). An empty handle returns "" (no
// fabrication). Use Lookup when the caller also needs to distinguish the raw-handle fallback.
func (r *Resolver) Resolve(handle string) string {
	if name, ok := r.Lookup(handle); ok {
		return name
	}
	return handle // D-09 raw-handle fallback
}

// normalizeHandle canonicalizes a handle for matching: an email (contains "@") is
// lowercased; anything else is treated as a phone number and reduced to its digits
// (dropping "+", spaces, parens, dashes) so equivalent spellings collide. A handle
// with no digits and no "@" (e.g. an opaque service id) normalizes to its lowercased
// self so it can still match an identically-seeded key without crashing.
func normalizeHandle(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	if strings.Contains(h, "@") {
		return strings.ToLower(h)
	}
	var digits strings.Builder
	for _, c := range h {
		if c >= '0' && c <= '9' {
			digits.WriteRune(c)
		}
	}
	if digits.Len() > 0 {
		return digits.String()
	}
	return strings.ToLower(h)
}

// loadAddressBookSource reads one AddressBook-v22.abcddb read-only and adds its
// phone/email → name rows to out. It is schema-defensive (the ZABCD* schema is
// private and version-variable, Assumption A4): it verifies the columns it needs via
// PRAGMA and silently degrades (adds nothing) on any mismatch or read failure, so a
// contact that cannot be resolved falls back to its raw handle (D-09) rather than
// aborting the build.
func loadAddressBookSource(dbPath string, out map[string]string) {
	db, err := sql.Open("sqlite", chatDBDSN(dbPath)) // reuse the mode=ro DSN form
	if err != nil {
		return
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return // FDA-denied / unreadable: skip this source
	}

	// Build the display name once per record: prefer first+last, fall back to
	// organization. ZABCDRECORD holds the name columns; ZABCDPHONENUMBER /
	// ZABCDEMAILADDRESS hold the handle values joined back via ZOWNER → Z_PK.
	if !addressBookColumnsPresent(db) {
		return
	}

	// ZNICKNAME is part of the private, version-variable ZABCD schema (A4), so it is
	// queried only when present — older AddressBooks select NULL and degrade to the
	// first/last/org name exactly as before (never failing the whole source). Two
	// complete literal queries instead of splicing the column name in (#176).
	recordQuery := `SELECT Z_PK, ZFIRSTNAME, ZLASTNAME, ZORGANIZATION, NULL FROM ZABCDRECORD`
	if addressBookHasColumn(db, "ZABCDRECORD", "ZNICKNAME") {
		recordQuery = `SELECT Z_PK, ZFIRSTNAME, ZLASTNAME, ZORGANIZATION, ZNICKNAME FROM ZABCDRECORD`
	}
	names := map[int64]string{}
	if rows, err := db.Query(recordQuery); err == nil {
		for rows.Next() {
			var (
				pk    int64
				first sql.NullString
				last  sql.NullString
				org   sql.NullString
				nick  sql.NullString
			)
			if err := rows.Scan(&pk, &first, &last, &org, &nick); err != nil {
				continue
			}
			if name := composeName(first, last, org, nick); name != "" {
				names[pk] = name
			}
		}
		rows.Close()
	}
	if len(names) == 0 {
		return
	}

	// Phone numbers.
	if rows, err := db.Query(`SELECT ZOWNER, ZFULLNUMBER FROM ZABCDPHONENUMBER`); err == nil {
		for rows.Next() {
			var (
				owner sql.NullInt64
				value sql.NullString
			)
			if err := rows.Scan(&owner, &value); err != nil {
				continue
			}
			addResolved(out, names, owner, value)
		}
		rows.Close()
	}

	// Email addresses.
	if rows, err := db.Query(`SELECT ZOWNER, ZADDRESS FROM ZABCDEMAILADDRESS`); err == nil {
		for rows.Next() {
			var (
				owner sql.NullInt64
				value sql.NullString
			)
			if err := rows.Scan(&owner, &value); err != nil {
				continue
			}
			addResolved(out, names, owner, value)
		}
		rows.Close()
	}
}

// addResolved joins one handle value to its owning record's name and stores it under
// the normalized handle key. A first-seen name wins (do not clobber across sources).
func addResolved(out map[string]string, names map[int64]string, owner sql.NullInt64, value sql.NullString) {
	if !owner.Valid || !value.Valid {
		return
	}
	name, ok := names[owner.Int64]
	if !ok || name == "" {
		return
	}
	key := normalizeHandle(value.String)
	if key == "" {
		return
	}
	if _, exists := out[key]; !exists {
		out[key] = name
	}
}

// composeName builds a display name from the AddressBook name columns: "First Last"
// when present, else organization, else the nickname, else "". The nickname fallback
// fixes contacts saved under ONLY a nickname (no first/last/org), which otherwise
// loaded as no name and surfaced as a raw phone number in `mora graph`.
func composeName(first, last, org, nick sql.NullString) string {
	var parts []string
	if first.Valid && strings.TrimSpace(first.String) != "" {
		parts = append(parts, strings.TrimSpace(first.String))
	}
	if last.Valid && strings.TrimSpace(last.String) != "" {
		parts = append(parts, strings.TrimSpace(last.String))
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	if org.Valid && strings.TrimSpace(org.String) != "" {
		return strings.TrimSpace(org.String)
	}
	if nick.Valid && strings.TrimSpace(nick.String) != "" {
		return strings.TrimSpace(nick.String)
	}
	return ""
}

// addressBookHasColumn reports whether table has the named column (PRAGMA
// table_info). Used to query the optional, version-variable ZNICKNAME column only
// when it exists, so older AddressBook schemas degrade cleanly instead of failing.
func addressBookHasColumn(db *sql.DB, table, col string) bool {
	rows, err := pragmaTableInfo(db, table)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      sql.NullString
			notNull    int
			dfltValue  sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &primaryKey); err != nil {
			return false
		}
		if name == col {
			return true
		}
	}
	return false
}

// addressBookColumnsPresent verifies the ZABCD* tables/columns the resolver reads
// exist (schema-defensive, A4). On any private-schema mismatch it returns false so
// loadAddressBookSource degrades to adding nothing (raw-handle fallback) instead of
// failing the whole build with a cryptic "no such table/column".
func addressBookColumnsPresent(db *sql.DB) bool {
	required := map[string][]string{
		"ZABCDRECORD":       {"Z_PK", "ZFIRSTNAME", "ZLASTNAME", "ZORGANIZATION"},
		"ZABCDPHONENUMBER":  {"ZOWNER", "ZFULLNUMBER"},
		"ZABCDEMAILADDRESS": {"ZOWNER", "ZADDRESS"},
	}
	for table, cols := range required {
		have := map[string]bool{}
		rows, err := pragmaTableInfo(db, table)
		if err != nil {
			return false
		}
		for rows.Next() {
			var (
				cid        int
				name       string
				ctype      sql.NullString
				notNull    int
				dfltValue  sql.NullString
				primaryKey int
			)
			if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &primaryKey); err != nil {
				rows.Close()
				return false
			}
			have[name] = true
		}
		rows.Close()
		for _, c := range cols {
			if !have[c] {
				return false
			}
		}
	}
	return true
}
