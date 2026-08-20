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

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
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
