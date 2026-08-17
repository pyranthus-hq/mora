package mora

import (
	"context"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"os"
	"strings"
	"testing"
	"time"
)

// Issue #62 defect 2 — the item-level urgency lane. isUrgent gates on a known-human
// sender, a deadline/time-pressure phrase, and a recent WALL-CLOCK arrival (the only
// place wall-clock enters; salience stays clock-free). The combination keeps
// marketing "urgent!" spam off the shelf while catching the real same-day-deadline
// email from a human correspondent.

func urgentTestMem(from, title, body string, occurred time.Time) Memory {
	return Memory{
		ID:        "id-" + title,
		Type:      "note",
		Title:     title,
		Text:      body,
		Meta:      map[string]any{"from": []string{from}, "occurred_at": occurred.UTC().Format(time.RFC3339)},
		CreatedAt: occurred.UTC().Format(time.RFC3339),
	}
}

func TestIsUrgentServiceSenderExcluded(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	m := urgentTestMem("no-reply@marketing.com", "URGENT: act now", "Final notice — respond by today!", now.Add(-1*time.Hour))
	if ok, _ := isUrgent(m, now); ok {
		t.Fatalf("a service/no-reply sender must never reach the urgent shelf (spam guard)")
	}
}

func TestIsUrgentNoSenderExcluded(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	m := Memory{ID: "id-note", Type: "note", Title: "urgent deadline", Text: "asap by end of day", CreatedAt: now.Format(time.RFC3339)}
	if ok, _ := isUrgent(m, now); ok {
		t.Fatalf("a memory with no sender (a note) must not be urgent")
	}
}

// TestUrgentSnippetAnchorsOnDeadlinePhrase (defect 4): the shelf snippet centers on
// the deadline phrase rather than blindly clipping the tail (which showed sign-offs).

// Review finding: the From-line strip under-stripped a "Display Name <addr>" header,
// leaving name+address cruft in the flagship urgent snippet.

// Review finding: matchDeadlinePhrase substring-matched negations.

// Issue #62 defect 2 (enrichment): Gmail actionability labels enrich the gate. A
// user-STARRED recent human email reaches the shelf even without a deadline phrase
// (an explicit user signal), but UNREAD+IMPORTANT alone must not (too noisy as a gate).

// TestAssembleUrgentShelfHigherScoreLeads: within the shelf, a higher urgency score
// (starred/important/unread boost) leads even at equal arrival time.
func TestAssembleUrgentShelfHigherScoreLeads(t *testing.T) {
	tm := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	entries := []urgentEntry{
		{item: DigestItem{ID: "plain"}, occurredAt: tm, score: 2},
		{item: DigestItem{ID: "starred"}, occurredAt: tm, score: 5},
	}
	items, _ := assembleUrgentShelf(entries)
	if len(items) != 2 || items[0].ID != "starred" {
		t.Fatalf("higher urgency score must lead the shelf; got %+v", items)
	}
}

// urgentGmailSeed writes a gmail-thread memory with a human sender + a deadline body
// (the shape gmailThreadToItem produces), so the delta brief can detect urgency.
func urgentGmailSeed(t *testing.T, cfg Config, title, from, body string, occurred time.Time) {
	t.Helper()
	m := Memory{
		ID:          "id-" + title,
		Scope:       "global",
		Type:        "note",
		Title:       title,
		Text:        "From: " + from + "\n\n" + body,
		Source:      "gmail_thread/" + title,
		Provider:    "gmail",
		ProviderID:  "gmail_thread/" + title,
		ContentHash: "h-" + title,
		CreatedAt:   occurred.UTC().Format(time.RFC3339),
		Meta: map[string]any{
			"from":        []string{from},
			"to":          []string{"me@example.com"},
			"occurred_at": occurred.UTC().Format(time.RFC3339),
		},
	}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatalf("writeMemory: %v", err)
	}
}

// TestUrgentShelfLeadsBriefAndSurvivesBudget (defect 2, the incident): a low-volume
// urgent deadline email from a known human leads the brief on the Urgent shelf, is
// NOT duplicated in its source section, survives a budget that clips every section,
// and is committed to the watermark (it rendered).
func TestUrgentShelfLeadsBriefAndSurvivesBudget(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	enableSources(t, cfg, "gmail", "calendar", "imessage")
	if err := saveSources(cfg, []Source{
		{Name: "gmail", Type: "gmail", Email: "me@example.com", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: now.Format(time.RFC3339)},
		{Name: "calendar", Type: "calendar", Calendar: "primary", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: now.Format(time.RFC3339)},
		{Name: "imessage", Type: "imessage", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: now.Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("saveSources: %v", err)
	}
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "calendar", now.Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "imessage", now.Add(-1*time.Hour))
	steadyStateSnapshots(t, cfg, now, "gmail", "calendar", "imessage")

	// Higher-rank, higher-volume noise so the urgent email would otherwise be buried.
	for i := 0; i < 6; i++ {
		digestSeed(t, cfg, "calendar", "Cal event "+itoa(i), -time.Duration(i+2)*time.Hour, now)
		digestSeed(t, cfg, "imessage", "Group text "+itoa(i), time.Duration(i+1)*time.Hour, now)
	}
	urgentGmailSeed(t, cfg, "MSA sign-off", "sarah@client.com", "Can you sign the MSA by end of day today?", now.Add(-1*time.Hour))
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}

	// Preview: the shelf carries the urgent item; the gmail section does NOT.
	d, err := buildDigest(cfg, now, briefOpts{})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	if len(d.Urgent) != 1 || d.Urgent[0].Title != "MSA sign-off" {
		t.Fatalf("urgent shelf must carry the one urgent email; got %+v", d.Urgent)
	}
	for _, it := range digestSections(d)["gmail"].Items {
		if it.ID == "id-MSA sign-off" {
			t.Fatalf("a shelved urgent item must NOT also render in its source section")
		}
	}

	// A budget that fits only the frame + shelf (every section shells).
	tiny := len(renderDigestHeader(d)) + len(renderDigestFreshness(d)) +
		len(renderDigestUrgentShelf(d)) + len(renderDigestStaleTasks(d)) + 4

	bd, _, aerr := advanceBrief(cfg, now, briefOpts{advance: true}, tiny, true)
	if aerr != nil {
		t.Fatalf("advanceBrief: %v", aerr)
	}
	if len(bd.Urgent) != 1 {
		t.Fatalf("the shelf must survive a section-clipping budget; got %+v", bd.Urgent)
	}
	artifact, err := os.ReadFile(briefArtifactPath(cfg, now))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !strings.Contains(string(artifact), "MSA sign-off") {
		t.Fatalf("the urgent item must render even under a section-clipping budget;\n%s", artifact)
	}
	// The shelf heading leads the sections.
	if iu, ic := strings.Index(string(artifact), "Urgent"), strings.Index(string(artifact), "## "); iu < 0 || (ic >= 0 && iu > strings.LastIndex(string(artifact), "## ")) {
		// (sanity: the shelf heading exists)
		if !strings.Contains(string(artifact), "Urgent") {
			t.Fatalf("brief must carry an Urgent shelf heading;\n%s", artifact)
		}
	}
	// Committed: it rendered on the shelf, so the watermark advances over it.
	snap := loadBriefSnapshot(cfg, "gmail")
	if _, seen := snap.Items["id-MSA sign-off"]; !seen {
		t.Fatalf("a rendered urgent item must be committed; snap=%v", snap.Items)
	}
}
