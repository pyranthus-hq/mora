package mora

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestBuildDigestShowsHumanMailWithoutCommitment pins F14: a product-facing
// config (DataDir set) must still show a human's mail when the commitment
// extractor found nothing in it. Before the fix the inventory was an
// eligibility gate and such a brief rendered zero sections.
func TestBuildDigestShowsHumanMailWithoutCommitment(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	m := Memory{
		ID:          "id-lunch",
		Scope:       "global",
		Type:        "email",
		Title:       "Lunch yesterday",
		Text:        "From: bob@example.com\n\nThanks for lunch yesterday, the soup was great.",
		Source:      "gmail_thread/lunch",
		Provider:    "gmail",
		ProviderID:  "gmail_thread/lunch",
		ContentHash: "h-lunch",
		CreatedAt:   now.Add(-2 * time.Hour).Format(time.RFC3339),
		Meta: map[string]any{
			"from":        []string{"bob@example.com"},
			"occurred_at": now.Add(-2 * time.Hour).Format(time.RFC3339),
		},
	}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatalf("writeMemory: %v", err)
	}
	// A second mail that does open a commitment must rank above the first.
	digestSeed(t, cfg, "gmail", "Route cards", 4*time.Hour, now)

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 24, perSourceCap: 5})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	items := digestSections(d)["gmail"].Items
	if len(items) != 2 {
		t.Fatalf("gmail items = %+v, want both mails (commitment-free mail must not be dropped)", items)
	}
	if items[0].Title != "Route cards" || items[1].Title != "Lunch yesterday" {
		t.Fatalf("order = %q, %q; want the committed mail first", items[0].Title, items[1].Title)
	}
}

func f14Write(t *testing.T, cfg Config, m Memory) Memory {
	t.Helper()
	if err := writeMemory(cfg, m); err != nil {
		t.Fatalf("writeMemory(%s): %v", m.ID, err)
	}
	return m
}

// f14Mail writes one gmail memory sent by from, occurred ago before now.
func f14Mail(t *testing.T, cfg Config, now time.Time, id, title, from, body string, ago time.Duration) Memory {
	t.Helper()
	at := now.Add(-ago).Format(time.RFC3339)
	return f14Write(t, cfg, Memory{
		ID: id, Scope: "global", Type: "email", Title: title,
		Text:   "From: " + from + "\n\n" + body,
		Source: "gmail_thread/" + id, Provider: "gmail", ProviderID: "gmail_thread/" + id,
		ContentHash: "h-" + id, CreatedAt: at,
		Meta: map[string]any{"from": []string{from}, "to": []string{"me@example.com"}, "occurred_at": at},
	})
}

func f14Text(t *testing.T, cfg Config, now time.Time, id, title, body string, ago time.Duration) Memory {
	t.Helper()
	at := now.Add(-ago).Format(time.RFC3339)
	return f14Write(t, cfg, Memory{
		ID: id, Scope: "global", Type: "imessage", Title: title,
		Text:   "Digest Person: " + body,
		Source: "imessage_thread/" + id, Provider: "imessage", ProviderID: "imessage_thread/" + id,
		ContentHash: "h-" + id, CreatedAt: at,
		Meta: map[string]any{
			"participants": []map[string]string{{"handle": "+15550101999", "name": "Digest Person"}},
			"occurred_at":  at,
		},
	})
}

func f14SectionTitles(d Digest, source string) []string {
	var out []string
	for _, it := range digestSections(d)[source].Items {
		out = append(out, it.Title)
	}
	return out
}

func f14Contains(titles []string, want string) bool {
	for _, v := range titles {
		if v == want {
			return true
		}
	}
	return false
}

