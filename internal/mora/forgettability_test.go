package mora

import (
	"math"
	"slices"
	"testing"
	"time"
)

func TestForgettabilityGoldenRegimes(t *testing.T) {
	now := mustForgetTime(t, "2026-01-01T12:00:00Z")
	eventTitle := "Coffee kindergarten portfolio lease"
	attendees := []string{"Dana", "Sam", "Leo", "Pat", "Riley", "Austin", "Taylor", "Casey"}
	candidates := []forgettabilityCandidate{
		{
			StableID: "kid-thread", Title: "Mira kindergarten", Text: "Dana mentioned that her daughter Mira started kindergarten and loves the new class.",
			OccurredAt: daysAgo(now, 240), PersonID: "person:dana@example.com", PersonDisplay: "Dana", PersonKind: "person", PersonLastSeen: daysAgo(now, 45),
			AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, MessageCount: 1, MentionCount: 4,
		},
		{
			StableID: "daily-latest", Title: "1:1", Text: "see you in a few minutes",
			OccurredAt: now.Format(time.RFC3339), PersonID: "person:sam@example.com", PersonDisplay: "Sam", PersonKind: "person", PersonLastSeen: now.Format(time.RFC3339),
			AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, MessageCount: 800, MentionCount: 20,
		},
		{
			StableID: "moderate-dormant", Title: "Portfolio notes", Text: "Pat sent thoughtful notes on the portfolio positioning before going quiet.",
			OccurredAt: daysAgo(now, 120), PersonID: "person:pat@example.com", PersonDisplay: "Pat", PersonKind: "person", PersonLastSeen: daysAgo(now, 120),
			AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, MessageCount: 4, MentionCount: 3,
		},
		{
			StableID: "revived-thread", Title: "Old partnership thread", Text: "Riley revived the old thread with a current reply, so the thread is no longer forgotten.",
			CreatedAt: daysAgo(now, 360), OccurredAt: now.Format(time.RFC3339), PersonID: "person:riley@example.com", PersonDisplay: "Riley", PersonKind: "person", PersonLastSeen: now.Format(time.RFC3339),
			AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, MessageCount: 3, MentionCount: 3,
		},
		{
			StableID: "denver-old", Title: "Denver apartment lease", Text: "Casey said the Denver apartment lease paperwork was nearly done.",
			OccurredAt: daysAgo(now, 300), PersonID: "person:casey@example.com", PersonDisplay: "Casey", PersonKind: "person", PersonLastSeen: daysAgo(now, 20),
			AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, MessageCount: 3, MentionCount: 5,
		},
		{
			StableID: "austin-new", Title: "Austin apartment lease", Text: "Casey said the Austin apartment lease paperwork was done.",
			OccurredAt: daysAgo(now, 20), PersonID: "person:casey@example.com", PersonDisplay: "Casey", PersonKind: "person", PersonLastSeen: daysAgo(now, 20),
			AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, MessageCount: 5, MentionCount: 5,
		},
		{
			StableID: "undated-relevant", Title: "Portfolio", Text: "Taylor had a portfolio idea with no parseable timestamp.",
			CreatedAt: "not-a-date", PersonID: "person:taylor@example.com", PersonDisplay: "Taylor", PersonKind: "person", PersonLastSeen: daysAgo(now, 90),
			AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, MessageCount: 1, MentionCount: 3,
		},
		{
			StableID: "service-sender", Title: "Portfolio alert", Text: "Automated alert about portfolio changes.",
			OccurredAt: daysAgo(now, 300), PersonID: "person:alerts@example.com", PersonDisplay: "Alerts", PersonKind: "service", PersonLastSeen: daysAgo(now, 300),
			AttendeeKnown: true, IdentityCorroborated: true, MessageCount: 1, MentionCount: 10,
		},
		{
			StableID: "bulk-human", Title: "Coffee newsletter", Text: "A human-looking address sent a bulk newsletter.",
			OccurredAt: daysAgo(now, 300), PersonID: "person:newsletter@example.com", PersonDisplay: "Newsletter", PersonKind: "person", PersonLastSeen: daysAgo(now, 300),
			AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, BulkAuthored: true, MessageCount: 1, MentionCount: 10,
		},
		{
			StableID: "unknown-identity", Title: "Coffee kindergarten portfolio", Text: "This would otherwise score highly, but the attendee identity was not resolved.",
			OccurredAt: daysAgo(now, 300), PersonID: "person:unknown@example.com", PersonDisplay: "Unknown", PersonKind: "person", PersonLastSeen: daysAgo(now, 300),
			HumanAuthored: true, MessageCount: 1, MentionCount: 1,
		},
		{
			StableID: "self-memory", Title: "Coffee kindergarten portfolio", Text: "The user's own old note must not be scored as attendee context.",
			OccurredAt: daysAgo(now, 300), PersonID: "person:me@example.com", PersonDisplay: "Me", PersonKind: "person", PersonLastSeen: daysAgo(now, 300),
			AttendeeKnown: true, IdentityCorroborated: true, Self: true, HumanAuthored: true, MessageCount: 1, MentionCount: 10,
		},
		{
			StableID: "deleted-memory", Title: "Coffee kindergarten portfolio", Text: "A deleted memory must not survive the validity gate.",
			OccurredAt: daysAgo(now, 300), DeletedAt: daysAgo(now, 1), PersonID: "person:deleted@example.com", PersonDisplay: "Deleted", PersonKind: "person", PersonLastSeen: daysAgo(now, 300),
			AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, MessageCount: 1, MentionCount: 10,
		},
		{
			StableID: "cold-start", Title: "Coffee intro", Text: "Austin sent one useful coffee intro thread months ago.",
			OccurredAt: daysAgo(now, 120), PersonID: "person:austin@example.com", PersonDisplay: "Austin", PersonKind: "person", PersonLastSeen: daysAgo(now, 120),
			AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, MessageCount: 1, MentionCount: 1,
		},
		{
			StableID: "buried-live-thread", Title: "Leo daily chat", Text: "A months-old intro commitment is buried inside an otherwise live daily chat.",
			OccurredAt: now.Format(time.RFC3339), PersonID: "person:leo@example.com", PersonDisplay: "Leo", PersonKind: "person", PersonLastSeen: now.Format(time.RFC3339),
			AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, MessageCount: 800, MentionCount: 50,
		},
	}

	got := rankForgettability(now, eventTitle, attendees, candidates, forgettabilityOptions{})
	want := expectedForgettabilityMicros(t, now, eventTitle, attendees, candidates)
	for _, r := range got.All {
		if r.ValueMicros != want[r.StableID] {
			t.Fatalf("%s value_micros = %d, want %d", r.StableID, r.ValueMicros, want[r.StableID])
		}
	}

	selectedIDs := resultIDs(got.Selected)
	if !slices.Contains(selectedIDs, "kid-thread") {
		t.Fatalf("kid thread was not selected: %v", selectedIDs)
	}
	if slices.Contains(selectedIDs, "daily-latest") {
		t.Fatalf("daily texter latest message should not survive selected pool: %v", selectedIDs)
	}
	if slices.Contains(selectedIDs, "buried-live-thread") {
		t.Fatalf("buried fact in a live high-volume thread must stay invisible: %v", selectedIDs)
	}
	for _, id := range []string{"service-sender", "bulk-human", "unknown-identity", "self-memory", "deleted-memory"} {
		if slices.Contains(selectedIDs, id) {
			t.Fatalf("%s passed a hard gate and was selected: %v", id, selectedIDs)
		}
	}
	if got.ByID("denver-old").Freshness >= 1 {
		t.Fatalf("old Denver thread freshness = %.3f, want shadow-dampened", got.ByID("denver-old").Freshness)
	}
	if got.ByID("denver-old").ValueMicros <= got.ByID("austin-new").ValueMicros {
		t.Fatalf("forgotten old location should outrank newer replacement when dated-historical: old=%d new=%d", got.ByID("denver-old").ValueMicros, got.ByID("austin-new").ValueMicros)
	}
	if !slices.Contains(got.Gaps.ThinAttendees, "Only 1 memory about Austin - coverage is thin.") {
		t.Fatalf("thin attendee gap missing: %#v", got.Gaps.ThinAttendees)
	}
}

