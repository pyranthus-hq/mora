package salience

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"math"
	"testing"
	"time"
)

func rfc3339(t *testing.T, s string) string {
	t.Helper()
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Fatalf("bad fixture time %q: %v", s, err)
	}
	return s
}
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

func TestSalienceMessageCount(t *testing.T) {
	// Present + parseable -> the real count.
	if got := metaMessageCount(memory.Memory{Meta: map[string]any{"message_count": "42"}}); got != 42 {
		t.Fatalf("metaMessageCount(42)=%d, want 42", got)
	}
	// Whitespace-tolerant.
	if got := metaMessageCount(memory.Memory{Meta: map[string]any{"message_count": " 7 "}}); got != 7 {
		t.Fatalf("metaMessageCount(' 7 ')=%d, want 7", got)
	}
	// Absent key -> fallback 1.
	if got := metaMessageCount(memory.Memory{Meta: map[string]any{}}); got != 1 {
		t.Fatalf("metaMessageCount(absent)=%d, want 1", got)
	}
	// Nil Meta -> fallback 1.
	if got := metaMessageCount(memory.Memory{}); got != 1 {
		t.Fatalf("metaMessageCount(nilMeta)=%d, want 1", got)
	}
	// Empty / unparseable / non-string / sub-1 -> fallback 1.
	for _, v := range []any{"", "garbage", "0", "-3", 42 /* not a string */, "3.5"} {
		if got := metaMessageCount(memory.Memory{Meta: map[string]any{"message_count": v}}); got != 1 {
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

func TestExportedSalienceSurface(t *testing.T) {
	if Saturate(0, 1) != 0 || ChannelScale("email") != emailSatScale || RecencyDecay("bad", "bad") != recencyFloor || Micros(0.5) != 500000 {
		t.Fatal("exported scalar surface changed")
	}
	in := Input{Kind: "person", PerChannelVolume: map[string]float64{"email": 1}, Channels: map[string]bool{"email": true}, LastSeen: "2026-01-01T00:00:00Z"}
	if Score(in, "2026-01-01T00:00:00Z") <= 0 {
		t.Fatal("score must be positive")
	}
	if MessageCount(memory.Memory{Meta: map[string]any{"message_count": "3"}}) != 3 {
		t.Fatal("message count changed")
	}
}
