package mora

import (
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/companion"
)

// now for every fixture: the real Screening notice landed 2026-09-16/17 with a
// "Complete before September 23, 2026" directive, so 2026-09-18 is the reference
// instant that makes that deadline five days out.
var serviceLaneNow = time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)

func serviceMail(id, title, body string, labels []string, headers []string) Memory {
	meta := map[string]any{
		"from":        []string{"support@screening.example.com"},
		"last_sender": "support@screening.example.com",
		"to":          []string{"user@example.com"},
		"labels":      labels,
		"occurred_at": "2026-09-17T13:51:27Z",
		"messages": []map[string]any{{
			"message_ref": id + "#1",
			"sender":      "support@screening.example.com",
			"to":          []string{"user@example.com"},
			"at":          "2026-09-17T13:51:27Z",
		}},
	}
	if len(headers) > 0 {
		meta["messages"].([]map[string]any)[0]["automation_headers"] = headers
	}
	return Memory{
		ID:        id,
		Type:      "email",
		Provider:  "gmail",
		Title:     title,
		Text:      "From: Example Screening Service <support@screening.example.com>\n\n" + body,
		CreatedAt: "2026-09-17T13:51:27Z",
		Meta:      meta,
	}
}

func senderMail(id, sender, title, body string, headers []string) Memory {
	m := serviceMail(id, title, body, []string{"UNREAD", "IMPORTANT"}, headers)
	m.Meta["from"] = []string{sender}
	m.Meta["last_sender"] = sender
	m.Meta["messages"].([]map[string]any)[0]["sender"] = sender
	m.Text = "From: " + sender + "\n\n" + body
	return m
}

const screeningBody = "background check by Screening\n\n" +
	"6 days left\n" +
	"Sam, we need more information to complete your background check\n" +
	"There was an issue verifying some of your information. Go to your candidate portal to see what’s needed.\n" +
	"Complete before September 23, 2026\n"

// TestScreeningBackgroundCheckEntersActionableServiceLane is the grounding case: the
// real artifact must clear the gate on its text alone, with no read-state help.
func TestScreeningBackgroundCheckEntersActionableServiceLane(t *testing.T) {
	m := serviceMail("gmail_thread/screening-notice", "Update: more information needed", screeningBody, []string{"UNREAD"}, nil)
	if !isActionableServiceMail(m, serviceLaneNow) {
		t.Fatal("real Screening deadline notice did not enter the actionable-service lane")
	}
	// Read state is NOT an admission gate: opening it on the phone must not evict it.
	read := serviceMail("gmail_thread/read", "Update: more information needed", screeningBody, nil, nil)
	if !isActionableServiceMail(read, serviceLaneNow) {
		t.Fatal("clearing UNREAD in Gmail evicted an unresolved portal task")
	}
}

// TestActionableServiceLaneRejectsFloodFalsePositives locks the five leak vectors
// the adversarial review named against a naive paused/more-info + UNREAD gate.
func TestActionableServiceLaneRejectsFloodFalsePositives(t *testing.T) {
	tests := []struct {
		name   string
		sender string
		title  string
		body   string
	}{
		{
			name:   "instacart cart recovery",
			sender: "no-reply@instacart.com",
			title:  "Complete your order before 9:00 PM for free delivery",
			body:   "Complete your order before 9:00 PM tonight for free delivery on your cart.",
		},
		{
			name:   "linkedin application paused",
			sender: "messages-noreply@linkedin.com",
			title:  "Your application to Acme Corp is paused — we need more information",
			body:   "Your application is paused. We need more information before it can continue.",
		},
		{
			name:   "dtc cart abandonment with a real date",
			sender: "no-reply@store.com",
			title:  "6 days left: Complete your purchase before Sep 24 to claim your reward",
			body:   "6 days left. Complete your purchase before Sep 24, 2026 to claim your reward.",
		},
		{
			name:   "github account verification",
			sender: "notifications@github.com",
			title:  "Action paused: We need more information to verify your account concurrency limit",
			body:   "Action paused. We need more information to verify your account concurrency limit.",
		},
		{
			name:   "spotify dunning",
			sender: "billing@spotify.com",
			title:  "Your subscription is paused — update your payment method before Sep 22",
			body:   "Your subscription is paused. Update your payment method before September 22, 2026.",
		},
	}
	for _, tt := range tests {
		subRun(t, tt.name, func(t *testing.T) {
			m := senderMail("gmail_thread/"+tt.name, tt.sender, tt.title, tt.body, nil)
			if isActionableServiceMail(m, serviceLaneNow) {
				t.Fatalf("%s leaked onto the actionable-service lane", tt.name)
			}
		})
	}
}

