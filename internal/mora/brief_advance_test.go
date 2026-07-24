package mora

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Issue #62 defect 1 — the commit must follow what actually RENDERED into the
// budgeted brief, never the pre-truncation per-source cap. The scheduled --advance
// path (advanceBrief) budgets the brief to the persist budget, writes the artifact,
// and advances the watermark ONLY over items that survived the byte budget — so a
// budget-clipped urgent email re-surfaces next run instead of being marked seen and
// lost forever.

// steadyStateSnapshots pre-commits an empty-but-stamped watermark for each key so
// classify treats subsequently-seeded memories as steady-state "new" deltas (not a
// cold-start courtesy window).
func steadyStateSnapshots(t *testing.T, cfg Config, now time.Time, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if err := saveBriefSnapshot(cfg, briefSnapshot{Key: k, Items: map[string]string{}}, now.Add(-24*time.Hour)); err != nil {
			t.Fatalf("seed steady snapshot %q: %v", k, err)
		}
	}
}

func gmailItemLine(t *testing.T, d Digest, title string) (string, bool) {
	t.Helper()
	for _, s := range d.Sections {
		for _, it := range s.Items {
			if it.Title == title {
				return renderDigestItemLine(it), true
			}
		}
	}
	return "", false
}

// TestAdvanceCommitsOnlyBudgetSurvivors: a low-volume urgent Gmail thread that is
// within the per-source cap but past the byte budget is NOT committed and re-surfaces
// next run; the same item under a generous budget IS committed and does not re-surface.
func TestAdvanceCommitsOnlyBudgetSurvivors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	enableSources(t, cfg, "gmail", "calendar", "imessage")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "calendar", now.Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "imessage", now.Add(-1*time.Hour))
	steadyStateSnapshots(t, cfg, now, "gmail", "calendar", "imessage")

	// Higher-rank noise (calendar rank 0, texts rank 1) fills the budget; the urgent
	// gmail thread (rank 2) is the tail that clips.
	digestSeed(t, cfg, "calendar", "Team standup", -2*time.Hour, now)
	digestSeed(t, cfg, "imessage", "Lunch plans", 1*time.Hour, now)
	digestSeed(t, cfg, "gmail", "UrgentApproval", 1*time.Hour, now)

	// Measure the full brief and clip exactly the gmail line.
	preview, err := buildDigest(cfg, now, briefOpts{})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	line, ok := gmailItemLine(t, preview, "UrgentApproval")
	if !ok {
		t.Fatalf("precondition: UrgentApproval not surfaced in preview")
	}
	full := renderDigest(preview, 1<<20)
	tightBudget := len(full) - len(line) // one line short: gmail clips.

	// --- clip: advance under the tight budget, persisting the artifact ---
	if _, _, aerr := advanceBrief(cfg, now, briefOpts{advance: true}, tightBudget, true); aerr != nil {
		t.Fatalf("advanceBrief(tight): %v", aerr)
	}
	artifact, err := os.ReadFile(briefArtifactPath(cfg, now))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if strings.Contains(string(artifact), "UrgentApproval") {
		t.Fatalf("tight-budget artifact must NOT contain the clipped urgent item;\n%s", artifact)
	}
	snap := loadBriefSnapshot(cfg, "gmail")
	if _, seen := snap.Items["id-UrgentApproval"]; seen {
		t.Fatalf("the clipped urgent item must NOT be committed to the watermark; snap=%v", snap.Items)
	}
	// It re-surfaces on the next preview (never marked seen).
	after, err := buildDigest(cfg, now.Add(1*time.Hour), briefOpts{})
	if err != nil {
		t.Fatalf("post preview: %v", err)
	}
	if _, ok := gmailItemLine(t, after, "UrgentApproval"); !ok {
		t.Fatalf("clipped urgent item must re-surface next run")
	}
}