func TestForgettabilityHardShadowDropsNearRestatement(t *testing.T) {
	now := mustForgetTime(t, "2026-01-01T12:00:00Z")
	candidates := []forgettabilityCandidate{
		{
			StableID: "old-restatement", Title: "Denver apartment lease paperwork", Text: "Denver apartment lease paperwork done",
			OccurredAt: daysAgo(now, 300), PersonID: "person:casey@example.com", PersonDisplay: "Casey", PersonKind: "person", PersonLastSeen: daysAgo(now, 20),
			AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, MessageCount: 1, MentionCount: 3,
		},
		{
			StableID: "new-restatement", Title: "Denver apartment lease paperwork", Text: "Denver apartment lease paperwork done",
			OccurredAt: daysAgo(now, 20), PersonID: "person:casey@example.com", PersonDisplay: "Casey", PersonKind: "person", PersonLastSeen: daysAgo(now, 20),
			AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, MessageCount: 1, MentionCount: 3,
		},
	}

	got := rankForgettability(now, "Denver lease", []string{"Casey"}, candidates, forgettabilityOptions{})
	old := got.ByID("old-restatement")
	if old.Gates.Valid {
		t.Fatalf("old near-verbatim restatement remained valid: %#v", old.Gates)
	}
	if old.ValueMicros != 0 {
		t.Fatalf("old near-verbatim restatement value_micros = %d, want 0", old.ValueMicros)
	}
	if slices.Contains(resultIDs(got.Selected), "old-restatement") {
		t.Fatalf("old near-verbatim restatement was selected: %v", resultIDs(got.Selected))
	}
}

