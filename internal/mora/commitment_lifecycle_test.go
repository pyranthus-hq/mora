package mora

import (
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"reflect"
	"strings"
	"testing"
	"time"
)

func lifecycleTestCommitment() Commitment {
	self := govAtom{Kind: atomAddress, Value: "self@example.com"}
	other := govAtom{Kind: atomAddress, Value: "sam@example.org"}
	return Commitment{
		ID:               commitmentID("gmail_thread/open#m1", "body", 0),
		Owner:            self,
		Counterparty:     other,
		CounterpartyKeys: []string{"name:sam rivera", "given:sam"},
		Direction:        commitOwedBySelf,
		Summary:          "Send Sam the reviewer list",
		OpenedBy: commitSpan{
			MemoryID:   "gmail_thread/open",
			MessageRef: "gmail_thread/open#m1",
			BlockRef:   "body",
			Quote:      "Can you send the reviewer list?",
			OccurredAt: "2026-07-20T10:00:00Z",
		},
		Due:        commitDue{Kind: commitDueNone},
		State:      commitOpen,
		ClosureRef: commitClosureNone,
		Citations: []CommitmentCitation{{
			Citation:     mustLifecycleCitation("gmail_thread/open", "2026-07-20T10:00:00Z"),
			CommitmentID: commitmentID("gmail_thread/open#m1", "body", 0),
			Role:         commitCitationOpener,
		}},
	}
}

func mustLifecycleCitation(memoryID, at string) BriefCitation {
	citation, err := newBriefCitation(memoryID, "gmail", memoryID, at)
	if err != nil {
		panic(err)
	}
	return citation
}

func TestCommitmentClosurePreservesOpeningCitation(t *testing.T) {
	commitment := lifecycleTestCommitment()
	got := applyCommitmentLifecycle([]Commitment{commitment}, []commitmentEvidence{{
		MemoryID:         "imessage_chat/closure",
		Text:             "I sent the reviewer list.",
		OccurredAt:       "2026-07-20T11:00:00Z",
		Party:            commitmentPartySelf,
		Authored:         true,
		Citation:         mustLifecycleCitation("imessage_chat/closure", "2026-07-20T11:00:00Z"),
		CounterpartyKeys: []string{"name:sam rivera"},
	}})
	if len(got) != 1 {
		t.Fatalf("commitments = %d, want 1", len(got))
	}
	closed := got[0]
	if closed.State != commitClosed || closed.ClosureRef != "imessage_chat/closure" {
		t.Fatalf("closure = state %q ref %q", closed.State, closed.ClosureRef)
	}
	if len(closed.Citations) != 2 {
		t.Fatalf("citations = %+v, want opener plus closure", closed.Citations)
	}
	if closed.Citations[0].Role != commitCitationOpener ||
		closed.Citations[0].Citation.MemoryID() != commitment.OpenedBy.MemoryID ||
		closed.Citations[1].Role != commitCitationClosure ||
		closed.Citations[1].Citation.MemoryID() != "imessage_chat/closure" {
		t.Fatalf("citation order/roles = %+v", closed.Citations)
	}
}

func imessageLifecycleMemory(t *testing.T, times []string) Memory {
	t.Helper()
	const id = "imessage_chat/same-thread-review"
	lines := []string{
		"Lucia: Can you send the review notes?",
		"Me: I sent the review notes.",
		"Lucia: Got the review notes, thanks.",
	}
	body := "## 2026-08-05\n" + strings.Join(lines, "\n")
	if len(times) != len(lines) {
		t.Fatalf("times=%d, want %d", len(times), len(lines))
	}
	entries := make([]map[string]any, 0, len(lines))
	cursor := 0
	for i, line := range lines {
		start := strings.Index(body[cursor:], line)
		if start < 0 {
			t.Fatalf("line %q not found", line)
		}
		start += cursor
		end := start + len(line)
		cursor = end
		fromMe := i == 1
		sender := "Lucia"
		if fromMe {
			sender = "Me"
		}
		entries = append(entries, map[string]any{
			"evidence_ref": id + "#" + []string{"ask", "delivery", "ack"}[i],
			"at":           times[i], "from_me": fromMe, "sender": sender,
			"block_start": start, "block_end": end,
		})
	}
	return Memory{
		ID: id, Type: "imessage", Provider: "imessage", Source: "same-thread-review",
		CreatedAt: times[len(times)-1], Text: body,
		Meta: map[string]any{
			"occurred_at":   times[len(times)-1],
			"message_count": "3",
			"participants": []map[string]string{{
				"handle": "+15550100104", "name": "Lucia",
			}},
			"message_evidence_schema": 1,
			"message_evidence":        entries,
		},
	}
}

