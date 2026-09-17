package mora

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/memory"
	"os"
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
	st := &memory.SyncStatus{AccountEmail: "other@example.test", Checkpoint: "old-page", CursorMode: memory.CursorModeHistory, PendingSyncCursor: "old-pending", IncrementalCursor: "old-history", LastSuccessAt: "old-success", LastSynced: "old-sync"}
	if !bindGoogleSyncStatus(st, "owner@example.test") || st.Checkpoint != "" || st.IncrementalCursor != "" || st.LastSuccessAt != "" || st.LastSynced != "" || st.LastError == "" {
		t.Fatalf("old mailbox cursor/freshness retained: %+v", st)
	}
	// The staged baseline and the mode that describes the checkpoint belong to
	// the previous mailbox just as much as the cursor does. Leaving either
	// behind lets the next completed walk promote a foreign history id.
	if st.PendingSyncCursor != "" || st.CursorMode != "" {
		t.Fatalf("old mailbox resume state retained: %+v", st)
	}
	st.Checkpoint = "new-page"
	st.IncrementalCursor = "new-history"
	if bindGoogleSyncStatus(st, "owner@example.test") || st.Checkpoint != "new-page" || st.IncrementalCursor != "new-history" {
		t.Fatal("same-account resume reset")
	}
}

func TestGoogleBindEmptyAccountEmailKeepsCursor(t *testing.T) {
	st := &memory.SyncStatus{Checkpoint: "page", IncrementalCursor: "history", LastSuccessAt: "ok", LastSynced: "ok"}
	if !bindGoogleSyncStatus(st, "owner@example.test") {
		t.Fatal("empty account_email should bind")
	}
	if st.AccountEmail != "owner@example.test" || st.Checkpoint != "page" || st.IncrementalCursor != "history" || st.LastSuccessAt != "ok" {
		t.Fatalf("legacy persist omit wiped a live cursor: %+v", st)
	}
	if bindGoogleSyncStatus(st, "owner@example.test") {
		t.Fatal("same-account rebind should be a no-op")
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

func TestCanonicalizeGmailMappedRefsOnlyQualifiesOwnParent(t *testing.T) {
	mm := memory.MappedMemory{StableID: "gmail_thread/t", Provider: "gmail", Title: "t", Body: "body", Meta: map[string]any{
		"messages": []map[string]any{{"message_ref": "gmail_thread/t#one"}, {"message_ref": "gmail_thread/other#two"}, {"message_ref": "gmail_thread/t#"}},
	}}
	mm.ContentHash = "legacy-hash"
	canonicalizeGmailMappedRefs(&mm, "work")
	b, _ := json.Marshal(mm.Meta["messages"])
	var rows []map[string]any
	_ = json.Unmarshal(b, &rows)
	if rows[0]["message_ref"] != "gmail_thread/t@work#one" || rows[1]["message_ref"] != "gmail_thread/other#two" || rows[2]["message_ref"] != "gmail_thread/t#" || mm.ContentHash != "legacy-hash" {
		t.Fatalf("unexpected canonical refs: %+v", rows)
	}
}

func TestCanonicalizedAccountGmailHashSkipsExistingMarkdown(t *testing.T) {
	cfg := gate2Vault(t)
	mm := memory.MappedMemory{StableID: "gmail_thread/t", Account: "work", Scope: "global", Type: "email", Title: "t", Body: "body", Provider: "gmail", Source: "t", ContentHash: "legacy-hash", Meta: map[string]any{"messages": []map[string]any{{"message_ref": "gmail_thread/t#one"}}}}
	canonicalizeGmailMappedRefs(&mm, "work")
	mm.StableID += "@work"
	if wrote, err := writeMappedMemoryDetailed(cfg, mm); err != nil || !wrote {
		t.Fatalf("initial write=%t err=%v", wrote, err)
	}
	files, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := ""
	for _, candidate := range files {
		if strings.Contains(candidate, "/sources/gmail/") {
			path = candidate
			break
		}
	}
	if path == "" {
		t.Fatalf("gmail file absent: %v", files)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if wrote, err := writeMappedMemoryDetailed(cfg, mm); err != nil || wrote {
		t.Fatalf("hash skip write=%t err=%v", wrote, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("hash-skipped account Gmail changed Markdown")
	}
}
