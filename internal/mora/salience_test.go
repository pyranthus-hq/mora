package mora

import (
	"reflect"
	"strconv"
	"testing"
)

// ---- Task 1: sat / channelScale / recencyDecay / salienceMicros ----

// ---- Task 2: scoreSalience / aggregatePersonSalience / metaMessageCount ----

// imsgMemory builds a one-conversation iMessage memory between the given handle and
// a fixed "me" handle, carrying a message_count string as the real connectors emit it.
func imsgMemory(id, handle, name, occurred string, msgCount int) Memory {
	return Memory{
		ID:        id,
		Type:      "imessage",
		CreatedAt: occurred,
		Meta: map[string]any{
			"occurred_at":   occurred,
			"message_count": strconv.Itoa(msgCount),
			"participants": []any{
				map[string]any{"handle": handle, "name": name},
				map[string]any{"handle": "+10000000000", "name": "Me"},
			},
		},
	}
}

// emailMemory builds a single email thread from->to with a message_count string.
func emailMemory(id, from, to, occurred string, msgCount int) Memory {
	return Memory{
		ID:        id,
		Type:      "email",
		CreatedAt: occurred,
		Meta: map[string]any{
			"occurred_at":   occurred,
			"message_count": strconv.Itoa(msgCount),
			"from":          []any{from},
			"to":            []any{to},
		},
	}
}

// eventMemory builds a calendar event with an organizer + one attendee.
func eventMemory(id, organizer, attendee, occurred string) Memory {
	return Memory{
		ID:        id,
		Type:      "event",
		CreatedAt: occurred,
		Meta: map[string]any{
			"occurred_at": occurred,
			"organizer":   organizer,
			"attendees":   []any{attendee},
		},
	}
}

// TestSalienceAggregateSaturationCapsRunaway is the core property this phase relies on:
// per-channel saturation BOUNDS a high-volume single channel so it cannot run away. A
// 50,000-message texter must NOT outscore a 5,000-message texter (both saturate the
// imsg channel to ~1.0) — proving the volume signal is capped, not linear. (Pre-
// saturation, naive message-count would make 50k score 10× the 5k contact.)
func TestSalienceAggregateSaturationCapsRunaway(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z" // hold recency equal
	mega := "+15550001111"
	heavy := "+15550002222"
	mems := []Memory{
		imsgMemory("im_mega", mega, "Mega Texter", occ, 50000),
		imsgMemory("im_heavy", heavy, "Heavy Texter", occ, 5000),
	}
	scores := aggregatePersonSalience(mems)
	megaScore := scores[personID(mega)]
	heavyScore := scores[personID(heavy)]
	if megaScore == 0 || heavyScore == 0 {
		t.Fatalf("unexpected zero: mega=%d heavy=%d", megaScore, heavyScore)
	}
	// 10× the messages must NOT yield a meaningfully higher score (both saturate to ~1).
	if megaScore != heavyScore {
		t.Fatalf("saturation did not cap runaway volume: mega(50k)=%d != heavy(5k)=%d", megaScore, heavyScore)
	}
}

// TestSalienceAggregateMultiChannelWins is the inversion fix's positive case: once
// per-channel volumes saturate, a MULTI-CHANNEL human outranks a single-channel one of
// equal saturated volume, because Breadth (0.05) + the clamped multi-channel Volume
// break the tie in the human's favor. This is what stops a one-channel noise source
// from sitting above a real, multi-surface relationship.
func TestSalienceAggregateMultiChannelWins(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z" // hold recency equal
	single := "+15550001111"
	multi := "multi@example.com"
	multiH := "+15559998888"
	mems := []Memory{
		// Single-channel contact, channel fully saturated.
		imsgMemory("im_single", single, "Single Chan", occ, 5000),
		// Multi-channel human: each channel ALSO saturated (high per-channel counts),
		// spanning email + calendar + iMessage.
		emailMemory("em_multi", multi, "me@example.com", occ, 500),
		eventMemory("ev_multi", multi, "me@example.com", occ),
		imsgMemory("im_multi", multiH, "Multi Phone", occ, 5000),
	}
	scores := aggregatePersonSalience(mems)
	singleScore := scores[personID(single)]
	multiScore := scores[personID(multi)] // email+event identity (multi-channel)
	if singleScore == 0 || multiScore == 0 {
		t.Fatalf("unexpected zero: single=%d multi=%d", singleScore, multiScore)
	}
	if multiScore <= singleScore {
		t.Fatalf("multi-channel did not win: multi=%d should beat single=%d (Breadth tiebreak)",
			multiScore, singleScore)
	}
}

