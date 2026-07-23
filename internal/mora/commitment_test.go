package mora

import (
	"context"
	"database/sql"
	"testing"
)

func TestCommitmentDueClassification(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		occurredAt string
		want       commitDue
	}{
		{
			name:       "explicit calendar date",
			text:       "Can you send the signed outline by July 14?",
			occurredAt: "2026-07-12T17:00:00Z",
			want:       commitDue{Kind: commitDueExplicitDate, At: "2026-07-14"},
		},
		{
			name:       "explicit calendar date never infers a clock",
			text:       "Can you send the signed outline by July 14 at 3:30 pm?",
			occurredAt: "2026-07-12T17:00:00Z",
			want:       commitDue{Kind: commitDueExplicitDate, At: "2026-07-14"},
		},
		{
			name:       "relative deadline",
			text:       "I will confirm the sample count tomorrow.",
			occurredAt: "2026-07-12T17:00:00Z",
			want:       commitDue{Kind: commitDueRelative},
		},
		{
			name:       "anchored relative deadline",
			text:       "Please send the route cards before the review.",
			occurredAt: "2026-07-12T17:00:00Z",
			want:       commitDue{Kind: commitDueRelative},
		},
		{
			name:       "urgency does not invent a deadline",
			text:       "Please send the receipt urgently.",
			occurredAt: "2026-07-12T17:00:00Z",
			want:       commitDue{Kind: commitDueNone},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyCommitmentDue(tt.text, tt.occurredAt); got != tt.want {
				t.Fatalf("due = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCommitmentDirectionTable(t *testing.T) {
	self := govAtom{Kind: atomAddress, Value: "self@example.com"}
	other := govAtom{Kind: atomAddress, Value: "other@example.com"}
	tests := []struct {
		name       string
		text       string
		author     govAtom
		addressee  govAtom
		reported   *govAtom
		wantOwner  govAtom
		wantDir    string
		wantExists bool
	}{
		{
			name: "self authored commitment", text: "I will send the outline.",
			author: self, addressee: other, wantOwner: self, wantDir: commitOwedBySelf, wantExists: true,
		},
		{
			name: "counterparty authored commitment", text: "I'll bring the room key.",
			author: other, addressee: self, wantOwner: other, wantDir: commitOwedByCounterparty, wantExists: true,
		},
		{
			name: "self request to counterparty", text: "Could you confirm the sample count?",
			author: self, addressee: other, wantOwner: other, wantDir: commitOwedByCounterparty, wantExists: true,
		},
		{
			name: "counterparty request to self", text: "Please send the receipt.",
			author: other, addressee: self, wantOwner: self, wantDir: commitOwedBySelf, wantExists: true,
		},
		{
			name: "reported speech follows actor", text: "Milo said he'll upload the selects for me.",
			author: self, addressee: other, reported: &other, wantOwner: other, wantDir: commitOwedByCounterparty, wantExists: true,
		},
		{
			name: "ambiguous addressee refuses", text: "Could you send the receipt?",
			author: other, wantExists: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, direction, ok := classifyCommitmentSpeech(tt.text, commitmentSpeechContext{
				Author: tt.author, Addressee: tt.addressee, Self: self,
				Counterparty: other, ReportedActor: tt.reported,
			})
			if ok != tt.wantExists {
				t.Fatalf("classified=%v, want %v (owner=%+v direction=%q)", ok, tt.wantExists, owner, direction)
			}
			if !ok {
				return
			}
			if !atomEqual(owner, tt.wantOwner) || direction != tt.wantDir {
				t.Fatalf("owner/direction = %+v/%q, want %+v/%q", owner, direction, tt.wantOwner, tt.wantDir)
			}
		})
	}
}

func TestCommitmentIDEvidenceOnly(t *testing.T) {
	const want = "commit:v1:10b7c665ae18290d686f4947d1afcf69240905e84a21df6eba0c5d36be2409c8"
	if got := commitmentID("memory#message", "block", 0); got != want {
		t.Fatalf("commitment id = %q, want %q", got, want)
	}
	if got := commitmentID("", "block", 0); got != "" {
		t.Fatalf("missing evidence minted id %q", got)
	}
}

func TestCommitmentsMaterializedByIndexGeneration(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "self@example.com",
		Enabled: ptr(true), CreatedAt: "2026-07-01T00:00:00Z",
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

	inventory, err := readCommitmentInventory(context.Background(), cfg)
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
