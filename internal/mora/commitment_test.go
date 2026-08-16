package mora

import (
	"context"
	"database/sql"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"strings"
	"testing"
	"time"
)

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
		wantDir    Direction
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

// obligations-v2 says the user's own clear promise to another person belongs in
// owed_by_self. These invented notes exercise that rule without borrowing frozen
// fixture wording, plus the "concrete future action" near-miss boundary.
func TestManualAuthoredPromiseMaterialization(t *testing.T) {
	cfg := Config{SelfEmails: []string{"self@example.com"}}
	memory := Memory{
		ID:        "invented-note-promise",
		Type:      "note",
		Source:    "manual",
		CreatedAt: "2026-08-03T09:00:00Z",
		Text:      "I told Jordan I'd return the borrowed lens before the workshop.",
	}
	got := classifyCommitments(memory, cfg)
	if len(got) != 1 {
		t.Fatalf("commitments = %+v, want one clear authored promise", got)
	}
	commitment := got[0]
	if !atomEqual(commitment.Owner, canonicalSelfAtom(cfg, "")) ||
		commitment.Direction != commitOwedBySelf ||
		commitment.Due != (commitDue{Kind: commitDueRelative}) {
		t.Fatalf("typed commitment = %+v, want self-owned relative promise", commitment)
	}
	if atomPresent(commitment.Counterparty) {
		t.Fatalf("counterparty = %+v, want an honest gap without source-native identity metadata", commitment.Counterparty)
	}
	if commitment.CounterpartyLabel != "Jordan" {
		t.Fatalf("counterparty label = %q, want source-authored addressee", commitment.CounterpartyLabel)
	}
	if commitment.ID != "" {
		t.Fatalf("commitment id = %q, want no fabricated immutable evidence id", commitment.ID)
	}

	memory.ID = "invented-note-would-promise"
	memory.Text = "I told Rowan Vale I would return the borrowed lens before the workshop."
	got = classifyCommitments(memory, cfg)
	if len(got) != 1 || got[0].CounterpartyLabel != "Rowan Vale" ||
		atomPresent(got[0].Counterparty) {
		t.Fatalf("explicit would-promise = %+v, want name-grain label without identity atom", got)
	}

	memory.ID = "invented-note-past-report"
	memory.Text = "I told Jordan I'd already returned the borrowed lens before the workshop."
	if got := classifyCommitments(memory, cfg); len(got) != 0 {
		t.Fatalf("past-completion near miss became future work: %+v", got)
	}
}

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

func TestGmailCommitmentUsesAuthoredPrefixBlockRef(t *testing.T) {
	cfg := Config{SelfEmails: []string{"self@example.com"}}
	for _, tt := range []struct {
		name      string
		body      string
		blockRefs []string
		want      int
	}{
		{
			name: "footer after authored ask",
			body: "Could you deliver the calibration sheet tomorrow?\n\n" +
				"Manage workshop notices or update your preferences.",
			blockRefs: []string{"authored-body", "footer"},
			want:      1,
		},
		{
			name: "forward after authored ask",
			body: "Could you deliver the calibration sheet tomorrow?\n\n" +
				"---------- Forwarded message ---------\nReserve a seat today.",
			blockRefs: []string{"authored-body", "forward"},
			want:      1,
		},
		{
			name:      "bare forward is not authored evidence",
			body:      "---------- Forwarded message ---------\nReserve a seat today.",
			blockRefs: []string{"forward"},
			want:      0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			memory := Memory{
				ID: "gmail_thread/invented-structured", Type: "email", Provider: "gmail",
				Source: "invented-structured", CreatedAt: "2026-08-01T10:00:00Z",
				Text: "From: Other <other@example.com>\n\n" + tt.body,
				Meta: map[string]any{
					"from": []string{"other@example.com"},
					"to":   []string{"self@example.com"},
					"messages": []commitmentMessageEvidence{{
						MessageRef: "gmail_thread/invented-structured#message-1",
						Sender:     "other@example.com",
						To:         []string{"self@example.com"},
						At:         "2026-08-01T10:00:00Z",
						BlockRefs:  tt.blockRefs,
					}},
				},
			}
			got := classifyCommitments(memory, cfg)
			if len(got) != tt.want {
				t.Fatalf("commitments = %+v, want %d", got, tt.want)
			}
			if tt.want == 0 {
				return
			}
			wantID := commitmentID("gmail_thread/invented-structured#message-1", "authored-body", 0)
			if got[0].ID != wantID || got[0].OpenedBy.BlockRef != "authored-body" {
				t.Fatalf("opening identity = %+v, want authored prefix %q", got[0].OpenedBy, wantID)
			}
		})
	}
}