func TestWindowDigestHidesWhatsAppLaneNone(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour).Format(time.RFC3339)
	m := f14Write(t, cfg, Memory{
		ID: "id-wa", Scope: "global", Type: "message", Title: "Family group",
		Text:   "Someone: lunch at one?",
		Source: "whatsapp_chat/wa", Provider: "whatsapp", ProviderID: "whatsapp_chat/wa",
		ContentHash: "h-wa", CreatedAt: at,
		Meta: map[string]any{"relevance_lane": "none", "chat_kind": "group", "occurred_at": at},
	})
	rebuildDigestIndex(t, cfg)

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 24})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	for _, s := range d.Sections {
		for _, it := range s.Items {
			if it.ID == m.ID {
				t.Fatalf("lane-none WhatsApp memory rendered in window section %q", s.Source)
			}
		}
	}
	for _, it := range d.Urgent {
		if it.ID == m.ID {
			t.Fatal("lane-none WhatsApp memory reached the Urgent shelf")
		}
	}

	// Delta path: not rendered, acknowledged count-only.
	items, urgent, _, countOnly, _ := deltaSectionItems(cfg, briefDelta{ColdStart: true}, []Memory{m}, now, "whatsapp", 10, nil, nil)
	if len(items) != 0 || len(urgent) != 0 || len(countOnly) != 1 || countOnly[0] != m.ID {
		t.Fatalf("delta: items=%v urgent=%v countOnly=%v, want count-only only", items, urgent, countOnly)
	}
}

// closedMailFixture builds an urgent-looking human mail and a raw inventory in
// which its only commitment is closed.
func closedMailFixture(provider string, now time.Time) (Memory, map[string][]Commitment) {
	at := now.Add(-2 * time.Hour).Format(time.RFC3339)
	m := Memory{
		ID: "id-closed", Type: "email", Title: "Contract confirmation", Provider: provider,
		Text:      "From: bob@example.com\n\nPlease confirm by Friday, deadline.",
		CreatedAt: at,
		Meta:      map[string]any{"from": []string{"bob@example.com"}, "occurred_at": at},
	}
	if provider == "imessage" {
		m.Type = "imessage"
		m.Text = "Digest Person: Please confirm by Friday, deadline."
		m.Meta = map[string]any{
			"participants": []map[string]string{{"handle": "+15550101999", "name": "Digest Person"}},
			"occurred_at":  at,
		}
	}
	return m, map[string][]Commitment{m.ID: {{ID: "c1", State: commitClosed, OpenedBy: commitSpan{MemoryID: m.ID, OccurredAt: at}}}}
}

func TestUrgentShelfSkipsClosedCommitment(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	m, raw := closedMailFixture("gmail", now)
	byInstance := map[string][]Memory{"gmail": {m}}

	// Control: with no closed set the same mail leads the Urgent shelf.
	d, err := buildWindowDigest(cfg, now, 24, 8, byInstance, nil, "", nil)
	if err != nil || len(d.Urgent) != 1 {
		t.Fatalf("control: err=%v urgent=%+v, want the mail on the shelf", err, d.Urgent)
	}

	daily, closed := applyCommitmentRanking(raw, true, map[string]int64{}, now)
	if !closed[m.ID] {
		t.Fatalf("closed set = %v, want %s", closed, m.ID)
	}
	d, err = buildWindowDigest(cfg, now, 24, 8, byInstance, nil, "", closed)
	if err != nil {
		t.Fatalf("buildWindowDigest: %v", err)
	}
	attachDigestCommitments(&d, daily)
	if len(d.Urgent) != 0 {
		t.Fatalf("urgent = %+v, want a closed commitment kept off the shelf", d.Urgent)
	}
	items := digestSections(d)["gmail"].Items
	if len(items) != 1 || items[0].ID != m.ID {
		t.Fatalf("gmail items = %+v, want the mail as a plain section item", items)
	}
	if len(items[0].Obligations) != 0 {
		t.Fatalf("obligations = %+v, want none for a closed commitment", items[0].Obligations)
	}
}

func TestClosedCommitmentChatStillShows(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	m, raw := closedMailFixture("imessage", now)
	_, closed := applyCommitmentRanking(raw, true, map[string]int64{}, now)
	d, err := buildWindowDigest(cfg, now, 24, 8, map[string][]Memory{"imessage": {m}}, nil, "", closed)
	if err != nil {
		t.Fatalf("buildWindowDigest: %v", err)
	}
	if got := f14SectionTitles(d, "imessage"); !f14Contains(got, m.Title) {
		t.Fatalf("imessage titles = %v, want the closed-commitment chat to still render", got)
	}
	if len(d.Urgent) != 0 {
		t.Fatalf("urgent = %+v, want none", d.Urgent)
	}
}

