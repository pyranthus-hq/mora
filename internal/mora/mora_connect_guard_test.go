package mora

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// TestGoogleAccountForEmail locks the same-account re-auth guard's lookup:
// match by stamped email across labels (case-insensitive), no match for
// unknown addresses or empty input — connecting one mailbox under two labels
// would double-ingest every thread.
func TestGoogleAccountForEmail(t *testing.T) {
	sources := []Source{
		{Name: "gmail", Type: "gmail", Account: "", Email: "alex.owner@gmail.com"},
		{Name: "calendar", Type: "calendar", Account: "", Email: "alex.owner@gmail.com"},
		{Name: "gmail-pyranthus", Type: "gmail", Account: "pyranthus", Email: "alex@example-co.com"},
		{Name: "docs", Type: "filesystem", Email: "ignored@nowhere.com"}, // non-google rows never match
	}
	if label, ok := googleAccountForEmail(sources, "Alex.Owner@GMAIL.com"); !ok || label != "" {
		t.Fatalf("default-account email: label=%q ok=%v", label, ok)
	}
	if label, ok := googleAccountForEmail(sources, "alex@example-co.com"); !ok || label != "pyranthus" {
		t.Fatalf("labeled-account email: label=%q ok=%v", label, ok)
	}
	if _, ok := googleAccountForEmail(sources, "new@person.com"); ok {
		t.Fatalf("unknown email must not match")
	}
	if _, ok := googleAccountForEmail(sources, ""); ok {
		t.Fatalf("empty email must not match")
	}
	if _, ok := googleAccountForEmail(sources, "ignored@nowhere.com"); ok {
		t.Fatalf("non-google source emails must not match")
	}
}

// TestSourceFreshlySynced locks the connect-path skip guard: a clean sync
// inside the window skips the re-pull; stale, never-synced, or failed-attempt
// (no LastSuccessAt) sources do not.
func TestSourceFreshlySynced(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.RFC3339, "2026-06-10T12:00:00Z")
	s := Source{Name: "gmail", Type: "gmail"}
	if sourceFreshlySynced(cfg, s, time.Hour, now) {
		t.Fatalf("never-synced must not read as fresh")
	}
	write := func(successAt string) {
		st := &memory.SyncStatus{Source: "gmail", LastSynced: successAt, LastSuccessAt: successAt}
		if err := memory.SaveStatus(filepath.Join(cfg.StateDir, "sync", "google-gmail.json"), st); err != nil {
			t.Fatal(err)
		}
	}
	write("2026-06-10T11:45:00Z") // 15 minutes ago
	if !sourceFreshlySynced(cfg, s, time.Hour, now) {
		t.Fatalf("15m-old clean sync must read fresh within 1h")
	}
	write("2026-06-10T09:00:00Z") // 3 hours ago
	if sourceFreshlySynced(cfg, s, time.Hour, now) {
		t.Fatalf("3h-old sync must not read fresh within 1h")
	}
}