// TestActionableServiceLaneRejectsBulkAndUndatedMail covers the remaining shape
// suppressions: bulk headers, bulk body footers, and transactional mail with no
// deadline at all.
func TestActionableServiceLaneRejectsBulkAndUndatedMail(t *testing.T) {
	bulkHeader := serviceMail("gmail_thread/bulk-header", "Complete before September 23, 2026", screeningBody, []string{"UNREAD"}, []string{"list-unsubscribe"})
	if isActionableServiceMail(bulkHeader, serviceLaneNow) {
		t.Fatal("List-Unsubscribe mail entered the lane")
	}
	precedence := serviceMail("gmail_thread/bulk-precedence", "Complete before September 23, 2026", screeningBody, []string{"UNREAD"}, []string{"precedence: bulk"})
	if isActionableServiceMail(precedence, serviceLaneNow) {
		t.Fatal("Precedence: bulk mail entered the lane")
	}
	bulkBody := serviceMail("gmail_thread/bulk-body", "Update: more information needed", screeningBody+"\nView in browser. Manage your preferences at any time.", []string{"UNREAD"}, nil)
	if isActionableServiceMail(bulkBody, serviceLaneNow) {
		t.Fatal("headerless mail with a campaign footer entered the lane")
	}
	// A bare CAN-SPAM unsubscribe link is NOT a campaign footer: transactional mail
	// carries one too, and treating it as bulk dropped a real Screening invitation.
	canSpam := serviceMail("gmail_thread/can-spam", "Update: more information needed", screeningBody+"\nUnsubscribe", []string{"UNREAD"}, nil)
	if !isActionableServiceMail(canSpam, serviceLaneNow) {
		t.Fatal("a bare unsubscribe link evicted a real directive")
	}
	receipt := serviceMail("gmail_thread/receipt", "Your background check is in progress", "Your background check is in progress. No action is needed right now.", []string{"UNREAD"}, nil)
	if isActionableServiceMail(receipt, serviceLaneNow) {
		t.Fatal("a status receipt with no deadline entered the lane")
	}
}

// TestActionableServiceLaneRequiresAnAbsoluteDateInWindow locks the date anchor:
// a relative countdown is not a deadline, a stale deadline is not actionable, and
// a deadline past the commitment window is not today's problem.
func TestActionableServiceLaneRequiresAnAbsoluteDateInWindow(t *testing.T) {
	countdown := serviceMail("gmail_thread/countdown", "6 days left", "6 days left to complete your background check. Go to your candidate portal.", []string{"UNREAD"}, nil)
	if isActionableServiceMail(countdown, serviceLaneNow) {
		t.Fatal("a relative countdown passed as an absolute deadline")
	}
	stale := serviceMail("gmail_thread/stale", "Update", "Complete before September 3, 2026. Go to your candidate portal.", []string{"UNREAD"}, nil)
	if isActionableServiceMail(stale, serviceLaneNow) {
		t.Fatal("an expired deadline stayed actionable")
	}
	distant := serviceMail("gmail_thread/distant", "Update", "Complete before December 23, 2026. Go to your candidate portal.", []string{"UNREAD"}, nil)
	if isActionableServiceMail(distant, serviceLaneNow) {
		t.Fatal("a deadline past the seven-day window entered the lane")
	}
}