func TestIMessageSameThreadCommitmentUsesStableMessageEvidence(t *testing.T) {
	m := imessageLifecycleMemory(t, []string{
		"2026-08-05T10:00:00Z", "2026-08-05T10:05:00Z", "2026-08-05T10:06:00Z",
	})
	evidence := commitmentEvidenceFromMemories([]Memory{m}, Config{})
	if len(evidence) != 3 || evidence[0].MessageRef != m.ID+"#ask" ||
		evidence[0].OccurredAt != "2026-08-05T10:00:00Z" || evidence[0].Party != commitmentPartyCounterparty ||
		evidence[1].MessageRef != m.ID+"#delivery" || evidence[1].BlockRef != "body" ||
		evidence[1].OccurredAt != "2026-08-05T10:05:00Z" || evidence[1].Party != commitmentPartySelf ||
		evidence[2].MessageRef != m.ID+"#ack" || evidence[2].OccurredAt != "2026-08-05T10:06:00Z" ||
		evidence[2].Party != commitmentPartyCounterparty {
		t.Fatalf("message-grain lifecycle evidence=%+v", evidence)
	}
	got := materializeCommitments([]Memory{m}, Config{}, time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC))
	if len(got) != 1 {
		t.Fatalf("commitments=%+v, want one", got)
	}
	commitment := got[0]
	if commitment.State != commitClosed || commitment.OpenedBy.MessageRef != m.ID+"#ask" ||
		commitment.OpenedBy.OccurredAt != "2026-08-05T10:00:00Z" ||
		commitment.ClosureRef != m.ID {
		t.Fatalf("same-thread lifecycle lost message evidence: %+v", commitment)
	}
	if commitment.OpenedBy.BlockRef != "body" || commitment.ID == "" {
		t.Fatalf("opener was not anchored to its bounded message block: %+v", commitment.OpenedBy)
	}
	if len(commitment.Citations) != 2 {
		t.Fatalf("citations=%+v, want opener and closure", commitment.Citations)
	}
	opener, closure := commitment.Citations[0], commitment.Citations[1]
	if opener.Role != commitCitationOpener || opener.EvidenceRef != m.ID+"#ask" ||
		opener.Citation.Date() != "2026-08-05T10:00:00Z" ||
		closure.Role != commitCitationClosure || closure.EvidenceRef != m.ID+"#delivery" ||
		closure.Citation.Date() != "2026-08-05T10:05:00Z" {
		t.Fatalf("message-grain citations=%+v", commitment.Citations)
	}
}

func TestIMessageSameThreadAcknowledgementClosesWhenDeliveryIsAbsent(t *testing.T) {
	m := imessageLifecycleMemory(t, []string{
		"2026-08-05T10:00:00Z", "2026-08-05T10:05:00Z", "2026-08-05T10:06:00Z",
	})
	entries := m.Meta["message_evidence"].([]map[string]any)
	delivery := entries[1]
	start, end := delivery["block_start"].(int), delivery["block_end"].(int)
	m.Text = m.Text[:start] + strings.Repeat(" ", end-start) + m.Text[end:]
	m.Meta["message_evidence"] = []map[string]any{entries[0], entries[2]}
	m.Meta["message_count"] = "2"

	got := materializeCommitments([]Memory{m}, Config{}, time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC))
	if len(got) != 1 || got[0].State != commitClosed || got[0].ClosureRef != m.ID ||
		len(got[0].Citations) != 2 || got[0].Citations[1].EvidenceRef != m.ID+"#ack" {
		t.Fatalf("acknowledgement did not close from its own message evidence: %+v", got)
	}
}

