package mora

import (
	"reflect"
	"testing"
	"time"
)

// ---- Phase 14-05: salience-ranked digest ordering (SC#3) ----
//
// These tests pin that the digest orders/selects WITHIN a section by salience
// (most-salient first), not recency alone — via the EXISTING capRecency seam —
// while staying deterministic, byte-clean, and file-based (no DB). The salience
// per item comes from the SAME 14-01 kernel (aggregatePersonSalience) the entity
// graph consumes, so the digest and `mora graph` rank on identical math.

const salT0 = "2026-06-01T00:00:00Z"

// ---- Task 1: digestMemorySalience — per-item salience from the shared kernel ----

// TestDigestMemorySaliencePersonOutranksService pins that a memory whose
// most-salient participant is a real person scores ABOVE a service-only thread
// (which scores exactly 0 via the HumanGate) and a no-participant memo (also 0).
func TestDigestMemorySaliencePersonOutranksService(t *testing.T) {
	person := "friend@example.com"
	// A service→service thread: EVERY participant is a service-classified address, so
	// the memory's max-participant salience is 0 (no human in the thread). A real
	// inbox normally has YOU (a person) on the `to`, but a service-to-service relay
	// (e.g. bounce notifications) has none — the case the HumanGate must zero.
	mems := []Memory{
		emailMemory("em_person", person, "me@example.com", salT0, 9),
		emailMemory("em_service", "no-reply@billing.example.com", "bounce@mailer.example.com", salT0, 9),
		{ID: "memo_none", Type: "note", CreatedAt: salT0}, // no participants
	}

	memSal := digestMemorySalience(mems)

	if memSal["em_person"] <= 0 {
		t.Fatalf("person thread memSal=%d, want > 0", memSal["em_person"])
	}
	if memSal["em_service"] != 0 {
		t.Fatalf("service-only thread memSal=%d, want exactly 0", memSal["em_service"])
	}
	if memSal["memo_none"] != 0 {
		t.Fatalf("no-participant memo memSal=%d, want exactly 0", memSal["memo_none"])
	}
	if memSal["em_person"] <= memSal["em_service"] {
		t.Fatalf("person (%d) must outrank service (%d)", memSal["em_person"], memSal["em_service"])
	}
}

// TestDigestMemorySalienceMatchesKernel asserts the digest does NOT re-implement
// the salience math: each memory's score is the MAX of its participants' kernel
// micros — the single source of truth shared with the graph.
func TestDigestMemorySalienceMatchesKernel(t *testing.T) {
	a := "alice@example.com"
	b := "+15550001111"
	mems := []Memory{
		emailMemory("em_a", a, "me@example.com", salT0, 12),
		imsgMemory("im_b", b, "Bob Roberts", salT0, 400),
	}

	memSal := digestMemorySalience(mems)
	kernel := aggregatePersonSalience(mems)

	if memSal["em_a"] != kernel[personID(a)] {
		t.Fatalf("em_a memSal=%d, want kernel[%s]=%d — must reuse the kernel", memSal["em_a"], personID(a), kernel[personID(a)])
	}
	if memSal["im_b"] != kernel[personID(b)] {
		t.Fatalf("im_b memSal=%d, want kernel[%s]=%d — must reuse the kernel", memSal["im_b"], personID(b), kernel[personID(b)])
	}
}

