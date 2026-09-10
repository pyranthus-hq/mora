package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pyranthus-hq/mora/internal/activity"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// activityStampVersion identifies the serialized projection contract. Stamps are
// disposable index data; a later version is rebuilt from Markdown, never trusted
// as a vault fact.
const activityStampVersion = 1

var activityStampSchemaStmts = []string{
	`DROP TABLE IF EXISTS activity_stamps`,
	`CREATE TABLE IF NOT EXISTS activity_stamps (
        memory_id TEXT PRIMARY KEY,
        scope TEXT,
        event_at TEXT NULL,
        event_at_unix INTEGER NULL,
        event_at_nanos INTEGER NULL CHECK(event_at_nanos BETWEEN 0 AND 999999999),
        participation_json,
        automated INTEGER NULL CHECK(automated IN (0,1)),
        automation_basis,
        version
    )`,
	`CREATE INDEX IF NOT EXISTS idx_activity_stamps_scope_event ON activity_stamps(scope, event_at_unix, event_at_nanos)`,
	`CREATE INDEX IF NOT EXISTS idx_activity_stamps_event ON activity_stamps(event_at_unix, event_at_nanos)`,
}

func activityStampSupported(m Memory) bool {
	switch providerToType(m.Provider) {
	case "gmail", "imessage", "whatsapp", "calendar", "applecalendar":
		return true
	default:
		return false
	}
}

func writeActivityStamp(ctx context.Context, stmt *sql.Stmt, m Memory) error {
	if !activityStampSupported(m) {
		return nil
	}
	p := activity.Derive(m, time.Now())
	var eventAt any
	var unix, nanos any
	if p.EventAt != nil {
		at := p.EventAt.UTC()
		eventAt, unix, nanos = at.Format(time.RFC3339Nano), at.Unix(), at.Nanosecond()
	}
	var participation any
	if p.Participation != nil {
		b, err := json.Marshal(p.Participation)
		if err != nil {
			return err
		}
		participation = string(b)
	}
	var automated any
	if p.Automated != nil {
		if *p.Automated {
			automated = 1
		} else {
			automated = 0
		}
	}
	basis := p.AutomationBasis
	_, err := stmt.ExecContext(ctx, m.ID, m.Scope, eventAt, unix, nanos, participation, automated, basis, activityStampVersion)
	return err
}

func prepareActivityStampStmt(ctx context.Context, tx *sql.Tx) (*sql.Stmt, error) {
	return tx.PrepareContext(ctx, `INSERT OR REPLACE INTO activity_stamps
        (memory_id,scope,event_at,event_at_unix,event_at_nanos,participation_json,automated,automation_basis,version)
        VALUES (?,?,?,?,?,?,?,?,?)`)
}

// overlayActivityStamps restores derived index facts after Markdown hydration.
// Invalid rows are ignored rather than becoming a source of fabricated activity.
func overlayActivityStamps(ctx context.Context, db *sql.DB, mems []Memory) error {
	for i := range mems {
		var eventText, participationText, basis sql.NullString
		var unix, nanos, automated sql.NullInt64
		var version sql.NullInt64
		err := db.QueryRowContext(ctx, `SELECT event_at,event_at_unix,event_at_nanos,participation_json,automated,automation_basis,version FROM activity_stamps WHERE memory_id=? AND scope=?`, mems[i].ID, mems[i].Scope).
			Scan(&eventText, &unix, &nanos, &participationText, &automated, &basis, &version)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return fmt.Errorf("read activity stamp: %w", err)
		}
		if version.Int64 != activityStampVersion {
			continue
		}
		if eventText.Valid && unix.Valid && nanos.Valid && nanos.Int64 >= 0 && nanos.Int64 <= 999999999 {
			at, err := time.Parse(time.RFC3339Nano, eventText.String)
			if err == nil && at.Unix() == unix.Int64 && int64(at.Nanosecond()) == nanos.Int64 {
				mems[i].EventAt = at.UTC().Format(time.RFC3339Nano)
			}
		}
		if participationText.Valid {
			var p memory.Participation
			if json.Unmarshal([]byte(participationText.String), &p) == nil && p.MessageEvidenceCount >= 0 {
				mems[i].Participation = &p
			}
		}
		if automated.Valid && (automated.Int64 == 0 || automated.Int64 == 1) {
			value := automated.Int64 == 1
			mems[i].Automated = &memory.NullableBool{Value: &value}
		}
		// basis is deliberately not exposed in Memory yet; keeping it in the
		// index supports doctor coverage without leaking message content.
		_ = basis
	}
	return nil
}