// TestAdvanceCommitsSurvivorUnderGenerousBudget: the SAME urgent item, when it fits
// the budget, IS committed and does not re-surface (proving the fix does not simply
// stop committing everything).
func TestAdvanceCommitsSurvivorUnderGenerousBudget(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	enableSources(t, cfg, "gmail", "calendar", "imessage")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "calendar", now.Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "imessage", now.Add(-1*time.Hour))
	steadyStateSnapshots(t, cfg, now, "gmail", "calendar", "imessage")
	digestSeed(t, cfg, "calendar", "Team standup", -2*time.Hour, now)
	digestSeed(t, cfg, "imessage", "Lunch plans", 1*time.Hour, now)
	digestSeed(t, cfg, "gmail", "UrgentApproval", 1*time.Hour, now)

	if _, _, aerr := advanceBrief(cfg, now, briefOpts{advance: true}, 1<<20, true); aerr != nil {
		t.Fatalf("advanceBrief(generous): %v", aerr)
	}
	artifact, err := os.ReadFile(briefArtifactPath(cfg, now))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !strings.Contains(string(artifact), "UrgentApproval") {
		t.Fatalf("generous-budget artifact must contain the urgent item;\n%s", artifact)
	}
	snap := loadBriefSnapshot(cfg, "gmail")
	if _, seen := snap.Items["id-UrgentApproval"]; !seen {
		t.Fatalf("a rendered urgent item MUST be committed; snap=%v", snap.Items)
	}
	after, err := buildDigest(cfg, now.Add(1*time.Hour), briefOpts{})
	if err != nil {
		t.Fatalf("post preview: %v", err)
	}
	if _, ok := gmailItemLine(t, after, "UrgentApproval"); ok {
		t.Fatalf("a committed urgent item must NOT re-surface next run")
	}
}

// TestAdvanceCollapsedSeriesCommitsAllMembers (codex risk #1): a collapsed recurring
// series renders as ONE line but stands for many stable ids; when that line survives
// the budget, the watermark advances over EVERY member so the series never re-floods
// instance-by-instance next run.
func TestAdvanceCollapsedSeriesCommitsAllMembers(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	enableSources(t, cfg, "calendar")
	seedSyncStatus(t, cfg, "calendar", now.Add(-1*time.Hour))
	// Three upcoming instances of one recurring series (collapse to ONE line).
	for i := 1; i <= 3; i++ {
		seedCalEvent(t, cfg, "Standup "+itoa(i), now.Add(time.Duration(i*24)*time.Hour), "series-standup")
	}
	cfg = ungatedDigestConfig(cfg)

	if _, _, err := advanceBrief(cfg, now, briefOpts{advance: true}, 1<<20, true); err != nil {
		t.Fatalf("advanceBrief: %v", err)
	}
	snap := loadBriefSnapshot(cfg, "calendar")
	for i := 1; i <= 3; i++ {
		id := "id-Standup " + itoa(i)
		if _, ok := snap.Items[id]; !ok {
			t.Fatalf("a surviving collapsed series line must commit ALL member ids; missing %q; snap=%v", id, snap.Items)
		}
	}
	after, err := buildDigest(cfg, now.Add(1*time.Hour), briefOpts{})
	if err != nil {
		t.Fatalf("post preview: %v", err)
	}
	if sec, ok := digestSections(after)["calendar"]; ok && len(sec.Items) != 0 {
		t.Fatalf("a fully-committed series must not re-surface; got %d items", len(sec.Items))
	}
}