func TestIMessageMalformedMessageEvidenceFailsClosed(t *testing.T) {
	baseTimes := []string{
		"2026-08-05T10:00:00Z", "2026-08-05T10:05:00Z", "2026-08-05T10:06:00Z",
	}
	for _, test := range []struct {
		name   string
		mutate func([]map[string]any)
	}{
		{name: "invalid time", mutate: func(entries []map[string]any) { entries[1]["at"] = "not-a-time" }},
		{name: "duplicate ref", mutate: func(entries []map[string]any) { entries[1]["evidence_ref"] = entries[0]["evidence_ref"] }},
		{name: "direction disagrees with body", mutate: func(entries []map[string]any) { entries[1]["from_me"] = false; entries[1]["sender"] = "Lucia" }},
		{name: "sender disagrees with direction", mutate: func(entries []map[string]any) { entries[1]["sender"] = "Lucia" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := imessageLifecycleMemory(t, baseTimes)
			test.mutate(m.Meta["message_evidence"].([]map[string]any))
			if got := classifyCommitments(m, Config{}); len(got) != 0 {
				t.Fatalf("malformed metadata opened commitments: %+v", got)
			}
			if got := commitmentEvidenceFromMemories([]Memory{m}, Config{}); len(got) != 0 {
				t.Fatalf("malformed metadata produced lifecycle evidence: %+v", got)
			}
		})
	}
	t.Run("schema without entries", func(t *testing.T) {
		m := imessageLifecycleMemory(t, baseTimes)
		delete(m.Meta, "message_evidence")
		if got := classifyCommitments(m, Config{}); len(got) != 0 {
			t.Fatalf("missing message evidence opened commitments: %+v", got)
		}
		if got := commitmentEvidenceFromMemories([]Memory{m}, Config{}); len(got) != 0 {
			t.Fatalf("missing message evidence produced lifecycle evidence: %+v", got)
		}
	})
	t.Run("rendered message count mismatch", func(t *testing.T) {
		m := imessageLifecycleMemory(t, baseTimes)
		entries := m.Meta["message_evidence"].([]map[string]any)
		m.Meta["message_evidence"] = entries[1:]
		if got := classifyCommitments(m, Config{}); len(got) != 0 {
			t.Fatalf("incomplete rendered-message coverage opened commitments: %+v", got)
		}
		if got := commitmentEvidenceFromMemories([]Memory{m}, Config{}); len(got) != 0 {
			t.Fatalf("incomplete rendered-message coverage produced lifecycle evidence: %+v", got)
		}
	})
}