func TestForgettabilityDatedTieBreakAndPerAttendeeCap(t *testing.T) {
	now := mustForgetTime(t, "2026-01-01T12:00:00Z")
	candidates := []forgettabilityCandidate{
		forgettabilityCandidateForCap("dana-1", "person:dana@example.com", "Dana", daysAgo(now, 120), now.Format(time.RFC3339)),
		forgettabilityCandidateForCap("dana-2", "person:dana@example.com", "Dana", "", now.Format(time.RFC3339)),
		forgettabilityCandidateForCap("dana-3", "person:dana@example.com", "Dana", daysAgo(now, 121), daysAgo(now, 1)),
		forgettabilityCandidateForCap("dana-4", "person:dana@example.com", "Dana", daysAgo(now, 122), daysAgo(now, 2)),
		forgettabilityCandidateForCap("pat-1", "person:pat@example.com", "Pat", daysAgo(now, 90), daysAgo(now, 90)),
	}

	got := rankForgettability(now, "portfolio", []string{"Dana", "Pat"}, candidates, forgettabilityOptions{EvidenceCap: 4, PerAttendeeCap: 3})
	selectedIDs := resultIDs(got.Selected)
	if countPerson(got.Selected, "person:dana@example.com") != 3 {
		t.Fatalf("selected %d Dana items, want per-attendee cap 3: %v", countPerson(got.Selected, "person:dana@example.com"), selectedIDs)
	}
	if !slices.Contains(selectedIDs, "pat-1") {
		t.Fatalf("global pool should leave room for another attendee under the cap: %v", selectedIDs)
	}
	if indexOf(resultIDs(got.All), "dana-1") > indexOf(resultIDs(got.All), "dana-2") {
		t.Fatalf("dated item should sort before undated at equal value: %v", resultIDs(got.All))
	}
}

func forgettabilityCandidateForCap(id, personID, display, occurred, lastSeen string) forgettabilityCandidate {
	text := map[string]string{
		"dana-1": "orchard runway cacao lantern marble violet",
		"dana-2": "harbor cobalt prism velvet meadow copper",
		"dana-3": "summit amber linen quartz canyon silver",
		"dana-4": "forest indigo ceramic basil comet willow",
		"pat-1":  "market juniper slate apricot signal nickel",
	}[id]
	return forgettabilityCandidate{
		StableID: id, Title: "portfolio " + id, Text: text,
		OccurredAt: occurred, PersonID: personID, PersonDisplay: display, PersonKind: "person", PersonLastSeen: lastSeen,
		AttendeeKnown: true, IdentityCorroborated: true, HumanAuthored: true, MessageCount: 1, MentionCount: 5,
	}
}