// TestAdvanceClippedSeriesReSurfaces: the same collapsed series, when its ONE line is
// clipped by the budget, commits NONE of its members and re-surfaces next run.
func TestAdvanceClippedSeriesReSurfaces(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	enableSources(t, cfg, "calendar")
	seedSyncStatus(t, cfg, "calendar", now.Add(-1*time.Hour))
	for i := 1; i <= 3; i++ {
		seedCalEvent(t, cfg, "Standup "+itoa(i), now.Add(time.Duration(i*24)*time.Hour), "series-standup")
	}
	cfg = ungatedDigestConfig(cfg)

	preview, err := buildDigest(cfg, now, briefOpts{})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	cal := digestSections(preview)["calendar"]
	if len(cal.Items) != 1 {
		t.Fatalf("precondition: series should collapse to one line; got %d", len(cal.Items))
	}
	full := renderDigest(preview, 1<<20)
	tightBudget := len(full) - len(renderDigestItemLine(cal.Items[0])) // clip the series line.

	if _, _, err := advanceBrief(cfg, now, briefOpts{advance: true}, tightBudget, true); err != nil {
		t.Fatalf("advanceBrief: %v", err)
	}
	snap := loadBriefSnapshot(cfg, "calendar")
	for i := 1; i <= 3; i++ {
		if _, seen := snap.Items["id-Standup "+itoa(i)]; seen {
			t.Fatalf("a clipped series must NOT commit any member; snap=%v", snap.Items)
		}
	}
	after, err := buildDigest(cfg, now.Add(1*time.Hour), briefOpts{})
	if err != nil {
		t.Fatalf("post preview: %v", err)
	}
	if sec := digestSections(after)["calendar"]; len(sec.Items) != 1 {
		t.Fatalf("a clipped series must re-surface next run as one collapsed line; got %d", len(sec.Items))
	}
}

// TestAdvanceColdStartArchiveVsClipped (defect 1b): on the FIRST run, an
// out-of-window archive item is baselined (does not flood later runs), while an
// in-window item chosen for display but clipped by the byte budget is NOT baselined
// and re-surfaces next run.
func TestAdvanceColdStartArchiveVsClipped(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	enableSources(t, cfg, "gmail", "calendar", "imessage")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "calendar", now.Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "imessage", now.Add(-1*time.Hour))
	// NO prior snapshots => cold start for every source.

	// Out-of-window archive (older than the 7d cold-start window).
	digestSeed(t, cfg, "gmail", "AncientArchive", 30*24*time.Hour, now)
	// In-window noise + in-window urgent (clipped by budget).
	digestSeed(t, cfg, "calendar", "Team standup", -2*time.Hour, now)
	digestSeed(t, cfg, "imessage", "Lunch plans", 1*time.Hour, now)
	digestSeed(t, cfg, "gmail", "UrgentApproval", 1*time.Hour, now)
	cfg = ungatedDigestConfig(cfg)

	// Full cold-start render, then clip the in-window gmail urgent line.
	preview, err := buildDigest(cfg, now, briefOpts{})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	line, ok := gmailItemLine(t, preview, "UrgentApproval")
	if !ok {
		t.Fatalf("precondition: UrgentApproval not displayed in the cold-start window")
	}
	full := renderDigest(preview, 1<<20)
	tightBudget := len(full) - len(line)

	if _, _, aerr := advanceBrief(cfg, now, briefOpts{advance: true}, tightBudget, true); aerr != nil {
		t.Fatalf("advanceBrief: %v", aerr)
	}

	snap := loadBriefSnapshot(cfg, "gmail")
	if _, seen := snap.Items["id-AncientArchive"]; !seen {
		t.Fatalf("out-of-window archive MUST be baselined on cold start (flood suppression); snap=%v", snap.Items)
	}
	if _, seen := snap.Items["id-UrgentApproval"]; seen {
		t.Fatalf("an in-window item clipped by the budget must NOT be baselined; snap=%v", snap.Items)
	}
	// Archive does not re-surface; clipped in-window item does.
	after, err := buildDigest(cfg, now.Add(1*time.Hour), briefOpts{})
	if err != nil {
		t.Fatalf("post preview: %v", err)
	}
	if _, ok := gmailItemLine(t, after, "AncientArchive"); ok {
		t.Fatalf("baselined archive must NOT re-surface")
	}
	if _, ok := gmailItemLine(t, after, "UrgentApproval"); !ok {
		t.Fatalf("clipped in-window item must re-surface as a steady-state delta")
	}
}
