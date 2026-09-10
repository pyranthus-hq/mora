package mora

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/segments"
	"reflect"
	"testing"
	"time"
)

func TestActivitySegmentArmFiltersBeforeParentPool(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 62; i++ {
		id := fmt.Sprintf("gmail_thread/decoy-%02d", i)
		at := now.Add(-48 * time.Hour)
		body := "segmentprobe segmentprobe segmentprobe"
		if i == 60 {
			id = "gmail_thread/excluded"
			at = now.Add(-time.Hour)
		}
		if i == 61 {
			id = "gmail_thread/kept"
			at = now.Add(-time.Hour)
			body = "weak segmentprobe mention with extra unrelated words"
		}
		sender := "person@example.test"
		m := Memory{ID: id, Scope: "global", Type: "email", Provider: "gmail", Source: "gmail", Title: id, CreatedAt: now.Format(time.RFC3339), Text: gmailSegJoinBody([2]string{sender, body}), Meta: map[string]any{"messages": gmailSegMessages(commitmentMessageEvidence{MessageRef: id + "#message", Sender: sender, At: at.Format(time.RFC3339), BlockRefs: []string{"body"}})}}
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	correction := Memory{ID: "correction", Scope: "global", Type: "correction", Source: "manual", Title: "explicit correction", Text: "set aside", CreatedAt: now.Format(time.RFC3339), Meta: map[string]any{"target": "gmail_thread/excluded", "disposition": "not-context"}}
	if err := writeMemory(cfg, correction); err != nil {
		t.Fatal(err)
	}
	mustRebuild(t, cfg)
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f, err := prepareDispositionFilter(cfg, searchFilters{Now: now, EventSinceHours: 24, ExcludeDispositions: []string{"not-context"}})
	if err != nil {
		t.Fatal(err)
	}
	ids, _, err := segments.Query(context.Background(), db, "segmentprobe", "global", 1, f, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"gmail_thread/kept"}) {
		t.Fatalf("segment filters applied after pool: %v", ids)
	}
}