func TestHumanMailSurvivesNewsletterFlood(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 58; i++ {
		f14Mail(t, cfg, now, fmt.Sprintf("news-%02d", i), fmt.Sprintf("Weekly digest %02d", i),
			"no-reply@news.example.com", "Top stories this week.", time.Duration(i+1)*10*time.Minute)
	}
	f14Mail(t, cfg, now, "human", "Lunch yesterday", "bob@example.com", "Thanks for lunch, the soup was great.", 20*time.Hour)
	rebuildDigestIndex(t, cfg)

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 24, perSourceCap: 5})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	got := f14SectionTitles(d, "gmail")
	if len(got) == 0 || got[0] != "Lunch yesterday" {
		t.Fatalf("gmail titles = %v, want the human mail first", got)
	}
}

func TestDeltaCalendarNewEventNotNoChanges(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	enableSources(t, cfg, "calendar")
	seedSyncStatus(t, cfg, "calendar", now.Add(-time.Hour))
	steadyStateSnapshots(t, cfg, now, "calendar")
	digestSeedHash(t, cfg, "calendar", "Design review", -3*time.Hour, now, "h-design")

	d := deltaPreview(t, cfg, now)
	sec := digestSections(d)["calendar"]
	if sec.State != stateDelta {
		t.Fatalf("calendar state = %q, want %q for a new event", sec.State, stateDelta)
	}
	if !f14Contains(f14SectionTitles(d, "calendar"), "Design review") {
		t.Fatalf("calendar items = %+v, want the new event", sec.Items)
	}
}

func TestBriefTwentyTextsFiftyNineMails(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 20; i++ {
		f14Text(t, cfg, now, fmt.Sprintf("text-%02d", i), fmt.Sprintf("Text %02d", i), "see you soon", time.Duration(i+1)*20*time.Minute)
	}
	// 50 service mails (newsletters and receipts), 8 plain human mails, 1 real commitment.
	for i := 0; i < 50; i++ {
		title := fmt.Sprintf("Newsletter %02d", i)
		if i%2 == 1 {
			title = fmt.Sprintf("Your receipt %02d", i)
		}
		f14Mail(t, cfg, now, fmt.Sprintf("svc-%02d", i), title, "no-reply@shop.example.com", "Thanks for your order.", time.Duration(i+1)*5*time.Minute)
	}
	humans := []string{"alice", "bob", "carol", "dan", "erin", "frank", "grace", "heidi"}
	var humanTitles []string
	for i, h := range humans {
		title := "Note from " + h
		humanTitles = append(humanTitles, title)
		f14Mail(t, cfg, now, "human-"+h, title, h+"@example.com", "Just checking in, no rush.", time.Duration(10+i)*time.Hour)
	}
	f14Mail(t, cfg, now, "committed", "Signed contract", "judy@example.com", "I will send the signed contract by Friday.", 22*time.Hour)
	rebuildDigestIndex(t, cfg)

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 24, perSourceCap: 10})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	var order []string
	for _, s := range d.Sections {
		order = append(order, s.Source)
	}
	if fmt.Sprint(order) != "[imessage gmail]" {
		t.Fatalf("section order = %v, want [imessage gmail]", order)
	}
	texts := f14SectionTitles(d, "imessage")
	if len(texts) != 10 || texts[0] != "Text 00" || texts[9] != "Text 09" {
		t.Fatalf("texts = %v, want the 10 most recent in order", texts)
	}

	mails := f14SectionTitles(d, "gmail")
	if len(mails) != 10 || mails[0] != "Signed contract" {
		t.Fatalf("mails = %v, want 10 items led by the committed mail", mails)
	}
	got := append([]string(nil), mails[1:9]...)
	sort.Strings(got)
	want := append([]string(nil), humanTitles...)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("mails[1:9] = %v, want all 8 humans before any low-signal item (%v)", got, want)
	}
	if mails[9] != "Newsletter 00" {
		t.Fatalf("mails[9] = %q, want the newest service mail as the only low-signal line", mails[9])
	}
	if sec := digestSections(d)["gmail"]; sec.MoreCount != 49 {
		t.Fatalf("gmail MoreCount = %d, want 49", sec.MoreCount)
	}
}

