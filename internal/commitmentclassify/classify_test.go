package commitmentclassify

import (
	"github.com/pyranthus-hq/mora/internal/commitment"
	"github.com/pyranthus-hq/mora/internal/memory"
	"strings"
	"testing"
)

func TestManualAuthoredPromiseMaterialization(t *testing.T) {
	opts := Options{SelfEmails: map[string]bool{"self@example.com": true}}
	m := memory.Memory{
		ID:        "invented-note-promise",
		Type:      "note",
		Source:    "manual",
		CreatedAt: "2026-08-03T09:00:00Z",
		Text:      "I told Jordan I'd return the borrowed lens before the workshop.",
	}
	got := Classify(m, opts)
	if len(got) != 1 {
		t.Fatalf("commitments = %+v, want one clear authored promise", got)
	}
	record := got[0]
	if !commitment.EqualAtom(record.Owner, commitment.CanonicalSelf(opts.SelfEmails, "")) ||
		record.Direction != commitment.OwedBySelf ||
		record.Due != (commitment.Due{Kind: commitment.DueRelative}) {
		t.Fatalf("typed commitment = %+v, want self-owned relative promise", record)
	}
	if record.Counterparty.Kind != "" || record.Counterparty.Value != "" {
		t.Fatalf("counterparty = %+v, want an honest gap without source-native identity metadata", record.Counterparty)
	}
	if record.CounterpartyLabel != "Jordan" {
		t.Fatalf("counterparty label = %q, want source-authored addressee", record.CounterpartyLabel)
	}
	if record.ID != "" {
		t.Fatalf("commitment id = %q, want no fabricated immutable evidence id", record.ID)
	}

	m.ID = "invented-note-would-promise"
	m.Text = "I told Rowan Vale I would return the borrowed lens before the workshop."
	got = Classify(m, opts)
	if len(got) != 1 || got[0].CounterpartyLabel != "Rowan Vale" ||
		got[0].Counterparty.Kind != "" || got[0].Counterparty.Value != "" {
		t.Fatalf("explicit would-promise = %+v, want name-grain label without identity atom", got)
	}

	m.ID = "invented-note-past-report"
	m.Text = "I told Jordan I'd already returned the borrowed lens before the workshop."
	if got := Classify(m, opts); len(got) != 0 {
		t.Fatalf("past-completion near miss became future work: %+v", got)
	}
}

func TestGmailCommitmentUsesAuthoredPrefixBlockRef(t *testing.T) {
	opts := Options{SelfEmails: map[string]bool{"self@example.com": true}}
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
			memory := memory.Memory{
				ID: "gmail_thread/invented-structured", Type: "email", Provider: "gmail",
				Source: "invented-structured", CreatedAt: "2026-08-01T10:00:00Z",
				Text: "From: Other <other@example.com>\n\n" + tt.body,
				Meta: map[string]any{
					"from": []string{"other@example.com"},
					"to":   []string{"self@example.com"},
					"messages": []commitment.GmailMessage{{
						MessageRef: "gmail_thread/invented-structured#message-1",
						Sender:     "other@example.com",
						To:         []string{"self@example.com"},
						At:         "2026-08-01T10:00:00Z",
						BlockRefs:  tt.blockRefs,
					}},
				},
			}
			got := Classify(memory, opts)
			if len(got) != tt.want {
				t.Fatalf("commitments = %+v, want %d", got, tt.want)
			}
			if tt.want == 0 {
				return
			}
			wantID := commitment.ID("gmail_thread/invented-structured#message-1", "authored-body", 0)
			if got[0].ID != wantID || got[0].OpenedBy.BlockRef != "authored-body" {
				t.Fatalf("opening identity = %+v, want authored prefix %q", got[0].OpenedBy, wantID)
			}
		})
	}
}

