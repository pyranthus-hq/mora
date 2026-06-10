package mora

import (
	"math"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// rfc3339 builds a fixed UTC RFC3339 instant for deterministic recency tests.
func rfc3339(t *testing.T, s string) string {
	t.Helper()
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Fatalf("bad fixture time %q: %v", s, err)
	}
	return s
}

// ---- Task 1: sat / channelScale / recencyDecay / salienceMicros ----

func TestSalienceSat(t *testing.T) {
	const tol = 1e-9
	// Exact boundary pins.
	if got := sat(0, 12); got != 0 {
		t.Fatalf("sat(0,12)=%v, want 0", got)
	}
	if got := sat(12, 12); math.Abs(got-1) > tol {
		t.Fatalf("sat(12,12)=%v, want 1", got)
	}
	if got := sat(250, 250); math.Abs(got-1) > tol {
		t.Fatalf("sat(250,250)=%v, want 1", got)
	}
	// Clamps to 1 for x>scale.
	if got := sat(1000, 12); got != 1 {
		t.Fatalf("sat(1000,12)=%v, want exactly 1 (clamped)", got)
	}
	// Guard: scale<=0 -> 0 (no NaN/Inf from log1p(0)=0 division).
	if got := sat(5, 0); got != 0 {
		t.Fatalf("sat(5,0)=%v, want 0 (guarded)", got)
	}
	if got := sat(5, -3); got != 0 {
		t.Fatalf("sat(5,-3)=%v, want 0 (guarded)", got)
	}
	// Mid value is strictly in (0,1) and stable across calls.
	mid := sat(6, 12)
	if !(mid > 0 && mid < 1) {
		t.Fatalf("sat(6,12)=%v, want in (0,1)", mid)
	}
	if again := sat(6, 12); math.Abs(again-mid) > tol {
		t.Fatalf("sat not stable across calls: %v vs %v", mid, again)
	}
	want := math.Log1p(6) / math.Log1p(12)
	if math.Abs(mid-want) > tol {
		t.Fatalf("sat(6,12)=%v, want %v", mid, want)
	}
	// Monotonic non-decreasing in x.
	prev := -1.0
	for x := 0.0; x <= 300; x += 7 {
		cur := sat(x, 250)
		if cur < prev-tol {
			t.Fatalf("sat not monotonic at x=%v: %v < prev %v", x, cur, prev)
		}
		prev = cur
	}
}

func TestSalienceScale(t *testing.T) {
	cases := []struct {
		typ  string
		want float64
	}{
		{"imessage", 250},
		{"email", 12},
		{"event", 6},
		{"filesystem", 12}, // unknown -> email scale (conservative middle)
		{"", 12},           // empty -> email scale
	}
	for _, c := range cases {
		if got := channelScale(c.typ); got != c.want {
			t.Fatalf("channelScale(%q)=%v, want %v", c.typ, got, c.want)
		}
	}
}

func TestSalienceRecency(t *testing.T) {
	const tol = 1e-9
	vaultMax := rfc3339(t, "2026-06-01T00:00:00Z")

	// lastSeen == vaultMax -> 1.0 (most recent person).
	if got := recencyDecay(vaultMax, vaultMax); math.Abs(got-1) > tol {
		t.Fatalf("recencyDecay(max,max)=%v, want 1.0", got)
	}

	// Exactly one half-life (180 days) before vaultMax -> 0.5 (>= floor).
	half := rfc3339(t, "2025-12-03T00:00:00Z") // 180 days before 2026-06-01
	if got := recencyDecay(half, vaultMax); math.Abs(got-0.5) > 1e-6 {
		t.Fatalf("recencyDecay(-180d)=%v, want ~0.5", got)
	}

	// Two half-lives (360 days) -> 0.25, but floored to 0.40.
	twoHalf := rfc3339(t, "2025-06-06T00:00:00Z") // 360 days before
	if got := recencyDecay(twoHalf, vaultMax); math.Abs(got-recencyFloor) > 1e-6 {
		t.Fatalf("recencyDecay(-360d)=%v, want floor %v", got, recencyFloor)
	}

	// Very old lastSeen -> exactly the floor.
	ancient := rfc3339(t, "2000-01-01T00:00:00Z")
	if got := recencyDecay(ancient, vaultMax); got != recencyFloor {
		t.Fatalf("recencyDecay(ancient)=%v, want exactly floor %v", got, recencyFloor)
	}

	// Empty/unparseable lastSeen or vaultMax -> floor, no panic.
	if got := recencyDecay("", vaultMax); got != recencyFloor {
		t.Fatalf("recencyDecay(\"\",max)=%v, want floor", got)
	}
	if got := recencyDecay(vaultMax, ""); got != recencyFloor {
		t.Fatalf("recencyDecay(max,\"\")=%v, want floor", got)
	}
	if got := recencyDecay("garbage", "also-bad"); got != recencyFloor {
		t.Fatalf("recencyDecay(garbage)=%v, want floor", got)
	}

	// Never below the floor for any Δ.
	wayOld := rfc3339(t, "1990-01-01T00:00:00Z")
	if got := recencyDecay(wayOld, vaultMax); got < recencyFloor {
		t.Fatalf("recencyDecay floor violated: %v < %v", got, recencyFloor)
	}

	// Determinism: same args -> same result.
	a := recencyDecay(half, vaultMax)
	b := recencyDecay(half, vaultMax)
	if a != b {
		t.Fatalf("recencyDecay non-deterministic: %v vs %v", a, b)
	}
}