// f14Thread builds an in-memory gmail thread. rows are per-message automation
// headers; labels may be nil.
func f14Thread(now time.Time, from, title, text string, ago time.Duration, rows [][]string, labels []string) Memory {
	at := now.Add(-ago).Format(time.RFC3339)
	var msgs []map[string]any
	for i, h := range rows {
		row := map[string]any{"message_ref": fmt.Sprintf("t#%d", i), "sender": from}
		if len(h) > 0 {
			row["automation_headers"] = h
		}
		msgs = append(msgs, row)
	}
	meta := map[string]any{"from": []string{from}, "to": []string{"me@example.com"}, "occurred_at": at}
	if msgs != nil {
		meta["messages"] = msgs
	}
	if labels != nil {
		meta["labels"] = labels
	}
	return Memory{ID: "t", Type: "email", Title: title, Text: text, Provider: "gmail", CreatedAt: at, Meta: meta}
}

var f14Now = time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

func TestBulkMailIsLowSignal(t *testing.T) {
	bulk := f14Thread(f14Now, "shop@poshmark.com", "Deals", "From: shop@poshmark.com\n\nBig sale", time.Hour, [][]string{{"list-unsubscribe"}}, nil)
	if !isLowSignalItem(bulk, "", f14Now) {
		t.Fatal("bulk mail must be low signal")
	}
	human := f14Thread(f14Now, "bob@example.com", "Lunch", "From: bob@example.com\n\nhi", time.Hour, [][]string{nil}, nil)
	if isLowSignalItem(human, "", f14Now) {
		t.Fatal("human mail must not be low signal")
	}
	mixed := f14Thread(f14Now, "shop@poshmark.com", "Deals", "From: shop@poshmark.com\n\nSale\n\n---\n\nFrom: bob@example.com\n\nre", time.Hour, [][]string{{"list-unsubscribe"}, nil}, nil)
	if isBulkMail(mixed) || isLowSignalItem(mixed, "", f14Now) {
		t.Fatal("thread with a human reply is not bulk")
	}
}

func TestBulkMailNotUrgent(t *testing.T) {
	body := "From: shop@poshmark.com\n\nPlease confirm by Friday"
	bulk := f14Thread(f14Now, "shop@poshmark.com", "Sale", body, time.Hour, [][]string{{"list-unsubscribe"}}, nil)
	if ok, _ := isUrgent(bulk, f14Now); ok {
		t.Fatal("bulk deadline mail must not be urgent")
	}
	starred := f14Thread(f14Now, "shop@poshmark.com", "Sale", body, time.Hour, [][]string{{"list-unsubscribe"}}, []string{"STARRED"})
	if ok, _ := isUrgent(starred, f14Now); !ok {
		t.Fatal("starred bulk mail stays urgent")
	}
}

func TestQuotedDeadlineNotUrgent(t *testing.T) {
	text := "From: bob@example.com\n\nFYI, archive copy.\n\nOn Mon, Sam wrote:\n> Please send the worksheet by tomorrow."
	m := f14Thread(f14Now, "bob@example.com", "Archive", text, time.Hour, [][]string{nil}, nil)
	if ok, p := isUrgent(m, f14Now); ok {
		t.Fatalf("quoted deadline made mail urgent (%q)", p)
	}
	// no per-message structure: quotes are still stripped.
	delete(m.Meta, "messages")
	if ok, _ := isUrgent(m, f14Now); ok {
		t.Fatal("quoted deadline urgent without message rows")
	}
}

func TestOldRequestNewReplyNotUrgent(t *testing.T) {
	text := "From: sam@example.com\n\nPlease send the worksheet by tomorrow.\n\n---\n\nFrom: bob@example.com\n\nThanks, sounds good."
	m := f14Thread(f14Now, "bob@example.com", "Please send the worksheet by tomorrow", text, time.Hour, [][]string{nil, nil}, nil)
	if ok, p := isUrgent(m, f14Now); ok {
		t.Fatalf("old request refreshed by unrelated reply is urgent (%q)", p)
	}
}

func TestFreshDeadlineStillUrgent(t *testing.T) {
	text := "From: sam@example.com\n\nHello.\n\n---\n\nFrom: bob@example.com\n\nPlease confirm by Friday."
	m := f14Thread(f14Now, "bob@example.com", "Plan", text, time.Hour, [][]string{nil, nil}, nil)
	ok, p := isUrgent(m, f14Now)
	if !ok {
		t.Fatal("fresh deadline must stay urgent")
	}
	if s := urgentItemFor(Config{}, m, "gmail", "", p).Snippet; !strings.Contains(s, "confirm by Friday") {
		t.Fatalf("snippet %q lost the ask", s)
	}
}
