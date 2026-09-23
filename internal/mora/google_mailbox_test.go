package mora

import (
	"testing"

	"github.com/pyranthus-hq/mora/internal/genericutil"
)

func TestGmailMailboxOwnerUsesObservedMailboxAndFetchSelection(t *testing.T) {
	first := Source{Name: "gmail", Type: "gmail", Email: "First@Example.com", LabelIDs: []string{"INBOX", "STARRED"}, Enabled: genericutil.Ptr(true)}
	second := Source{Name: "gmail-work", Type: "gmail", Email: "first@example.com", LabelIDs: []string{"STARRED", "INBOX"}, SinceDays: 90, Enabled: genericutil.Ptr(true)}
	if owner, found := gmailMailboxOwner([]Source{first, second}, second, "first@example.com"); !found || owner.Name != first.Name {
		t.Fatalf("same mailbox and read selection must have one owner: %+v, %v", owner, found)
	}
	if _, found := gmailMailboxOwner([]Source{first, second}, first, "first@example.com"); found {
		t.Fatal("first claimant must remain able to sync")
	}
	if _, found := gmailMailboxOwner([]Source{first, second}, second, "other@example.com"); found {
		t.Fatal("a different observed mailbox must not inherit another account's claim")
	}
	second.LabelIDs = []string{"IMPORTANT"}
	if _, found := gmailMailboxOwner([]Source{first, second}, second, "first@example.com"); found {
		t.Fatal("disjoint label selection may read the same mailbox")
	}
	second.LabelIDs = []string{"STARRED", "INBOX"}
	first.Enabled = genericutil.Ptr(false)
	if _, found := gmailMailboxOwner([]Source{first, second}, second, "first@example.com"); found {
		t.Fatal("a disabled source cannot own a mailbox")
	}
}