// TestDigestMemorySalienceMaxFold pins that a memory with MULTIPLE participants
// takes the MAX salience (the most-salient human in the thread), never the sum —
// mirroring the graph's canon remap so the digest and graph agree.
func TestDigestMemorySalienceMaxFold(t *testing.T) {
	// vip: high iMessage volume (saturates high) — the strongest signal.
	// lurker: a thin email contact (lower micros).
	vip := "+15551112222"
	lurker := "quiet@example.com"
	mems := []Memory{
		imsgMemory("im_vip", vip, "VIP", salT0, 400),
		emailMemory("em_lurker", lurker, "me@example.com", salT0, 1),
		// A group memory with BOTH participants — its salience is the max (vip).
		{
			ID:        "im_group",
			Type:      "imessage",
			CreatedAt: salT0,
			Meta: map[string]any{
				"occurred_at":   salT0,
				"message_count": "5",
				"participants": []any{
					map[string]any{"handle": vip, "name": "VIP"},
					map[string]any{"handle": lurker, "name": "Quiet"},
				},
			},
		},
	}

	memSal := digestMemorySalience(mems)
	kernel := aggregatePersonSalience(mems)

	wantMax := kernel[personID(vip)]
	if kernel[personID(lurker)] >= wantMax {
		t.Fatalf("fixture invalid: lurker (%d) should be below vip (%d)", kernel[personID(lurker)], wantMax)
	}
	if memSal["im_group"] != wantMax {
		t.Fatalf("group memSal=%d, want max participant = %d (NOT a sum)", memSal["im_group"], wantMax)
	}
}

// TestDigestMemorySalienceSkipsTombstones pins that a tombstoned memory contributes
// no entry (mirrors buildGraph's live-stats rule).
func TestDigestMemorySalienceSkipsTombstones(t *testing.T) {
	dead := emailMemory("em_dead", "ghost@example.com", "me@example.com", salT0, 9)
	dead.DeletedAt = salT0
	mems := []Memory{dead}

	memSal := digestMemorySalience(mems)
	if _, ok := memSal["em_dead"]; ok {
		t.Fatalf("tombstoned memory must not appear in memSal, got %d", memSal["em_dead"])
	}
}

// TestDigestMemorySalienceDeterministic pins that two calls over the same input
// produce DeepEqual maps (no map-iteration-order leak into the values).
func TestDigestMemorySalienceDeterministic(t *testing.T) {
	mems := []Memory{
		emailMemory("em_a", "alice@example.com", "me@example.com", salT0, 12),
		imsgMemory("im_b", "+15550001111", "Bob", salT0, 400),
		emailMemory("em_svc", "no-reply@billing.example.com", "me@example.com", salT0, 9),
	}
	a := digestMemorySalience(mems)
	b := digestMemorySalience(mems)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("digestMemorySalience not deterministic:\n a=%v\n b=%v", a, b)
	}
}

// ---- Task 2: salience-primary ordering + cap selection in capRecency ----