// TestActionableServiceLaneRequiresAnImperativeNearTheDeadline bans a bare status
// word and a date that merely appears somewhere in the body.
func TestActionableServiceLaneRequiresAnImperativeNearTheDeadline(t *testing.T) {
	passive := serviceMail("gmail_thread/passive", "Scheduled maintenance", "Service is paused. Maintenance runs through September 23, 2026.", []string{"UNREAD"}, nil)
	if isActionableServiceMail(passive, serviceLaneNow) {
		t.Fatal("a passive status report with a date entered the lane")
	}
	// "Now, your background check should finish by Sep 24" is a real subject line
	// from the same Screening family: verb and deadline both present, nothing asked of
	// the owner. It is a forecast about the service, not a directive.
	forecast := serviceMail("gmail_thread/forecast", "Now, your background check should finish by Sep 24", "Now, your background check should finish by September 24, 2026.", []string{"UNREAD"}, nil)
	if isActionableServiceMail(forecast, serviceLaneNow) {
		t.Fatal("a service-side forecast entered the lane")
	}
	detached := serviceMail("gmail_thread/detached", "Report", "Your report was generated on September 23, 2026 and is ready to view in the portal, but this sentence has no directive anywhere near it at all whatsoever.", []string{"UNREAD"}, nil)
	if isActionableServiceMail(detached, serviceLaneNow) {
		t.Fatal("a date with no imperative before it entered the lane")
	}
}

// TestActionableServiceLaneIsServiceOnly keeps the lane off human mail: a human
// sender is already served by urgency and commitments, so it must not double-route.
func TestActionableServiceLaneIsServiceOnly(t *testing.T) {
	human := senderMail("gmail_thread/human", "recruiter@employer.example.com", "Background check", "Please complete the background check before September 23, 2026.", nil)
	if isActionableServiceMail(human, serviceLaneNow) {
		t.Fatal("a human-sent deadline was routed through the service lane")
	}
	note := Memory{ID: "note", Type: "note", Title: "Complete before September 23, 2026", Text: "Complete before September 23, 2026"}
	if isActionableServiceMail(note, serviceLaneNow) {
		t.Fatal("a non-connector note entered the lane")
	}
}

// TestActionableServiceMailSurvivesTheCommitmentGate is the wiring assertion: the
// commitment inventory stays empty and the artifact still reaches assembly.
func TestActionableServiceMailSurvivesTheCommitmentGate(t *testing.T) {
	screening := serviceMail("gmail_thread/screening-notice", "Update: more information needed", screeningBody, []string{"UNREAD"}, nil)
	promo := senderMail("gmail_thread/promo", "no-reply@grocer.com", "Ends tonight", "Action required: complete your order before September 20, 2026.", nil)
	instances := map[string][]Memory{"gmail": {{ID: "open"}, screening, promo}}
	inventory := map[string][]Commitment{"open": {{State: commitOpen}}}

	got := filterDigestInstancesByCommitments(instances, inventory, serviceLaneNow)
	if len(got["gmail"]) != 2 {
		t.Fatalf("surviving artifacts = %+v, want the open commitment plus the actionable service notice", got)
	}
	if got["gmail"][0].ID != "open" || got["gmail"][1].ID != screening.ID {
		t.Fatalf("survivor order = %+v, want input order preserved", got["gmail"])
	}
	if len(instances["gmail"]) != 3 {
		t.Fatal("filter mutated its input")
	}
}