func TestAcceptedRequestDoesNotCreateExtraWork(t *testing.T) {
	cfg := Config{SelfEmails: []string{"self@example.com"}}
	memory := func(reply string) Memory {
		return Memory{
			ID: "gmail_thread/invented-acceptance", Type: "email", Provider: "gmail",
			Source: "invented-acceptance", CreatedAt: "2026-08-01T10:30:00Z",
			Text: "From: Self <self@example.com>\n\n" +
				"Could you reserve the calibration bench for the sensor run?\n\n---\n\n" + reply,
			Meta: map[string]any{
				"from": []string{"self@example.com", "other@example.com"},
				"to":   []string{"self@example.com", "other@example.com"},
				"messages": []commitmentMessageEvidence{
					{
						MessageRef: "gmail_thread/invented-acceptance#ask",
						Sender:     "self@example.com", To: []string{"other@example.com"},
						At: "2026-08-01T10:00:00Z", BlockRefs: []string{"ask-body"},
					},
					{
						MessageRef: "gmail_thread/invented-acceptance#reply",
						Sender:     "other@example.com", To: []string{"self@example.com"},
						At: "2026-08-01T10:30:00Z", BlockRefs: []string{"reply-body"},
					},
				},
			},
		}
	}

	got := classifyCommitments(memory(
		"I will hold the calibration bench. Please send me the sensor checklist before the review.",
	), cfg)
	if len(got) != 2 {
		t.Fatalf("accepted request produced extra work: %+v", got)
	}
	wantIDs := map[string]bool{
		commitmentID("gmail_thread/invented-acceptance#ask", "ask-body", 0):     true,
		commitmentID("gmail_thread/invented-acceptance#reply", "reply-body", 0): true,
	}
	for _, commitment := range got {
		if !wantIDs[commitment.ID] {
			t.Fatalf("commitment id %q shows an acceptance consumed a slot: %+v", commitment.ID, got)
		}
	}

	nearMiss := classifyCommitments(memory(
		"I will prepare the lighting budget. Please send me the sensor checklist before the review.",
	), cfg)
	if len(nearMiss) != 3 {
		t.Fatalf("materially changed action collapsed into the earlier request: %+v", nearMiss)
	}
}

func TestIMessageAcceptanceDoesNotCreateExtraWork(t *testing.T) {
	cfg := Config{SelfEmails: []string{"self@example.com"}}
	memory := func(reply string) Memory {
		return Memory{
			ID: "imessage_chat/invented-acceptance", Type: "imessage", Provider: "imessage",
			Source: "invented-acceptance", CreatedAt: "2026-08-01T10:00:00Z",
			Text: "Me: Would you send me the calibration code?\nLucia: " + reply,
			Meta: map[string]any{"participants": []map[string]string{{
				"handle": "+15550100104", "name": "Lucia Wynn",
			}}},
		}
	}

	got := classifyCommitments(memory("Yes, I will text you the calibration code when I reach the desk."), cfg)
	if len(got) != 1 || got[0].Direction != commitOwedByCounterparty ||
		got[0].Due != (commitDue{Kind: commitDueRelative}) ||
		got[0].Counterparty.Value != "+15550100104" {
		t.Fatalf("accepted iMessage request = %+v, want one due-enriched obligation", got)
	}

	if nearMiss := classifyCommitments(memory("I will prepare the lighting budget tomorrow."), cfg); len(nearMiss) != 2 {
		t.Fatalf("materially changed iMessage action collapsed into the request: %+v", nearMiss)
	}
}

