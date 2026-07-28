package applecal

import (
	"database/sql"
	"fmt"
	"regexp"
)

// sqlIdentifier matches a bare SQLite identifier. PRAGMA arguments cannot be
// bound as parameters, so any name interpolated into a PRAGMA statement must
// first pass this allowlist shape check — a guard, not an escape (#176).
var sqlIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// pragmaTableInfo is the single seam that interpolates a table name into a
// PRAGMA statement. Every caller today passes a compile-time literal; the
// guard exists so the concatenation can never carry hostile input if a future
// caller stops doing so. It refuses BEFORE touching the database.
func pragmaTableInfo(db *sql.DB, table string) (*sql.Rows, error) {
	if !sqlIdentifier.MatchString(table) {
		return nil, fmt.Errorf("refusing PRAGMA table_info on non-identifier %q", table)
	}
	return db.Query("PRAGMA table_info(" + table + ")")
}