// TestActionableServiceMailIsNotLowSignal keeps the lane out of the "+N more" tail
// it would otherwise fold into as service-only mail.
func TestActionableServiceMailIsNotLowSignal(t *testing.T) {
	screening := serviceMail("gmail_thread/screening-notice", "Update: more information needed", screeningBody, []string{"UNREAD"}, nil)
	if isLowSignalItem(screening, "new", serviceLaneNow) {
		t.Fatal("actionable service mail collapsed into the low-signal tail")
	}
	receipt := serviceMail("gmail_thread/receipt", "Your receipt", "Thanks for your payment.", []string{"UNREAD"}, nil)
	if !isLowSignalItem(receipt, "new", serviceLaneNow) {
		t.Fatal("ordinary service mail stopped collapsing")
	}
}

// TestActionableServiceLaneReachesTodayAsNeedsAttention locks the companion half
// of the wiring: the lane promotes ahead of the generic changed pass, lands as
// needs_attention rather than a commitment, and survives the 48h changed-mail
// cutoff that would otherwise delete a three-day-old portal deadline.
func TestActionableServiceLaneReachesTodayAsNeedsAttention(t *testing.T) {
	stale := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	d := Digest{
		Sections: []DigestSection{{
			Source: "gmail",
			Items: []DigestItem{{
				ID:        "gmail_thread/screening-notice",
				Title:     "Update: more information needed",
				Source:    "gmail",
				CreatedAt: stale,
				Snippet:   "Complete before September 23, 2026",
				Lane:      laneActionableService,
			}},
		}},
	}
	got := companionTodayCandidates(d)
	if len(got) != 1 {
		t.Fatalf("today candidates = %d, want the lane item", len(got))
	}
	if got[0].Kind != companion.ItemNeedsAttention {
		t.Fatalf("kind = %q, want needs_attention", got[0].Kind)
	}
	if got[0].CommitmentID != "" {
		t.Fatalf("lane item minted a commitment id %q", got[0].CommitmentID)
	}
}

// TestUnlanedStaleChangedMailStaysSuppressed is the control: the same stale item
// without the lane stamp is still dropped, so the exemption is lane-scoped.
func TestUnlanedStaleChangedMailStaysSuppressed(t *testing.T) {
	stale := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	d := Digest{
		Sections: []DigestSection{{
			Source: "gmail",
			Items:  []DigestItem{{ID: "gmail_thread/old", Title: "Weekly summary", Source: "gmail", CreatedAt: stale}},
		}},
	}
	if got := companionTodayCandidates(d); len(got) != 0 {
		t.Fatalf("today candidates = %+v, want stale changed-mail suppressed", got)
	}
}

// TestActionableServiceLaneRejectsImpossibleCalendarDates locks the round-trip
// check: time.Date normalizes nonsense into a real day, which would otherwise let
// an arbitrary digit run manufacture a deadline inside the window.
func TestActionableServiceLaneRejectsImpossibleCalendarDates(t *testing.T) {
	for _, body := range []string{
		"Please complete before February 31, 2026.",
		"Please complete before 2026-13-01.",
		"Please complete before 09/00/2026.",
		"Please complete before 13/32/2026.",
	} {
		m := serviceMail("gmail_thread/bad-date", "Action", body, []string{"UNREAD"}, nil)
		if isActionableServiceMail(m, time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)) {
			t.Fatalf("normalized impossible date admitted: %q", body)
		}
	}
}

// TestDeadlineWindowCoversTheLocalDeadlineDay: deadline DATES parse as UTC
// midnight while the owner lives in a local day, so the floor carries one day of
// grace. Without it a "before September 23" notice vanishes at 17:00 PDT on the
// 22nd — exactly when it matters most.
func TestDeadlineWindowCoversTheLocalDeadlineDay(t *testing.T) {
	due := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	lateOnDeadlineDay := time.Date(2026, 9, 24, 0, 30, 0, 0, time.UTC) // 17:30 PDT on the 23rd
	if !deadlineInWindow(due, lateOnDeadlineDay) {
		t.Fatal("deadline expired before the owner's local day ended")
	}
	if deadlineInWindow(due, time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("deadline outlived its grace day")
	}
}