func TestAcceptedRequestDoesNotCreateExtraWork(t *testing.T) {
	opts := Options{SelfEmails: map[string]bool{"self@example.com": true}}
	makeMemory := func(reply string) memory.Memory {
		return memory.Memory{
			ID: "gmail_thread/invented-acceptance", Type: "email", Provider: "gmail",
			Source: "invented-acceptance", CreatedAt: "2026-08-01T10:30:00Z",
			Text: "From: Self <self@example.com>\n\n" +
				"Could you reserve the calibration bench for the sensor run?\n\n---\n\n" + reply,
			Meta: map[string]any{
				"from": []string{"self@example.com", "other@example.com"},
				"to":   []string{"self@example.com", "other@example.com"},
				"messages": []commitment.GmailMessage{
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

	got := Classify(makeMemory(
		"I will hold the calibration bench. Please send me the sensor checklist before the review.",
	), opts)
	if len(got) != 2 {
		t.Fatalf("accepted request produced extra work: %+v", got)
	}
	wantIDs := map[string]bool{
		commitment.ID("gmail_thread/invented-acceptance#ask", "ask-body", 0):     true,
		commitment.ID("gmail_thread/invented-acceptance#reply", "reply-body", 0): true,
	}
	for _, record := range got {
		if !wantIDs[record.ID] {
			t.Fatalf("commitment id %q shows an acceptance consumed a slot: %+v", record.ID, got)
		}
	}

	nearMiss := Classify(makeMemory(
		"I will prepare the lighting budget. Please send me the sensor checklist before the review.",
	), opts)
	if len(nearMiss) != 3 {
		t.Fatalf("materially changed action collapsed into the earlier request: %+v", nearMiss)
	}
}

func TestIMessageAcceptanceDoesNotCreateExtraWork(t *testing.T) {
	opts := Options{SelfEmails: map[string]bool{"self@example.com": true}}
	makeMemory := func(reply string) memory.Memory {
		return memory.Memory{
			ID: "imessage_chat/invented-acceptance", Type: "imessage", Provider: "imessage",
			Source: "invented-acceptance", CreatedAt: "2026-08-01T10:00:00Z",
			Text: "Me: Would you send me the calibration code?\nLucia: " + reply,
			Meta: map[string]any{"participants": []map[string]string{{
				"handle": "+15550100104", "name": "Lucia Wynn",
			}}},
		}
	}

	got := Classify(makeMemory("Yes, I will text you the calibration code when I reach the desk."), opts)
	if len(got) != 1 || got[0].Direction != commitment.OwedByCounterparty ||
		got[0].Due != (commitment.Due{Kind: commitment.DueRelative}) ||
		got[0].Counterparty.Value != "+15550100104" {
		t.Fatalf("accepted iMessage request = %+v, want one due-enriched obligation", got)
	}

	if nearMiss := Classify(makeMemory("I will prepare the lighting budget tomorrow."), opts); len(nearMiss) != 2 {
		t.Fatalf("materially changed iMessage action collapsed into the request: %+v", nearMiss)
	}
}

func TestReportedThirdPartyPromiseRequiresNamedSelfBeneficiary(t *testing.T) {
	opts := Options{SelfEmails: map[string]bool{"ava@example.com": true}}
	makeMemory := func(beneficiary string) memory.Memory {
		body := "Rhea said, “I will bring " + beneficiary + " the sealed envelope before the review.”"
		return memory.Memory{
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
				"messages": []commitment.GmailMessage{{
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

	got := Classify(makeMemory("Ava"), opts)
	wantActor := commitment.Atom{Kind: commitment.AtomAddress, Value: "rhea@example.org"}
	if len(got) != 1 || !commitment.EqualAtom(got[0].Owner, wantActor) ||
		!commitment.EqualAtom(got[0].Counterparty, wantActor) ||
		got[0].Direction != commitment.OwedByCounterparty {
		t.Fatalf("safe reported promise = %+v, want Rhea owing Ava", got)
	}

	if got := Classify(makeMemory("Morgan"), opts); len(got) != 0 {
		t.Fatalf("third-party-only reported promise became the user's loop: %+v", got)
	}
}

func TestAuthoredDeliveryMaterializesFulfilledQuotedRequest(t *testing.T) {
	opts := Options{SelfEmails: map[string]bool{"self@example.com": true}}
	makeMemory := func(delivery string) memory.Memory {
		return memory.Memory{
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
				"messages": []commitment.GmailMessage{{
					MessageRef: "gmail_thread/invented-fulfilled-quote#reply",
					Sender:     "self@example.com", To: []string{"rowan@example.org"},
					At:        "2026-08-01T10:00:00Z",
					BlockRefs: []string{"authored-delivery", "quoted-request"},
				}},
			},
		}
	}

	got := Classify(makeMemory("I attached the signed access sheet in this reply."), opts)
	if len(got) != 1 {
		t.Fatalf("fulfilled quoted request = %+v, want one closed obligation", got)
	}
	record := got[0]
	wantID := commitment.ID("gmail_thread/invented-fulfilled-quote#reply", "quoted-request", 0)
	if record.ID != wantID || record.State != commitment.Closed ||
		record.ClosureRef != "gmail_thread/invented-fulfilled-quote" ||
		record.Due != (commitment.Due{Kind: commitment.DueRelative}) ||
		len(record.Citations) != 2 ||
		record.Citations[0].Role != commitment.CitationOpener ||
		record.Citations[1].Role != commitment.CitationClosure {
		t.Fatalf("fulfilled quoted request = %+v", record)
	}

	for _, nearMiss := range []string{
		"I will attach the signed access sheet tomorrow.",
		"I attached the lighting budget in this reply.",
	} {
		if got := Classify(makeMemory(nearMiss), opts); len(got) != 0 {
			t.Fatalf("quote without matching authored fulfillment materialized: %q -> %+v", nearMiss, got)
		}
	}
}

func TestClassifierEligibilityAndLegacyGmail(t *testing.T) {
	opts := Options{SelfEmails: map[string]bool{"self@example.com": true}}
	base := memory.Memory{ID: "gmail_thread/legacy", Provider: "gmail", Source: "gmail:me", CreatedAt: "2026-08-01T10:00:00Z", Title: "I will send the agenda tomorrow", Text: "From: Other <other@example.com>\n\nCould you send the review notes before kickoff?", Meta: map[string]any{"from": []string{"other@example.com"}, "to": []string{"self@example.com"}}}
	got := Classify(base, opts)
	if len(got) != 2 || got[0].Direction != commitment.OwedBySelf || got[1].Direction != commitment.OwedByCounterparty {
		t.Fatalf("legacy gmail=%+v", got)
	}
	deleted := base
	deleted.DeletedAt = "2026-08-01T11:00:00Z"
	if got := Classify(deleted, opts); got != nil {
		t.Fatalf("deleted=%+v", got)
	}
	opts.ServiceOnly = true
	if got := Classify(base, opts); got != nil {
		t.Fatalf("service=%+v", got)
	}
	if got := Classify(memory.Memory{ID: "none", Source: "manual", Text: "notes"}, Options{}); len(got) != 0 {
		t.Fatalf("no commitment=%+v", got)
	}
}

func structuredIMessage(times []string) memory.Memory {
	const id = "imessage_chat/same-thread-review"
	lines := []string{"Lucia: Can you send the review notes?", "Me: I sent the review notes.", "Lucia: Got the review notes, thanks."}
	body := "## 2026-08-05\n" + strings.Join(lines, "\n")
	entries := make([]map[string]any, 0, len(lines))
	for i, line := range lines {
		start := strings.Index(body, line)
		fromMe := i == 1
		sender := "Lucia"
		if fromMe {
			sender = "Me"
		}
		entries = append(entries, map[string]any{"evidence_ref": id + "#" + []string{"ask", "delivery", "ack"}[i], "at": times[i], "from_me": fromMe, "sender": sender, "block_start": start, "block_end": start + len(line)})
	}
	return memory.Memory{ID: id, Type: "imessage", Provider: "imessage", Source: "chat", CreatedAt: times[len(times)-1], Text: body, Meta: map[string]any{"occurred_at": times[len(times)-1], "message_count": "3", "participants": []map[string]string{{"handle": "+15550100104", "name": "Lucia"}}, "message_evidence_schema": 1, "message_evidence": entries}}
}
func TestStructuredIMessageAdmission(t *testing.T) {
	m := structuredIMessage([]string{"2026-08-05T10:00:00Z", "2026-08-05T10:05:00Z", "2026-08-05T10:06:00Z"})
	got := Classify(m, Options{})
	if len(got) != 1 || got[0].OpenedBy.MessageRef != "imessage_chat/same-thread-review#ask" || got[0].Direction != commitment.OwedBySelf {
		t.Fatalf("structured=%+v", got)
	}
	m.Meta["message_evidence"].([]map[string]any)[1]["at"] = "not-a-time"
	if got := Classify(m, Options{}); len(got) != 0 {
		t.Fatalf("malformed=%+v", got)
	}
}

func TestThirdPartyAssignmentAndUnknownStructuredSender(t *testing.T) {
	opts := Options{SelfEmails: map[string]bool{"self@example.com": true}}
	m := memory.Memory{ID: "gmail_thread/third-party", Provider: "gmail", Source: "gmail:me", CreatedAt: "2026-07-20T10:00:00Z", Title: "Next steps", Text: "From: Other <other@example.com>\n\nAction item for Kim: Please share the findings before kickoff.", Meta: map[string]any{"from": []string{"other@example.com"}, "to": []string{"self@example.com"}}}
	if got := Classify(m, opts); len(got) != 0 {
		t.Fatalf("third party=%+v", got)
	}
	m.Text = "From: Other <other@example.com>\n\nI will send the findings.\n\n---\n\nI will send the notes."
	m.Meta["messages"] = []commitment.GmailMessage{{MessageRef: "m1", Sender: "other@example.com", To: []string{"self@example.com"}, At: m.CreatedAt, BlockRefs: []string{"b1"}}, {MessageRef: "m2", Sender: "unknown@example.net", To: []string{"self@example.com"}, At: m.CreatedAt, BlockRefs: []string{"b2"}}}
	got := Classify(m, opts)
	if len(got) != 1 || got[0].OpenedBy.MessageRef != "m1" {
		t.Fatalf("unknown sender admission=%+v", got)
	}
}
