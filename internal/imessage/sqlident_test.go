package imessage

import (
	"database/sql"
	"strings"
	"testing"
)

// TestPragmaTableInfoRefusesHostileIdentifiers: the PRAGMA seam refuses
// anything not shaped like a bare identifier BEFORE touching the database —
// db is nil here, so reaching the query would panic instead of refuse.
func TestPragmaTableInfoRefusesHostileIdentifiers(t *testing.T) {
	for _, hostile := range []string{
		"",
		"message); DROP TABLE message;--",
		"message)",
		`message"`,
		"message name",
		"1message",
		"message;",
		"m.essage",
	} {
		rows, err := pragmaTableInfo(nil, hostile)
		if err == nil {
			rows.Close()
			t.Fatalf("pragmaTableInfo(%q) accepted a non-identifier", hostile)
		}
		if !strings.Contains(err.Error(), "refusing PRAGMA") {
			t.Fatalf("pragmaTableInfo(%q) error = %q, want refusal", hostile, err)
		}
	}
}

// TestPragmaTableInfoAcceptsRealTable: the guard passes a well-formed name
// through to the database and the rows are usable.
func TestPragmaTableInfoAcceptsRealTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE message (ROWID INTEGER PRIMARY KEY, text TEXT)`); err != nil {
		t.Fatal(err)
	}
	rows, err := pragmaTableInfo(db, "message")
	if err != nil {
		t.Fatalf("pragmaTableInfo(message) = %v", err)
	}
	defer rows.Close()
	cols := 0
	for rows.Next() {
		cols++
	}
	if cols != 2 {
		t.Fatalf("table_info(message) rows = %d, want 2", cols)
	}
}

// TestOptionalColumnConstantsAreIdentifiers pins the splice guard in
// conversationMessages: the optional-column constants must stay bare
// identifiers, or the query silently degrades them to literal 0.
func TestOptionalColumnConstantsAreIdentifiers(t *testing.T) {
	for _, c := range []string{colItemType, colDateRetracted} {
		if !sqlIdentifier.MatchString(c) {
			t.Fatalf("optional column constant %q is not a bare identifier", c)
		}
	}
}
