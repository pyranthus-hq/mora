package mora

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/memory"
	"strings"
	"testing"
)

func TestSourceAccountsRejectDuplicateAndMismatchedMailboxes(t *testing.T) {
	yes := true
	rows := []Source{{Name: "gmail", Type: "gmail", Email: "owner@example.test"}, {Name: "gmail-work", Type: "gmail", Account: "work", Email: "work@example.test"}}
	for i := range rows {
		rows[i].Enabled = &yes
	}
	checks := checkSourceAccounts(context.Background(), rows, func(context.Context, string) (string, error) { return "work@example.test", nil })
	for _, c := range checks {
		if c.State != "duplicate-account" {
			t.Fatalf("duplicate mailbox accepted: %+v", c)
		}
	}
	checks = checkSourceAccounts(context.Background(), rows[:1], func(context.Context, string) (string, error) { return "stranger@example.test", nil })
	if checks[0].State != "mismatch" {
		t.Fatal(checks)
	}
}

func TestSourceAccountsNeverInferIdentityOrExposeCredentialErrors(t *testing.T) {
	no := false
	rows := []Source{{Name: "disabled", Type: "gmail", Enabled: &no}, {Name: "good", Type: "gmail", Email: "owner@example.test"}, {Name: "unbound", Type: "gmail", Account: "work"}, {Name: "failed", Type: "gmail", Account: "bad"}, {Name: "calendar", Type: "calendar"}}
	yes := true
	for i := 1; i < len(rows); i++ {
		rows[i].Enabled = &yes
	}
	calls := 0
	checks := checkSourceAccounts(context.Background(), rows, func(_ context.Context, a string) (string, error) {
		calls++
		switch a {
		case "bad":
			return "", errors.New("sensitive token detail")
		case "work":
			return "work@example.test", nil
		default:
			return "owner@example.test", nil
		}
	})
	if calls != 3 || len(checks) != 4 {
		t.Fatal(calls, checks)
	}
	states := map[string]string{}
	for _, c := range checks {
		states[c.Name] = c.State
	}
	if states["disabled"] != "disabled" || states["good"] != "verified" || states["unbound"] != "unbound" || states["failed"] != "unavailable" {
		t.Fatal(states)
	}
	b, _ := json.Marshal(checks)
	if strings.Contains(string(b), "sensitive") {
		t.Fatal("raw error leaked")
	}
	if accountEmail("Person <owner@example.test>") != "" {
		t.Fatal("accepted display-name identity")
	}
}

func TestGoogleSyncRequiresExactDeclaredMailbox(t *testing.T) {
	for _, actual := range []string{"", "other@example.test", "Name <owner@example.test>"} {
		if requireExpectedGoogleAccount("owner@example.test", actual) == nil {
			t.Fatalf("accepted %q", actual)
		}
	}
	if err := requireExpectedGoogleAccount("OWNER@example.test", "owner@example.test"); err != nil {
		t.Fatal(err)
	}
}

func TestGoogleCursorCannotCrossAccountIdentity(t *testing.T) {
	for _, old := range []string{"", "other@example.test"} {
		st := &memory.SyncStatus{AccountEmail: old, Checkpoint: "old-page", IncrementalCursor: "old-history", LastSuccessAt: "old-success", LastSynced: "old-sync"}
		if !bindGoogleSyncStatus(st, "owner@example.test") || st.Checkpoint != "" || st.IncrementalCursor != "" || st.LastSuccessAt != "" || st.LastSynced != "" || st.LastError == "" {
			t.Fatalf("old mailbox cursor/freshness retained: %+v", st)
		}
		st.Checkpoint = "new-page"
		st.IncrementalCursor = "new-history"
		if bindGoogleSyncStatus(st, "owner@example.test") || st.Checkpoint != "new-page" || st.IncrementalCursor != "new-history" {
			t.Fatal("same-account resume reset")
		}
	}
}

func TestCalendarUsesSharedGoogleMailboxIdentityGuard(t *testing.T) {
	s := Source{Name: "calendar-work", Type: "calendar", Email: "owner@example.test"}
	for _, actual := range []string{"", "other@example.test"} {
		if err := verifyGoogleSyncIdentity(s, google.KindCalEvent, actual); err == nil {
			t.Fatalf("calendar accepted unavailable/mismatched identity %q", actual)
		}
	}
	if err := verifyGoogleSyncIdentity(s, google.KindCalEvent, "owner@example.test"); err != nil {
		t.Fatalf("calendar rejected verified identity: %v", err)
	}
	if err := verifyGoogleSyncIdentity(s, google.KindGmailThread, ""); err == nil {
		t.Fatal("gmail and calendar guard diverged on unavailable identity")
	}
}