func TestIMessagePartialMessageEvidenceFailsClosed(t *testing.T) {
	const id = "imessage_chat/partial-thread"
	lines := []string{
		"Lucia: Can you send the review notes?",
		"Lucia: Can you send the budget?",
		"Me: I sent it.",
	}
	body := "## 2026-08-05\n" + strings.Join(lines, "\n")
	entry := func(line, ref, at, sender string, fromMe bool) map[string]any {
		start := strings.Index(body, line)
		return map[string]any{
			"evidence_ref": id + "#" + ref, "at": at, "from_me": fromMe, "sender": sender,
			"block_start": start, "block_end": start + len(line),
		}
	}
	m := Memory{
		ID: id, Type: "imessage", Provider: "imessage", Source: "partial-thread",
		CreatedAt: "2026-08-05T10:02:00Z", Text: body,
		Meta: map[string]any{
			"occurred_at": "2026-08-05T10:02:00Z", "message_count": "3",
			"participants":            []map[string]string{{"handle": "+15550100104", "name": "Lucia"}},
			"message_evidence_schema": 1,
			// The first request has no provider GUID. Letting the remaining
			// messages through would make the vague delivery close the budget.
			"message_evidence": []map[string]any{
				entry(lines[1], "budget", "2026-08-05T10:01:00Z", "Lucia", false),
				entry(lines[2], "delivery", "2026-08-05T10:02:00Z", "Me", true),
			},
			"message_evidence_diagnostics": []map[string]any{{
				"reason": "missing_provider_guid", "at": "2026-08-05T10:00:00Z",
			}},
		},
	}
	if got := classifyCommitments(m, Config{}); len(got) != 0 {
		t.Fatalf("partial evidence opened only the wrong remaining commitment: %+v", got)
	}
	if got := commitmentEvidenceFromMemories([]Memory{m}, Config{}); len(got) != 0 {
		t.Fatalf("partial evidence produced a vague closure route: %+v", got)
	}
}

func TestIMessageOutOfOrderMessageTimesFailClosed(t *testing.T) {
	m := imessageLifecycleMemory(t, []string{
		"2026-08-05T10:00:00Z", "2026-08-05T09:59:00Z", "2026-08-05T10:06:00Z",
	})
	if got := classifyCommitments(m, Config{}); len(got) != 0 {
		t.Fatalf("backward message time opened commitments: %+v", got)
	}
	if got := commitmentEvidenceFromMemories([]Memory{m}, Config{}); len(got) != 0 {
		t.Fatalf("backward message time produced lifecycle evidence: %+v", got)
	}
}

func TestIMessageEqualSecondMessagesRemainValidButTiesDoNotClose(t *testing.T) {
	m := imessageLifecycleMemory(t, []string{
		"2026-08-05T10:00:00Z", "2026-08-05T10:00:00Z", "2026-08-05T10:06:00Z",
	})
	evidence := commitmentEvidenceFromMemories([]Memory{m}, Config{})
	if len(evidence) != 3 {
		t.Fatalf("equal-second metadata was rejected: %+v", evidence)
	}
	got := materializeCommitments([]Memory{m}, Config{}, time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC))
	if len(got) != 1 || got[0].State != commitClosed || got[0].ClosureRef != m.ID ||
		len(got[0].Citations) != 2 || got[0].Citations[1].EvidenceRef != m.ID+"#ack" {
		t.Fatalf("tied delivery closed the opener instead of later acknowledgement: %+v", got)
	}
}

func TestIMessageLegacyCommitmentBehaviorIsPreserved(t *testing.T) {
	m := imessageLifecycleMemory(t, []string{
		"2026-08-05T10:00:00Z", "2026-08-05T10:05:00Z", "2026-08-05T10:06:00Z",
	})
	delete(m.Meta, "message_evidence")
	delete(m.Meta, "message_evidence_schema")
	got := materializeCommitments([]Memory{m}, Config{}, time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC))
	if len(got) != 1 || got[0].State != commitOpen || got[0].ID != "" ||
		got[0].OpenedBy.MessageRef != "" || got[0].OpenedBy.OccurredAt != m.Meta["occurred_at"] {
		t.Fatalf("legacy transcript behavior changed: %+v", got)
	}
}

