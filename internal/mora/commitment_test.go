package mora

import (
	"context"
	"database/sql"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"strings"
	"testing"
	"time"
)

// obligations-v2 says the user's own clear promise to another person belongs in
// owed_by_self. These invented notes exercise that rule without borrowing frozen
// fixture wording, plus the "concrete future action" near-miss boundary.

func TestWindowDigestSurfacesManualPromiseOnly(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	cfg.SelfEmails = []string{"self@example.com"}
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	for _, memory := range []Memory{
		{
			ID: "invented-note-promise", Scope: "global", Type: "note",
			Title: "Borrowed equipment", Source: "manual",
			CreatedAt: "2026-08-03T09:00:00Z",
			Text:      "I told Jordan I'd return the borrowed lens before the workshop.",
		},
		{
			ID: "invented-note-past-report", Scope: "global", Type: "note",
			Title: "Equipment history", Source: "manual",
			CreatedAt: "2026-08-03T09:05:00Z",
			Text:      "I told Jordan I'd already returned the borrowed lens before the workshop.",
		},
	} {
		if err := writeMemory(cfg, memory); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	digest, err := buildDigest(cfg, at, briefOpts{sinceHours: 24 * 7, perSourceCap: 10})
	if err != nil {
		t.Fatal(err)
	}
	var surfaced []string
	var promiseItem *DigestItem
	for _, section := range digest.Sections {
		for i := range section.Items {
			item := section.Items[i]
			surfaced = append(surfaced, item.ID)
			if item.ID == "invented-note-promise" {
				promiseItem = &item
			}
		}
	}
	if !containsStringFold(surfaced, "invented-note-promise") {
		t.Fatalf("clear manual promise did not reach DAILY: %v", surfaced)
	}
	if containsStringFold(surfaced, "invented-note-past-report") {
		t.Fatalf("past-completion near miss reached DAILY: %v", surfaced)
	}
	if promiseItem == nil || promiseItem.CounterpartyLabel != "Jordan" ||
		!strings.Contains(renderDigestItemLine(*promiseItem), "counterparty=Jordan") {
		t.Fatalf("manual promise label did not render as plain attribution: %+v", promiseItem)
	}
}

func TestCommitmentCounterpartyExcludesExplicitSelfParticipant(t *testing.T) {
	cfg := Config{SelfEmails: []string{"mira.sen@example.com"}}
	m := Memory{
		Provider: "imessage",
		Meta: map[string]any{"participants": []map[string]string{
			{"handle": "+15550100100", "name": "Mira Sen"},
			{"handle": "+15550100104", "name": "Lucia Wynn"},
		}},
	}
	got, ok := commitmentCounterparty(m, cfg)
	if !ok || got.Kind != atomHandle || got.Value != "+15550100104" {
		t.Fatalf("counterparty = %+v, %v, want Lucia's handle", got, ok)
	}
}

func TestCommitmentCounterpartyDoesNotExcludePartialSelfNameMatch(t *testing.T) {
	cfg := Config{SelfEmails: []string{"mira.sen@example.com"}}
	m := Memory{
		Provider: "imessage",
		Meta: map[string]any{"participants": []map[string]string{
			{"handle": "+15550100100", "name": "Mira Patel"},
			{"handle": "+15550100104", "name": "Lucia Wynn"},
		}},
	}
	if got, ok := commitmentCounterparty(m, cfg); ok {
		t.Fatalf("ambiguous participants resolved to %+v; a partial self-name match must fail closed", got)
	}
}

func TestCommitmentsMaterializedByIndexGeneration(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "self@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: "2026-07-01T00:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}
	m := Memory{
		ID: "gmail_thread/materialized", Scope: "global", Type: "email",
		Title: "Outline", Source: "materialized", Provider: "gmail", ProviderID: "materialized",
		CreatedAt: "2026-07-20T10:00:00Z",
		Text:      "From: Other <other@example.com>\n\nPlease send the signed outline.",
		Meta: map[string]any{
			"from": []string{"other@example.com"},
			"to":   []string{"self@example.com"},
			"messages": []commitmentMessageEvidence{{
				MessageRef: "gmail_thread/materialized#msg-1",
				Sender:     "other@example.com", To: []string{"self@example.com"},
				At: "2026-07-20T10:00:00Z", BlockRefs: []string{"body"},
			}},
		},
	}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rowGeneration, metaGeneration, id string
	if err := db.QueryRow(`SELECT generation, commitment_id FROM commitments`).Scan(&rowGeneration, &id); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT value FROM index_meta WHERE key='commitments_generation'`).Scan(&metaGeneration); err != nil {
		t.Fatal(err)
	}
	if rowGeneration == "" || rowGeneration != metaGeneration {
		t.Fatalf("row generation %q, meta generation %q", rowGeneration, metaGeneration)
	}
	wantID := commitmentID("gmail_thread/materialized#msg-1", "body", 0)
	if id != wantID {
		t.Fatalf("commitment id = %q, want %q", id, wantID)
	}

	inventory, err := readCommitmentInventory(context.Background(), cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := inventory[m.ID]
	if len(got) != 1 {
		t.Fatalf("inventory[%q] = %+v, want one commitment", m.ID, got)
	}
	if got[0].Direction != commitOwedBySelf || !atomEqual(got[0].Owner, canonicalSelfAtom(cfg, "self@example.com")) {
		t.Fatalf("typed commitment = %+v", got[0])
	}
	if got[0].Due != (commitDue{Kind: commitDueNone}) {
		t.Fatalf("due = %+v, want none", got[0].Due)
	}
}

func TestCommitmentClassificationRejectsThirdPartyAssignment(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "self@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: "2026-07-01T00:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}
	m := Memory{
		ID: "gmail_thread/third-party", Scope: "global", Type: "email",
		Title: "Next steps", Source: "third-party", Provider: "gmail", ProviderID: "third-party",
		CreatedAt: "2026-07-20T10:00:00Z",
		Text:      "From: Other <other@example.com>\n\nAction item for Kim: Please share the findings before kickoff.",
		Meta: map[string]any{
			"from": []string{"other@example.com"},
			"to":   []string{"self@example.com"},
		},
	}
	if got := classifyCommitments(m, cfg); len(got) != 0 {
		t.Fatalf("third-party assignment materialized as the user's commitment: %+v", got)
	}
}