func expectedForgettabilityMicros(t *testing.T, now time.Time, eventTitle string, attendeeNames []string, candidates []forgettabilityCandidate) map[string]int64 {
	t.Helper()
	opts := defaultForgettabilityOptions(forgettabilityOptions{})
	eventTokens := testDistinctiveTokens(eventTitle, attendeeNames)
	tokenSets := map[string]map[string]bool{}
	for _, c := range candidates {
		tokenSets[c.StableID] = testDistinctiveTokens(c.Title+" "+c.Text, attendeeNames)
	}
	out := map[string]int64{}
	for _, c := range candidates {
		tFact, dated := parseForgettabilityTime(c.OccurredAt, c.CreatedAt)
		ageDays := 0.0
		if dated {
			ageDays = math.Max(0, now.Sub(tFact).Hours()/24)
		}
		lastSeen, ok := parseRFC3339(c.PersonLastSeen)
		dormantDays := 0.0
		if ok {
			dormantDays = math.Max(0, now.Sub(lastSeen).Hours()/24)
		}
		mc := c.MessageCount
		if mc < 1 {
			mc = 1
		}
		corroboration := opts.HapaxCap
		if c.ContentCorroborated || (c.HumanAuthored && len(c.Text) >= opts.BodyMinChars) {
			corroboration = 1
		}
		a := 1 - math.Exp2(-ageDays/opts.RecallHalfLifeDays)
		b := 1 - math.Exp2(-dormantDays/opts.DormancyHalfLifeDays)
		rawC := 1 - math.Min(1, math.Log1p(float64(mc-1))/math.Log1p(opts.RarityScale))
		forget := clamp01(opts.WeightAge*a + opts.WeightDormancy*b + opts.WeightRarity*corroboration*rawC)
		rel := 0.0
		if len(eventTokens) > 0 {
			rel = float64(testIntersectionSize(eventTokens, tokenSets[c.StableID])) / float64(len(eventTokens))
		}
		relPrime := opts.RelFloor + (1-opts.RelFloor)*clamp01(rel)
		maxOverlap := 0.0
		for _, newer := range candidates {
			if newer.PersonID != c.PersonID || newer.StableID == c.StableID {
				continue
			}
			nt, nd := parseForgettabilityTime(newer.OccurredAt, newer.CreatedAt)
			if !dated || !nd || !nt.After(tFact) {
				continue
			}
			denom := len(tokenSets[c.StableID])
			if denom == 0 {
				continue
			}
			ov := float64(testIntersectionSize(tokenSets[c.StableID], tokenSets[newer.StableID])) / float64(denom)
			if ov > maxOverlap {
				maxOverlap = ov
			}
		}
		fresh := clamp01(1 - opts.ShadowStrength*maxOverlap)
		valid := maxOverlap < opts.ShadowHardGateOverlap
		identity := c.AttendeeKnown && (mc > 1 || c.IdentityCorroborated)
		gated := c.PersonKind == "person" && identity && !c.Self && !c.BulkAuthored && c.DeletedAt == "" && valid
		value := 0.0
		if gated {
			commit := 0.0
			if c.Commit {
				commit = 1
			}
			value = clamp01(fresh * relPrime * clamp01(forget+opts.CommitLift*commit))
		}
		out[c.StableID] = int64(math.Round(value * forgettabilityMicrosScale))
	}
	return out
}

func testDistinctiveTokens(s string, attendeeNames []string) map[string]bool {
	exclude := map[string]bool{}
	for _, name := range attendeeNames {
		for _, tok := range tokenizeWords(name) {
			exclude[tok] = true
		}
	}
	out := map[string]bool{}
	for _, tok := range tokenizeWords(s) {
		if len(tok) < 3 || ftsStopwords[tok] || exclude[tok] {
			continue
		}
		out[tok] = true
	}
	return out
}

func testIntersectionSize(a, b map[string]bool) int {
	n := 0
	for tok := range a {
		if b[tok] {
			n++
		}
	}
	return n
}

func mustForgetTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func daysAgo(now time.Time, days int) string {
	return now.Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
}

func resultIDs(results []forgettabilityResult) []string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.StableID)
	}
	return ids
}

func countPerson(results []forgettabilityResult, personID string) int {
	n := 0
	for _, r := range results {
		if r.PersonID == personID {
			n++
		}
	}
	return n
}

func indexOf(ids []string, id string) int {
	for i, got := range ids {
		if got == id {
			return i
		}
	}
	return len(ids)
}