// salItem is a tiny helper to build a tsItem with an explicit salience + ts.
func salItem(id string, sal int64, ts time.Time) tsItem {
	return tsItem{item: DigestItem{ID: id, Title: id}, ts: ts, sal: sal}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

// TestCapRecencySalienceLeadsSection pins SC#3: within a section, a HIGHER-salience
// item leads even when a DIFFERENT item is more recent.
func TestCapRecencySalienceLeadsSection(t *testing.T) {
	older := mustTime(t, "2026-06-01T00:00:00Z")
	newer := mustTime(t, "2026-06-08T00:00:00Z")
	// "recent_noise" is the most RECENT but low salience; "salient_old" is older but
	// the most salient — it must lead.
	tis := []tsItem{
		salItem("recent_noise", 100, newer),
		salItem("salient_old", 900_000, older),
	}
	items, more := capRecency(tis, 8, false)
	if more != 0 {
		t.Fatalf("more=%d, want 0", more)
	}
	if items[0].ID != "salient_old" {
		t.Fatalf("section leader=%q, want salient_old (salience must beat recency)", items[0].ID)
	}
	if items[1].ID != "recent_noise" {
		t.Fatalf("second=%q, want recent_noise", items[1].ID)
	}
}

// TestCapRecencySalienceSurvivesCap pins that the most-salient item is KEPT through
// truncation even when it is NOT the most recent — the SC#3 inversion guard. Under
// pure recency it would have been dropped past the cap.
func TestCapRecencySalienceSurvivesCap(t *testing.T) {
	base := mustTime(t, "2026-06-01T00:00:00Z")
	// cap=2. Two very recent but LOW-salience items, plus one older HIGH-salience
	// item. Pure recency would drop the salient one; salience ordering keeps it.
	tis := []tsItem{
		salItem("recent_a", 10, base.Add(48*time.Hour)),
		salItem("recent_b", 20, base.Add(24*time.Hour)),
		salItem("salient_old", 900_000, base),
	}
	items, more := capRecency(tis, 2, false)
	if more != 1 {
		t.Fatalf("more=%d, want 1 (one item past the cap)", more)
	}
	if items[0].ID != "salient_old" {
		t.Fatalf("kept[0]=%q, want salient_old (must survive the cap)", items[0].ID)
	}
	// The kept set must CONTAIN the salient item; the dropped one is the least salient.
	kept := map[string]bool{}
	for _, it := range items {
		kept[it.ID] = true
	}
	if !kept["salient_old"] {
		t.Fatalf("salient_old was truncated — the exact SC#3 failure this guards against")
	}
	if kept["recent_a"] {
		t.Fatalf("recent_a (least salient) should have been the one dropped, but it was kept")
	}
}

// TestCapRecencyZeroSalienceSinksToBottom pins that 0-salience items (services /
// no-participant) fall BELOW salient ones, while preserving recency order AMONG the
// equal-salience zeros.
func TestCapRecencyZeroSalienceSinksToBottom(t *testing.T) {
	base := mustTime(t, "2026-06-01T00:00:00Z")
	tis := []tsItem{
		salItem("zero_old", 0, base),
		salItem("zero_new", 0, base.Add(24*time.Hour)),
		salItem("human", 500_000, base.Add(-72*time.Hour)), // oldest, but salient
	}
	items, _ := capRecency(tis, 8, false)
	if items[0].ID != "human" {
		t.Fatalf("leader=%q, want human (salient leads the zeros)", items[0].ID)
	}
	// Among the zeros, recency order is preserved: zero_new (more recent) before zero_old.
	if items[1].ID != "zero_new" || items[2].ID != "zero_old" {
		t.Fatalf("zero order=[%s,%s], want [zero_new,zero_old] (recency preserved among zeros)", items[1].ID, items[2].ID)
	}
}

// TestCapRecencyEqualSalienceRecencyTieBreak pins the existing recency tie-break is
// preserved when salience is equal: more recent leads, then id< on exact-instant ties.
func TestCapRecencyEqualSalienceRecencyTieBreak(t *testing.T) {
	t0 := mustTime(t, "2026-06-01T00:00:00Z")
	t1 := mustTime(t, "2026-06-02T00:00:00Z")
	tis := []tsItem{
		salItem("b_same_instant", 500, t0),
		salItem("a_same_instant", 500, t0), // same salience + instant → id< decides
		salItem("recent", 500, t1),         // same salience, more recent → leads
	}
	items, _ := capRecency(tis, 8, false)
	want := []string{"recent", "a_same_instant", "b_same_instant"}
	for i, w := range want {
		if items[i].ID != w {
			t.Fatalf("order[%d]=%q, want %q (recency then id< tie-break under equal salience)", i, items[i].ID, w)
		}
	}
}

// TestCapRecencyDeterministic pins byte-identical ordering across two passes.
func TestCapRecencyDeterministic(t *testing.T) {
	base := mustTime(t, "2026-06-01T00:00:00Z")
	build := func() []tsItem {
		return []tsItem{
			salItem("c", 100, base),
			salItem("a", 100, base),
			salItem("b", 900_000, base.Add(time.Hour)),
			salItem("d", 0, base.Add(2*time.Hour)),
		}
	}
	first, _ := capRecency(build(), 8, false)
	second, _ := capRecency(build(), 8, false)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("capRecency not deterministic:\n first=%v\n second=%v", first, second)
	}
}

