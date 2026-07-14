package exam

// Draw is the generator's ONLY source of variation, and it is a plain function on
// purpose: it lets both property tests share one generator without rapid ever
// entering a non-test file. The AST determinism guard bans a PRNG on any scoring
// path, and a generator that smuggled one in through the back door would make that
// ban a fiction.
type Draw func(label string, min, max int) int

// GenerateLedger builds ledgers that satisfy the twelve rules BY CONSTRUCTION,
// varying everything the rules leave free: how many people there are, how long the
// reply chain is, which obligations surface, what class the negative control
// carries. A generator that could not produce a VALID ledger would exercise the
// validator's reject path forever and its accept path never.
func GenerateLedger(draw Draw) Ledger {
	people := draw("people", 2, 4)
	chain := draw("chain", 2, 4)
	classes := []string{"footer", "marketing", "notification", "trivia", "bystander"}
	class := classes[draw("class", 0, len(classes)-1)]

	l := Ledger{
		Version: 1,
		AsOf:    "2026-07-14T12:00:00Z",
		Self:    Identity{ID: "p/self", Display: "Alex Morgan", Emails: []string{"alex@example.com"}, Handles: []string{"+15550100101"}},
	}
	for i := 0; i < people; i++ {
		digit := string(rune('2' + i))
		l.People = append(l.People, Identity{
			ID:      "p/other" + digit,
			Display: "Person " + digit,
			Emails:  []string{"other" + digit + "@example.org"},
			Handles: []string{"+1555010010" + digit},
		})
	}
	other := l.People[0].ID

	event := Artifact{
		ID: "a/cal", MemoryID: "calendar_event/prop", Channel: "calendar", Subject: "Prop review",
		OccurredAt: "2026-07-14T11:00:00Z",
		Messages: []Message{{ID: "m1", From: other, To: []string{l.Self.ID, other}, At: "2026-07-14T11:00:00Z",
			Body: []Block{{ID: "b1", Kind: "notification", Text: "Review the plan."}}}},
	}

	thread := Artifact{ID: "a/thread", MemoryID: "gmail_thread/prop", Channel: "gmail", Subject: "Prop thread"}
	for i := 0; i < chain; i++ {
		at := "2026-07-1" + string(rune('0'+i)) + "T09:00:00Z"
		from, to := other, l.Self.ID
		if i%2 == 1 {
			from, to = l.Self.ID, other
		}
		kind := "authored"
		if i == chain-1 {
			kind = "quoted_reply"
		}
		thread.Messages = append(thread.Messages, Message{
			ID: "m" + string(rune('1'+i)), From: from, To: []string{to}, At: at,
			Body: []Block{{ID: "b1", Kind: kind, Text: "Message body " + string(rune('1'+i)) + " of the thread."}},
		})
	}
	thread.OccurredAt = thread.Messages[len(thread.Messages)-1].At

	forwarded := Artifact{ID: "a/fwd", MemoryID: "gmail_thread/prop-fwd", Channel: "gmail", Subject: "Fwd: offer",
		OccurredAt: "2026-07-06T11:00:00Z",
		Messages: []Message{{ID: "m1", From: other, To: []string{l.Self.ID}, At: "2026-07-06T11:00:00Z",
			Body: []Block{{ID: "b1", Kind: "forwarded", Text: "-----Original Message-----\nA generic offer."}}}}}

	footer := Artifact{ID: "a/foot", MemoryID: "gmail_thread/prop-foot", Channel: "gmail", Subject: "Notice",
		OccurredAt: "2026-07-06T12:00:00Z",
		Messages: []Message{{ID: "m1", From: other, To: []string{l.Self.ID}, At: "2026-07-06T12:00:00Z",
			Body: []Block{{ID: "b1", Kind: "footer", Text: "Boilerplate notice text."}}}}}

	chat := Artifact{ID: "a/chat", MemoryID: "imessage_chat/prop", Channel: "imessage", Subject: "Person 2",
		OccurredAt: "2026-07-05T10:05:00Z", Participants: []string{other},
		Messages: []Message{
			{ID: "m1", From: other, At: "2026-07-05T10:00:00Z", Body: []Block{{ID: "b1", Kind: "authored", Text: "I will send the report."}}},
			{ID: "m2", From: l.Self.ID, At: "2026-07-05T10:05:00Z", Body: []Block{{ID: "b1", Kind: "authored", Text: "Thanks, noted."}}},
		}}

	closer := Artifact{ID: "a/note", MemoryID: "prop-note", Channel: "notes", Subject: "Closure note",
		OccurredAt: "2026-07-07T10:00:00Z",
		Messages: []Message{{ID: "m1", From: l.Self.ID, At: "2026-07-07T10:00:00Z",
			Body: []Block{{ID: "b1", Kind: "authored", Text: "The report was delivered."}}}}}

	negative := Artifact{ID: "a/neg", MemoryID: "gmail_thread/prop-neg", Channel: "gmail", Subject: "Noise",
		OccurredAt: "2026-07-04T10:00:00Z",
		Messages: []Message{{ID: "m1", From: other, To: []string{l.Self.ID}, At: "2026-07-04T10:00:00Z",
			Body: []Block{{ID: "b1", Kind: "authored", Text: "Nothing is being asked of you here."}}}}}

	l.Artifacts = []Artifact{event, thread, forwarded, footer, chat, closer, negative}

	// Class balance is a RATIO rule, so the generator satisfies it BY CONSTRUCTION —
	// four open obligations, two of them the user's, two of them surfaced. What the
	// generator varies is WHICH two surface, which is the part the rule leaves free.
	// (A generator that could not produce a valid ledger would test the validator's
	// error path over and over and never its accept path.)
	surfaceOffset := draw("surface_offset", 0, 3)
	open := []struct {
		id       string
		self     bool
		artifact string
		message  string
		quote    string
	}{
		{"c/open1", true, "a/thread", "m1", "Message body 1 of the thread."},
		{"c/open2", false, "a/thread", "m2", "Message body 2 of the thread."},
		{"c/open3", true, "a/fwd", "m1", "A generic offer."},
		{"c/open4", false, "a/foot", "m1", "Boilerplate notice text."},
	}
	for i, c := range open {
		owner, counterparty, direction := l.Self.ID, other, DirectionOwedBySelf
		if !c.self {
			owner, counterparty, direction = other, l.Self.ID, DirectionOwedByCounterparty
		}
		var expected []string
		// Exactly half surface, and the half rotates — so "surfaced" is never
		// perfectly correlated with "the user owes it", which is the skew the
		// class-balance rule exists to prevent.
		if (i-surfaceOffset+len(open))%len(open) < len(open)/2 {
			expected = []string{"meeting:" + event.MemoryID}
		}
		l.Commitments = append(l.Commitments, Commitment{
			ID: c.id, Owner: owner, Counterparty: counterparty, Direction: direction,
			Summary:  "A generated obligation",
			OpenedBy: Span{ArtifactID: c.artifact, MessageID: c.message, BlockID: "b1", Quote: c.quote},
			DueKind:  "none", State: "open", ExpectedIn: expected,
		})
	}
	// One CLOSED obligation whose evidence lives in another channel — the ledger is
	// invalid without a cross-channel closure, and that is deliberate.
	l.Commitments = append(l.Commitments, Commitment{
		ID: "c/closed", Owner: other, Counterparty: l.Self.ID, Direction: DirectionOwedByCounterparty,
		Summary:  "Send the report",
		OpenedBy: Span{ArtifactID: "a/chat", MessageID: "m1", BlockID: "b1", Quote: "I will send the report."},
		DueKind:  "none", State: "closed",
		Transitions: []Transition{{To: "closed", At: "2026-07-07T10:00:00Z",
			Evidence: Span{ArtifactID: "a/note", MessageID: "m1", BlockID: "b1", Quote: "The report was delivered."}}},
	})
	l.NonObligations = []NonObligation{{
		ID: "n/neg", Class: class, Why: "Generated negative control.",
		Span: Span{ArtifactID: "a/neg", MessageID: "m1", BlockID: "b1", Quote: "Nothing is being asked of you here."},
	}}
	return l
}