func TestCommitmentSnapshotCrossSurfaceDeterminism(t *testing.T) {
	cfg, event, at := seedExamHomeFromRoot(t, examFixtureV2Root)
	meeting, err := readCommitmentSnapshot(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	daily, err := readCommitmentSnapshot(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if meeting.Generation == "" || meeting.Generation != daily.Generation ||
		!reflect.DeepEqual(meeting.Commitments, daily.Commitments) {
		t.Fatalf("same index generation diverged by surface:\nmeeting=%+v\ndaily=%+v", meeting, daily)
	}
	brief, err := buildEventMeetingBrief(t.Context(), cfg, event.EventID, at, 0, meetingBriefDefaultPerGuest)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range brief.Sections {
		for _, line := range section.Lines {
			if line.CommitmentID != "" && (line.Lifecycle == "" || line.ClosureRef == "") {
				t.Fatalf("typed line omitted shared lifecycle state: %+v", line)
			}
		}
	}
}

func TestDigestUsesMaterializedCommitmentGeneration(t *testing.T) {
	cfg, _, at := seedExamHomeFromRoot(t, examFixtureV2Root)
	snapshot, err := readCommitmentSnapshot(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := buildDigest(cfg, at, briefOpts{
		sinceHours: examDailyWindowHours, perSourceCap: examDailyPerSourceCap,
	})
	if err != nil {
		t.Fatal(err)
	}

	byMemory := map[string][]Commitment{}
	for _, commitment := range snapshot.Commitments {
		byMemory[commitment.OpenedBy.MemoryID] = append(byMemory[commitment.OpenedBy.MemoryID], commitment)
	}
	typed := 0
	for _, item := range digestAllItems(digest) {
		commitments := byMemory[item.ID]
		if len(commitments) != 1 {
			if item.Direction != "" {
				t.Fatalf("digest item %s guessed among %d materialized commitments: %+v", item.ID, len(commitments), item)
			}
			continue
		}
		typed++
		commitment := commitments[0]
		if !atomEqual(item.Owner, commitment.Owner) ||
			item.Direction != commitment.Direction ||
			item.DueAt != commitDueValue(commitment.Due) ||
			item.Lifecycle != commitment.State ||
			item.ClosureRef != commitment.ClosureRef {
			t.Fatalf("digest item %s diverged from generation %s:\nitem=%+v\ncommitment=%+v",
				item.ID, snapshot.Generation, item, commitment)
		}
	}
	if typed == 0 {
		t.Fatal("exam digest exposed no materialized commitment")
	}
}

func TestMaterializedCommitmentLinksCrossSourceClosure(t *testing.T) {
	cfg, _, _ := seedExamHomeFromRoot(t, examFixtureV2Root)
	snapshot, err := readCommitmentSnapshot(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var receipt *Commitment
	for i := range snapshot.Commitments {
		if snapshot.Commitments[i].OpenedBy.MemoryID == "gmail_thread/v2-receipt-thread" {
			receipt = &snapshot.Commitments[i]
			break
		}
	}
	if receipt == nil {
		t.Fatal("paper receipt commitment was not materialized")
	}
	if receipt.State != commitClosed || receipt.ClosureRef != "imessage_chat/v2-receipt-ack" {
		t.Fatalf("cross-source lifecycle = %+v", *receipt)
	}
	if len(receipt.Citations) < 2 ||
		receipt.Citations[0].Role != commitCitationOpener ||
		receipt.Citations[len(receipt.Citations)-1].Role != commitCitationClosure {
		t.Fatalf("cross-source closure did not preserve opener and add closure: %+v", receipt.Citations)
	}
}

func TestStaleSourceMarksCommitmentStateUncertain(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "self@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: "2026-07-01T00:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-25*time.Hour))
	memory := Memory{
		ID: "gmail_thread/open", Type: "email", Provider: "gmail",
		ProviderID: "open", Source: "open", CreatedAt: "2026-07-20T10:00:00Z",
		Title: "Reviewer list",
		Text:  "From: Sam <sam@example.org>\n\nCan you send the reviewer list?",
		Meta: map[string]any{
			"from": []string{"sam@example.org"}, "to": []string{"self@example.com"},
			"occurred_at": "2026-07-20T10:00:00Z",
		},
	}
	got := materializeCommitments([]Memory{memory}, cfg, now)
	if len(got) != 1 || !got[0].StateUncertain || got[0].State != commitOpen {
		t.Fatalf("stale source silently asserted state: %+v", got)
	}
}