// TestSalienceAggregateMessageCountVsFallback proves Volume reads the real count when
// present and degrades to the 1-per-memory fallback when absent — a 100-message thread
// outranks a count-less thread for the same channel/recency.
func TestSalienceAggregateMessageCountVsFallback(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z"
	withCount := emailMemory("em_big", "big@example.com", "me@example.com", occ, 100)
	// A memory with NO message_count -> fallback 1.
	noCount := Memory{
		ID: "em_small", Type: "email", CreatedAt: occ,
		Meta: map[string]any{
			"occurred_at": occ,
			"from":        []any{"small@example.com"},
			"to":          []any{"me@example.com"},
		},
	}
	scores := aggregatePersonSalience([]Memory{withCount, noCount})
	big := scores[personID("big@example.com")]
	small := scores[personID("small@example.com")]
	if big <= small {
		t.Fatalf("message_count not consumed: big(count=100)=%d should beat small(fallback=1)=%d", big, small)
	}
}

func TestSalienceAggregateTombstoneSkipped(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z"
	live := emailMemory("em_live", "a@example.com", "me@example.com", occ, 5)
	dead := emailMemory("em_dead", "b@example.com", "me@example.com", occ, 5)
	dead.DeletedAt = "2026-06-02T00:00:00Z"
	scores := aggregatePersonSalience([]Memory{live, dead})
	if _, ok := scores[personID("a@example.com")]; !ok {
		t.Fatalf("live memory's person missing from scores")
	}
	if _, ok := scores[personID("b@example.com")]; ok {
		t.Fatalf("tombstoned memory's person should be skipped (live-stats rule)")
	}
}

func TestSalienceAggregateDeterminism(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z"
	mems := []Memory{
		imsgMemory("im1", "+15550001111", "A", occ, 30),
		emailMemory("em1", "b@example.com", "me@example.com", occ, 9),
		eventMemory("ev1", "c@example.com", "me@example.com", occ),
	}
	a := aggregatePersonSalience(mems)
	b := aggregatePersonSalience(mems)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("aggregatePersonSalience non-deterministic:\n a=%v\n b=%v", a, b)
	}
}

// TestSalienceAggregateGarbageMeta feeds tampered/garbage Meta and asserts a finite,
// defined micros (no NaN/Inf/panic) — the T-14-01 mitigation.
func TestSalienceAggregateGarbageMeta(t *testing.T) {
	garbage := Memory{
		ID: "im_garbage", Type: "imessage", CreatedAt: "not-a-time",
		Meta: map[string]any{
			"occurred_at":   "also-not-a-time",
			"message_count": "🙃 not-a-number",
			"participants": []any{
				map[string]any{"handle": "+15551234567", "name": "Weird, Name"},
			},
		},
	}
	scores := aggregatePersonSalience([]Memory{garbage})
	got := scores[personID("+15551234567")]
	if got < 0 || got > 1_000_000 {
		t.Fatalf("garbage Meta produced out-of-range micros: %d", got)
	}
}

func TestSalienceAggregateRecencyVaultRelative(t *testing.T) {
	// Two people, both on the same single channel/volume; the one seen at vaultMax
	// must outrank the older one purely on vault-relative recency (no wall clock).
	recent := emailMemory("em_recent", "recent@example.com", "me@example.com", "2026-06-01T00:00:00Z", 5)
	old := emailMemory("em_old", "old@example.com", "me@example.com", "2024-06-01T00:00:00Z", 5)
	scores := aggregatePersonSalience([]Memory{recent, old})
	r := scores[personID("recent@example.com")]
	o := scores[personID("old@example.com")]
	if r <= o {
		t.Fatalf("vault-relative recency not applied: recent=%d should beat old=%d", r, o)
	}
}