// TestBuildDigestSalienceFileBased asserts buildDigest threads salience into the
// rendered ordering end-to-end via the EXISTING window path, staying file-based
// (no DB). It writes a vault with two gmail threads in the same instance: a
// high-salience iMessage-volume human is NOT applicable to gmail, so we use two
// email threads where one participant (heavy) outscores the other (light) by
// message volume — the heavier thread must lead its section.
func TestBuildDigestSalienceWindowOrdering(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{VaultDir: dir, DataDir: dir, StateDir: dir}

	now := mustTime(t, "2026-06-08T12:00:00Z")
	// heavy: 12-message thread (saturates email volume high). light: 1-message.
	// Both created at the SAME instant so RECENCY can't decide — salience must.
	created := "2026-06-08T10:00:00Z"
	heavy := emailMemory("em_heavy", "heavy@example.com", "me@example.com", created, 12)
	heavy.Provider = "gmail"
	heavy.ProviderID = "inbox"
	heavy.Title = "Heavy thread"
	heavy.Text = "heavy body"
	light := emailMemory("em_light", "light@example.com", "me@example.com", created, 1)
	light.Provider = "gmail"
	light.ProviderID = "inbox"
	light.Title = "Light thread"
	light.Text = "light body"

	if err := writeMemory(cfg, heavy); err != nil {
		t.Fatalf("writeMemory heavy: %v", err)
	}
	if err := writeMemory(cfg, light); err != nil {
		t.Fatalf("writeMemory light: %v", err)
	}
	cfg = ungatedDigestConfig(cfg)

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 24})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}

	var sec *DigestSection
	for i := range d.Sections {
		if len(d.Sections[i].Items) >= 2 {
			sec = &d.Sections[i]
			break
		}
	}
	if sec == nil {
		t.Fatalf("no section with >=2 items; sections=%+v", d.Sections)
	}
	if sec.Items[0].ID != "em_heavy" {
		t.Fatalf("section leader=%q, want em_heavy (higher-volume thread must lead by salience)", sec.Items[0].ID)
	}
}

// TestCollapseLowSignal pins the digest noise-collapse: salient items always survive,
// at most digestLowSignalFloor low-signal items stay visible, and the rest are counted
// (never silently dropped). Pure function → deterministic.
func TestCollapseLowSignal(t *testing.T) {
	items := []DigestItem{
		{ID: "h1"},                  // salient
		{ID: "h2"},                  // salient
		{ID: "s1", LowSignal: true}, // floor keeps s1, s2
		{ID: "s2", LowSignal: true},
		{ID: "s3", LowSignal: true}, // collapsed
		{ID: "s4", LowSignal: true}, // collapsed
	}
	displayed, collapsed := collapseLowSignal(items)
	if collapsed != 2 {
		t.Fatalf("collapsed = %d, want 2 (4 low-signal − floor %d)", collapsed, digestLowSignalFloor)
	}
	if len(displayed) != 4 {
		t.Fatalf("displayed = %d items, want 4 (2 salient + %d floor)", len(displayed), digestLowSignalFloor)
	}
	// Both salient items must survive, in order, and never be collapsed.
	if displayed[0].ID != "h1" || displayed[1].ID != "h2" {
		t.Fatalf("salient items dropped/reordered: %+v", displayed)
	}
	// An all-salient section is untouched.
	allHuman := []DigestItem{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	d2, c2 := collapseLowSignal(allHuman)
	if c2 != 0 || len(d2) != 3 {
		t.Fatalf("all-salient section must be untouched: displayed=%d collapsed=%d", len(d2), c2)
	}
	// A pure-noise section keeps exactly the floor and counts the rest.
	noise := []DigestItem{{ID: "n1", LowSignal: true}, {ID: "n2", LowSignal: true}, {ID: "n3", LowSignal: true}}
	d3, c3 := collapseLowSignal(noise)
	if len(d3) != digestLowSignalFloor || c3 != 1 {
		t.Fatalf("pure-noise section: displayed=%d (want %d) collapsed=%d (want 1)", len(d3), digestLowSignalFloor, c3)
	}
}