// TestActionableServiceHoldIDsCollectsOnlyLaneMembers is the watermark guard: a
// standing portal task must stay un-baselined so it re-reads as new next run,
// while ordinary mail commits normally.
func TestActionableServiceHoldIDsCollectsOnlyLaneMembers(t *testing.T) {
	screening := serviceMail("gmail_thread/screening", "Update: more information needed", screeningBody, []string{"UNREAD"}, nil)
	receipt := serviceMail("gmail_thread/receipt", "Your receipt", "Thanks, no action needed.", []string{"UNREAD"}, nil)
	held := actionableServiceHoldIDs(map[string][]Memory{"gmail": {screening, receipt}}, serviceLaneNow)
	if !held[screening.ID] {
		t.Fatal("the standing portal task was not held out of the watermark")
	}
	if held[receipt.ID] || len(held) != 1 {
		t.Fatalf("held set = %+v, want only the lane member", held)
	}
}

// seedActionableService writes a service-authored dated directive straight into
// the vault: digestSeed's fixtures are human-sent and would mint a commitment,
// which is exactly the path this lane exists to bypass.
func seedActionableService(t *testing.T, cfg Config, id, title string, at time.Time) Memory {
	t.Helper()
	m := Memory{
		ID:          id,
		Scope:       "personal",
		Type:        "email",
		Title:       title,
		Text:        "From: Example Screening Service <support@screening.example.com>\n\n" + screeningBody,
		Source:      "gmail_thread/" + id,
		Provider:    "gmail",
		ProviderID:  "gmail_thread/" + id,
		ContentHash: "h-" + id,
		CreatedAt:   at.Format(time.RFC3339),
		Meta: map[string]any{
			"from":        []string{"support@screening.example.com"},
			"last_sender": "support@screening.example.com",
			"labels":      []string{"UNREAD"},
			"occurred_at": at.Format(time.RFC3339),
		},
	}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatalf("writeMemory: %v", err)
	}
	rebuildDigestIndex(t, cfg)
	return m
}

// TestActionableServiceNoticeReSurfacesUntilItsDeadlinePasses is the end-to-end
// watermark assertion. A portal task's text never changes while it stays undone,
// so a committed watermark would show it once and bury it for the rest of its
// window. It must re-surface on every advance until the deadline expires.
func TestActionableServiceNoticeReSurfacesUntilItsDeadlinePasses(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-time.Hour))
	steadyStateSnapshots(t, cfg, now, "gmail")
	seedActionableService(t, cfg, "screening-portal", "Update: more information needed", now.Add(-2*time.Hour))

	first, _, err := advanceBrief(cfg, now, briefOpts{advance: true}, 1<<20, true)
	if err != nil {
		t.Fatalf("advanceBrief(first): %v", err)
	}
	if _, ok := gmailItemLine(t, first, "Update: more information needed"); !ok {
		t.Fatal("the dated service directive never reached the brief")
	}

	// Same artifact, same bytes, one day later: the watermark must not have eaten it.
	second, _, err := advanceBrief(cfg, now.Add(24*time.Hour), briefOpts{advance: true}, 1<<20, true)
	if err != nil {
		t.Fatalf("advanceBrief(second): %v", err)
	}
	if _, ok := gmailItemLine(t, second, "Update: more information needed"); !ok {
		t.Fatal("the standing portal task was marked seen and buried while still due")
	}

	// Past the deadline the lane stops admitting it and the commitment gate drops it.
	expired, _, err := advanceBrief(cfg, time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC), briefOpts{advance: true}, 1<<20, true)
	if err != nil {
		t.Fatalf("advanceBrief(expired): %v", err)
	}
	if _, ok := gmailItemLine(t, expired, "Update: more information needed"); ok {
		t.Fatal("an expired deadline kept re-surfacing")
	}
}
