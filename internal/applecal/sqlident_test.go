package applecal

import (
	"strings"
	"testing"
)

// TestPragmaTableInfoRefusesHostileIdentifiers: the PRAGMA seam refuses
// anything not shaped like a bare identifier BEFORE touching the database —
// db is nil here, so reaching the query would panic instead of refuse.
func TestPragmaTableInfoRefusesHostileIdentifiers(t *testing.T) {
	for _, hostile := range []string{
		"",
		"CalendarItem); DROP TABLE CalendarItem;--",
		"CalendarItem)",
		"Calendar Item",
		"1Calendar",
		"Calendar;",
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
