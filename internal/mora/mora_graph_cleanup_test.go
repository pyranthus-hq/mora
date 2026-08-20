package mora

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// writeEmail is a tiny helper for the cleanup tests: one email memory from
// `from` (with optional display name) to adit.
func writeEmail(t *testing.T, cfg Config, id, from, name string) {
	t.Helper()
	meta := map[string]any{
		"from": []string{from},
		"to":   []string{"adit@x.com"},
	}
	if name != "" {
		meta["names"] = map[string]string{from: name}
	}
	if err := writeMemory(cfg, Memory{
		ID: id, Scope: "personal", Type: "email", Title: id,
		CreatedAt: "2026-05-01T00:00:00Z", Text: "body", Meta: meta,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestA1AutomatedSendersClassifiedService proves the A1 contract end-to-end: after
// an index rebuild on a vault mixing bots and humans, the People list (graphList-
// Entities kind=="person" / printEntities) is human-dominated and the bots are
// stored as kind "service" — present for search, absent from People.
func TestA1AutomatedSendersClassifiedService(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	bots := []struct{ addr, name string }{
		{"jobalerts-noreply@linkedin.com", "LinkedIn Job Alerts"},
		{"receipts@uber.com", "Uber Receipts"},
		{"calendar-notification@google.com", "Google Calendar"},
		{"no-reply@venmo.com", "Venmo"},
	}
	humans := []struct{ addr, name string }{
		{"riya@gmail.com", "Riya Sharma"},
		{"sam@owner.dev", "Sam Owner"},
		{"neil@example.com", "Neil Patel"},
	}
	// Give every sender two corroborating memories so names are trusted aliases and
	// classification has a stable display to read.
	for i, b := range bots {
		writeEmail(t, cfg, fmt.Sprintf("gmail_thread/bot%d-a", i), b.addr, b.name)
		writeEmail(t, cfg, fmt.Sprintf("gmail_thread/bot%d-b", i), b.addr, b.name)
	}
	for i, h := range humans {
		writeEmail(t, cfg, fmt.Sprintf("gmail_thread/hum%d-a", i), h.addr, h.name)
		writeEmail(t, cfg, fmt.Sprintf("gmail_thread/hum%d-b", i), h.addr, h.name)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	// Stored kind: bots are service, humans are person.
	ents := readEntities(t, cfg)
	for _, b := range bots {
		if got := ents[personID(b.addr)].kind; got != "service" {
			t.Errorf("bot %s stored kind = %q, want service", b.addr, got)
		}
	}
	for _, h := range humans {
		if got := ents[personID(h.addr)].kind; got != "person" {
			t.Errorf("human %s stored kind = %q, want person", h.addr, got)
		}
	}

	// Read path: graphListEntities surfaces the stored kind (NOT the "person:" id
	// prefix) so People excludes services.
	list, err := graphListEntities(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list {
		if e.Kind == "person" {
			for _, b := range bots {
				if strings.EqualFold(e.Name, b.name) {
					t.Errorf("bot %q leaked into People (kind=person)", b.name)
				}
			}
		}
	}
	// Services must still be present in the list (searchable), just not as people.
	svc := 0
	for _, e := range list {
		if e.Kind == "service" {
			svc++
		}
	}
	if svc != len(bots) {
		t.Errorf("service entity count = %d, want %d (kept for search)", svc, len(bots))
	}

	// CLI surface: printEntities People section has the humans, none of the bots.
	var buf bytes.Buffer
	printEntities(&buf, list)
	out := buf.String()
	people := peopleSection(out)
	for _, h := range humans {
		if !strings.Contains(people, h.name) {
			t.Errorf("human %q missing from printed People section", h.name)
		}
	}
	for _, b := range bots {
		if strings.Contains(people, b.name) {
			t.Errorf("bot %q present in printed People section:\n%s", b.name, people)
		}
	}
}

// peopleSection returns the text under the "People" header up to the next blank-
// line-delimited section header in printEntities output.
func peopleSection(out string) string {
	lines := strings.Split(out, "\n")
	var b strings.Builder
	in := false
	for _, ln := range lines {
		switch {
		case strings.TrimSpace(ln) == "People":
			in = true
		case in && (ln == "Scopes" || ln == "Links" || ln == "Categories" || ln == "Tags"):
			return b.String()
		case in:
			b.WriteString(ln)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// writeInbound writes an email FROM `sender` (named `senderName`) TO adit, where
// the sender's address book labels the recipient adit as `ownerLabel` — the
// recipient-side label that A2 must treat as untrusted (spam mail-merge, others
// mislabeling you).
func writeInbound(t *testing.T, cfg Config, id, sender, senderName, ownerLabel string) {
	t.Helper()
	names := map[string]string{}
	if senderName != "" {
		names[sender] = senderName
	}
	if ownerLabel != "" {
		names["alex.owner@gmail.com"] = ownerLabel
	}
	meta := map[string]any{
		"from": []string{sender},
		"to":   []string{"alex.owner@gmail.com"},
	}
	if len(names) > 0 {
		meta["names"] = names
	}
	if err := writeMemory(cfg, Memory{
		ID: id, Scope: "personal", Type: "email", Title: id,
		CreatedAt: "2026-05-01T00:00:00Z", Text: "body", Meta: meta,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestA2ProvenanceSenderTrustedRecipientNot proves the core A2 rule: a name a
// person presents for THEMSELVES as a sender is a trusted alias (even once), while
// a name an inbound sender attaches to the RECIPIENT never becomes an alias.
func TestA2ProvenanceSenderTrustedRecipientNot(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// chris sends, presenting himself as "Chris Real" (sender provenance -> trusted).
	writeEmail(t, cfg, "gmail_thread/c1", "chris@example.com", "Chris Real")
	// A spammer sends TO chris, labeling the recipient chris as "Wrongname"
	// (recipient provenance -> untrusted). Use a non-bot sender so chris is a plain
	// recipient.
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/c2", Scope: "personal", Type: "email", Title: "c2",
		CreatedAt: "2026-05-02T00:00:00Z", Text: "x",
		Meta: map[string]any{
			"from":  []string{"listings@jobfarm.net"},
			"to":    []string{"chris@example.com"},
			"names": map[string]string{"chris@example.com": "Wrongname"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	al := readAliases(t, cfg, "person:chris@example.com")
	if !contains(al, "Chris Real") {
		t.Errorf("aliases = %v, want self-presented sender name 'Chris Real'", al)
	}
	if contains(al, "Wrongname") {
		t.Errorf("aliases = %v, must NOT contain recipient-side label 'Wrongname'", al)
	}
	if !contains(al, "chris@example.com") {
		t.Errorf("aliases = %v, want the address itself", al)
	}
}

// TestA2IMessageContactNameTrusted proves iMessage participant names (from the
// user's own address book) are trusted aliases — this is how a friend's nickname
// ("Anna") legitimately resolves, in contrast to a Gmail spam label.
func TestA2IMessageContactNameTrusted(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	if err := writeMemory(cfg, Memory{
		ID: "imessage_chat/c1", Scope: "personal", Type: "imessage", Title: "chat",
		CreatedAt: "2026-05-01T00:00:00Z", Text: "x",
		Meta: map[string]any{
			"participants": []map[string]string{{"handle": "+15551234567", "name": "Anish Anna"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	al := readAliases(t, cfg, "person:+15551234567")
	if !contains(al, "Anish Anna") {
		t.Errorf("aliases = %v, want trusted iMessage contact name", al)
	}
	res, err := graphGetEntity(ctx, cfg, "Anish Anna")
	if err != nil {
		t.Fatal(err)
	}
	if found, _ := res["found"].(bool); !found {
		t.Errorf(`get_entity("Anish Anna") should resolve to the iMessage contact`)
	}
}

// TestA2CalendarOrganizerTrustedAttendeeNot proves the event-side provenance rule:
// the organizer (who owns/created the event) is self-presenting, so their name is a
// trusted alias and gazetteer-matchable; an attendee's name (applied BY the
// organizer/calendar) is recipient-side and stays untrusted.
func TestA2CalendarOrganizerTrustedAttendeeNot(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	if err := writeMemory(cfg, Memory{
		ID: "calendar_event/e1", Scope: "personal", Type: "event", Title: "Standup",
		CreatedAt: "2026-05-01T09:00:00Z", Text: "Big Boss ran the standup.",
		Meta: map[string]any{
			"organizer": "boss@corp.com",
			"attendees": []string{"adit@x.com"},
			"names":     map[string]string{"boss@corp.com": "Big Boss", "adit@x.com": "Adit Attendee"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// A note body mentions the organizer's name — should body-match via the gazetteer.
	if err := writeMemory(cfg, Memory{
		ID: "note/n1", Scope: "personal", Type: "note", Title: "recap",
		CreatedAt: "2026-05-02T00:00:00Z", Text: "Big Boss approved the plan.",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	if al := readAliases(t, cfg, "person:boss@corp.com"); !contains(al, "Big Boss") {
		t.Errorf("organizer aliases = %v, want trusted self-presented 'Big Boss'", al)
	}
	if al := readAliases(t, cfg, "person:adit@x.com"); contains(al, "Adit Attendee") {
		t.Errorf("attendee aliases = %v, must NOT trust organizer-applied label", al)
	}
	// Gazetteer body-match: "Big Boss" in note/n1 -> MENTIONS edge on the organizer.
	if edges := readEdges(t, cfg); !hasEdge(edges, "memory:note/n1|MENTIONS|person:boss@corp.com|note/n1") {
		t.Error("expected a MENTIONS edge from the note body to the organizer (trusted alias)")
	}
}

// TestA1RecipientLabelDoesNotMisclassify proves a real person who only ever appears
// as a RECIPIENT, labeled with a service-suffix name by an inbound sender, is NOT
// flipped to "service" — A1 classifies on the trusted name (here none) / address.
func TestA1RecipientLabelDoesNotMisclassify(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// A non-bot sender emails chris, labeling the recipient chris "Chris Receipts".
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/x1", Scope: "personal", Type: "email", Title: "x1",
		CreatedAt: "2026-05-01T00:00:00Z", Text: "hi",
		Meta: map[string]any{
			"from":  []string{"realfriend@gmail.com"},
			"to":    []string{"chris@gmail.com"},
			"names": map[string]string{"chris@gmail.com": "Chris Receipts"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := readEntities(t, cfg)["person:chris@gmail.com"].kind; got != "person" {
		t.Errorf("chris kind = %q, want person (untrusted recipient-label must not classify)", got)
	}
}

// TestA2FanoutPreservesSender proves the fan-out cap retains the sender even when
// more than maxParticipantFanout recipients sort before it — the sender keeps its
// edge and its trusted alias.

// hasEdgeSlice reports whether any edge with the given src/rel/dst exists.

// TestA1GetEntityServiceConsistency proves get_entity reports kind="service" for a
// service entity (consistent with list_entities), not the legacy "person" prefix.
func TestA1GetEntityServiceConsistency(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	writeEmail(t, cfg, "gmail_thread/b1", "receipts@uber.com", "Uber Receipts")
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	res, err := graphGetEntity(ctx, cfg, "Uber Receipts")
	if err != nil {
		t.Fatal(err)
	}
	if found, _ := res["found"].(bool); !found {
		t.Fatal("service entity should still be searchable via get_entity")
	}
	if k, _ := res["kind"].(string); k != "service" {
		t.Errorf(`get_entity kind = %q, want "service" (consistent with list_entities)`, k)
	}
}

// TestA2AliasBleedFixed reproduces the real-data defect: spam senders labeled the
// recipient adit as "Anna" (mail-merge), and that foreign name bled into adit's
// identity. After A2 provenance, a recipient-side label is neither an alias nor
// resolvable via get_entity — even repeated across memories.
func TestA2AliasBleedFixed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// adit presents himself as "Alex Owner" when he sends (trusted).
	writeEmail(t, cfg, "gmail_thread/me1", "alex.owner@gmail.com", "Alex Owner")
	// Three spam senders label the recipient adit "Anna" (recipient-side, untrusted)
	// — repetition must NOT make it trusted.
	writeInbound(t, cfg, "gmail_thread/s1", "listings@jobfarm1.net", "Your Area", "Anna")
	writeInbound(t, cfg, "gmail_thread/s2", "listings@jobfarm2.net", "Your Area", "Anna")
	writeInbound(t, cfg, "gmail_thread/s3", "listings@jobfarm3.net", "Your Area", "Anna")
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	al := readAliases(t, cfg, "person:alex.owner@gmail.com")
	if contains(al, "Anna") {
		t.Errorf("alias bleed: %v still contains recipient-side spam label 'Anna'", al)
	}
	if !contains(al, "Alex Owner") {
		t.Errorf("aliases = %v, want self-presented 'Alex Owner'", al)
	}

	// get_entity("Anna") must not resolve to Adit.
	res, err := graphGetEntity(ctx, cfg, "Anna")
	if err != nil {
		t.Fatal(err)
	}
	if found, _ := res["found"].(bool); found {
		t.Errorf(`get_entity("Anna") resolved to %v, want found:false`, res["display_name"])
	}
}