func TestSalienceMicros(t *testing.T) {
	cases := []struct {
		s    float64
		want int64
	}{
		{0, 0},
		{1, 1_000_000},
		{0.4, 400_000},
		{0.123456, 123_456},
		{0.1234565, 123_457},   // round-half-up via math.Round
		{0.1234564, 123_456},   // rounds down
		{0.9999995, 1_000_000}, // rounds up to the ceiling
	}
	for _, c := range cases {
		if got := salienceMicros(c.s); got != c.want {
			t.Fatalf("salienceMicros(%v)=%d, want %d", c.s, got, c.want)
		}
	}
}

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

func TestSalienceMessageCount(t *testing.T) {
	// Present + parseable -> the real count.
	if got := metaMessageCount(Memory{Meta: map[string]any{"message_count": "42"}}); got != 42 {
		t.Fatalf("metaMessageCount(42)=%d, want 42", got)
	}
	// Whitespace-tolerant.
	if got := metaMessageCount(Memory{Meta: map[string]any{"message_count": " 7 "}}); got != 7 {
		t.Fatalf("metaMessageCount(' 7 ')=%d, want 7", got)
	}
	// Absent key -> fallback 1.
	if got := metaMessageCount(Memory{Meta: map[string]any{}}); got != 1 {
		t.Fatalf("metaMessageCount(absent)=%d, want 1", got)
	}
	// Nil Meta -> fallback 1.
	if got := metaMessageCount(Memory{}); got != 1 {
		t.Fatalf("metaMessageCount(nilMeta)=%d, want 1", got)
	}
	// Empty / unparseable / non-string / sub-1 -> fallback 1.
	for _, v := range []any{"", "garbage", "0", "-3", 42 /* not a string */, "3.5"} {
		if got := metaMessageCount(Memory{Meta: map[string]any{"message_count": v}}); got != 1 {
			t.Fatalf("metaMessageCount(%v)=%d, want fallback 1", v, got)
		}
	}
}

func TestSalienceScoreServiceGate(t *testing.T) {
	// A service-kind identity scores exactly 0 regardless of volume/recency.
	in := salienceInput{
		kind:             "service",
		perChannelVolume: map[string]float64{"email": 1000},
		channels:         map[string]bool{"email": true},
		lastSeen:         "2026-06-01T00:00:00Z",
	}
	if got := scoreSalience(in, "2026-06-01T00:00:00Z"); got != 0 {
		t.Fatalf("service score=%d, want 0", got)
	}
	// The same evidence as a person scores > 0.
	in.kind = "person"
	if got := scoreSalience(in, "2026-06-01T00:00:00Z"); got <= 0 {
		t.Fatalf("person score=%d, want > 0", got)
	}
}

func TestSalienceScoreBounds(t *testing.T) {
	// Max evidence: every channel saturated, freshest lastSeen, person.
	in := salienceInput{
		kind: "person",
		perChannelVolume: map[string]float64{
			"imessage": 1e9, "email": 1e9, "event": 1e9,
		},
		channels: map[string]bool{"imessage": true, "email": true, "event": true},
		lastSeen: "2026-06-01T00:00:00Z",
	}
	got := scoreSalience(in, "2026-06-01T00:00:00Z")
	if got < 0 || got > 1_000_000 {
		t.Fatalf("score=%d out of [0,1e6] — Volume clamp/[0,1] invariant broken", got)
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