func TestReportedThirdPartyPromiseRequiresNamedSelfBeneficiary(t *testing.T) {
	cfg := Config{SelfEmails: []string{"ava@example.com"}}
	memory := func(beneficiary string) Memory {
		body := "Rhea said, “I will bring " + beneficiary + " the sealed envelope before the review.”"
		return Memory{
			ID: "gmail_thread/invented-relay", Type: "email", Provider: "gmail",
			Source: "invented-relay", CreatedAt: "2026-08-01T10:00:00Z",
			Text: "From: Jordan <jordan@example.net>\n\n" + body,
			Meta: map[string]any{
				"from": []string{"jordan@example.net"},
				"to":   []string{"ava@example.com"},
				"cc":   []string{"rhea@example.org"},
				"names": map[string]string{
					"ava@example.com":    "Ava Stone",
					"jordan@example.net": "Jordan Vale",
					"rhea@example.org":   "Rhea North",
				},
				"messages": []commitmentMessageEvidence{{
					MessageRef: "gmail_thread/invented-relay#message-1",
					Sender:     "jordan@example.net",
					To:         []string{"ava@example.com"},
					Cc:         []string{"rhea@example.org"},
					At:         "2026-08-01T10:00:00Z",
					BlockRefs:  []string{"relay-body"},
				}},
			},
		}
	}

	got := classifyCommitments(memory("Ava"), cfg)
	wantActor := govAtom{Kind: atomAddress, Value: "rhea@example.org"}
	if len(got) != 1 || !atomEqual(got[0].Owner, wantActor) ||
		!atomEqual(got[0].Counterparty, wantActor) ||
		got[0].Direction != commitOwedByCounterparty {
		t.Fatalf("safe reported promise = %+v, want Rhea owing Ava", got)
	}

	if got := classifyCommitments(memory("Morgan"), cfg); len(got) != 0 {
		t.Fatalf("third-party-only reported promise became the user's loop: %+v", got)
	}
}

func TestAuthoredDeliveryMaterializesFulfilledQuotedRequest(t *testing.T) {
	cfg := Config{SelfEmails: []string{"self@example.com"}}
	memory := func(delivery string) Memory {
		return Memory{
			ID: "gmail_thread/invented-fulfilled-quote", Type: "email", Provider: "gmail",
			Source: "invented-fulfilled-quote", CreatedAt: "2026-08-01T10:00:00Z",
			Text: "From: Self <self@example.com>\n\n" + delivery +
				"\n\nOn Fri, Aug 1, Rowan Vale wrote:\n> Could you send me the signed access sheet before noon?",
			Meta: map[string]any{
				"from": []string{"self@example.com"},
				"to":   []string{"rowan@example.org"},
				"names": map[string]string{
					"self@example.com":  "Ava Stone",
					"rowan@example.org": "Rowan Vale",
				},
				"messages": []commitmentMessageEvidence{{
					MessageRef: "gmail_thread/invented-fulfilled-quote#reply",
					Sender:     "self@example.com", To: []string{"rowan@example.org"},
					At:        "2026-08-01T10:00:00Z",
					BlockRefs: []string{"authored-delivery", "quoted-request"},
				}},
			},
		}
	}

	got := classifyCommitments(memory("I attached the signed access sheet in this reply."), cfg)
	if len(got) != 1 {
		t.Fatalf("fulfilled quoted request = %+v, want one closed obligation", got)
	}
	commitment := got[0]
	wantID := commitmentID("gmail_thread/invented-fulfilled-quote#reply", "quoted-request", 0)
	if commitment.ID != wantID || commitment.State != commitClosed ||
		commitment.ClosureRef != "gmail_thread/invented-fulfilled-quote" ||
		commitment.Due != (commitDue{Kind: commitDueRelative}) ||
		len(commitment.Citations) != 2 ||
		commitment.Citations[0].Role != commitCitationOpener ||
		commitment.Citations[1].Role != commitCitationClosure {
		t.Fatalf("fulfilled quoted request = %+v", commitment)
	}

	for _, nearMiss := range []string{
		"I will attach the signed access sheet tomorrow.",
		"I attached the lighting budget in this reply.",
	} {
		if got := classifyCommitments(memory(nearMiss), cfg); len(got) != 0 {
			t.Fatalf("quote without matching authored fulfillment materialized: %q -> %+v", nearMiss, got)
		}
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
