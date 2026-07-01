package mora

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// meeting_mt_cover_test.go (+ the sibling *_mt_cover_test.go files) push the four
// synthesis-envelope files (meetingprep.go, brief.go, think.go, openloops.go)
// toward full statement coverage with REAL behavior/error assertions — the error
// and empty paths the happy-path suites leave uncovered. Every helper here is
// mt-prefixed and every test is TestMt_* so this file merges additively with the
// existing suite and any sibling coverage worker.

// ---------------------------------------------------------------------------
// Shared mt-prefixed test fixtures (used across all *_mt_cover_test.go files).
// ---------------------------------------------------------------------------

// mtBreakIndex stales the on-disk index (user_version=0) AND pins the auto-heal
// policy OFF, so any subsequent ensureIndexDB/openIndexRO/hybridSearchTrace call
// fails LOUDLY with the actionable "mora index rebuild" error instead of silently
// self-healing. Mirrors makeStaleIndex + the semantic-embedder policy stub used in
// index_schema_test.go. The auto-heal var is restored on cleanup.
func mtBreakIndex(t *testing.T, cfg Config) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatalf("open index for staling: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		db.Close()
		t.Fatalf("stale index user_version: %v", err)
	}
	db.Close()
	prev := indexAutoHeal
	indexAutoHeal = func(Config) bool { return false }
	t.Cleanup(func() { indexAutoHeal = prev })
}

// mtScratchDB opens a fresh, empty temp-file sqlite DB (no mora schema) the caller
// can shape with hand-crafted rows to drive Scan/type-mismatch error paths. Closed
// on cleanup.
func mtScratchDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mt-scratch.db"))
	if err != nil {
		t.Fatalf("open scratch db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// mtClosedDB opens then closes a temp sqlite DB, so every Query/Exec on the
// returned handle fails with "sql: database is closed" — the cheapest deterministic
// driver-level query error for the QueryContext error branches.
func mtClosedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mt-closed.db"))
	if err != nil {
		t.Fatalf("open closed db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping before close: %v", err)
	}
	db.Close()
	return db
}

// mtRunPrepErr runs `mora prep <args>` and REQUIRES it to return an error,
// returning that error for message assertions.
func mtRunPrepErr(t *testing.T, args ...string) error {
	t.Helper()
	var out bytes.Buffer
	full := append([]string{"prep"}, args...)
	err := Run(context.Background(), full, &out, &out, strings.NewReader(""))
	if err == nil {
		t.Fatalf("Run(prep %v) = nil error, want failure\noutput:\n%s", args, out.String())
	}
	return err
}

// mtMakeMemoriesUnreadable creates an unreadable subdirectory under the vault's
// memories/ tree so allMemoryFiles' WalkDir surfaces a (non-NotExist) permission
// error. Skips as root (where 0000 is bypassed). Perms are restored on cleanup so
// the temp dir can be removed.
func mtMakeMemoriesUnreadable(t *testing.T, cfg Config) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("runs as root — 0000 perms are bypassed, so the walk error can't be provoked")
	}
	bad := filepath.Join(memoriesRoot(cfg), "mtbad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatalf("mkdir bad dir: %v", err)
	}
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatalf("chmod 0000: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o755) })
}

// ---------------------------------------------------------------------------
// meetingPrepMCPPayload — budget discipline
// ---------------------------------------------------------------------------

// TestMt_MeetingPrepMCPPayloadNilEvent: a no-event result is returned untouched
// (the MCP budget pass is a no-op when there's nothing to prep).
func TestMt_MeetingPrepMCPPayloadNilEvent(t *testing.T) {
	in := MeetingPrepResult{Event: nil, SynthesisPrompt: meetingPrepNoEventPrompt}
	out := meetingPrepMCPPayload(in, 100)
	if out.Event != nil {
		t.Fatalf("nil-event payload gained an event: %+v", out.Event)
	}
	if out.SynthesisPrompt != meetingPrepNoEventPrompt {
		t.Fatalf("nil-event payload rewrote the prompt: %q", out.SynthesisPrompt)
	}
}

// TestMt_MeetingPrepMCPPayloadTrims: >25 attendees are capped and evidence is
// byte-budgeted (kept prefix stops once the running size would blow the budget),
// then the prompt is rebuilt so it cites only the surviving evidence.
func TestMt_MeetingPrepMCPPayloadTrims(t *testing.T) {
	ev := &MeetingEvent{StableID: "evt", Title: "Sync", OccurredAt: "2026-06-14T14:00:00Z", Source: "google"}
	var attendees []PrepAttendee
	for i := 0; i < meetingPrepMaxAttendees+7; i++ {
		attendees = append(attendees, PrepAttendee{PersonID: "person:a" + strconv.Itoa(i), Display: "A" + strconv.Itoa(i)})
	}
	var evidence []ThinkEvidence
	for i := 0; i < 6; i++ {
		evidence = append(evidence, ThinkEvidence{
			StableID: "m" + strconv.Itoa(i), Title: "Thread " + strconv.Itoa(i), Scope: "personal",
			CreatedAt: "2026-06-10T00:00:00Z", Snippet: strings.Repeat("context ", 12),
		})
	}
	in := MeetingPrepResult{Event: ev, Attendees: attendees, Evidence: evidence}

	// Budget small enough that only the first evidence item survives (reserve = /3).
	out := meetingPrepMCPPayload(in, 300)

	if len(out.Attendees) != meetingPrepMaxAttendees {
		t.Fatalf("attendees = %d, want capped to %d", len(out.Attendees), meetingPrepMaxAttendees)
	}
	if len(out.Evidence) == 0 || len(out.Evidence) >= len(evidence) {
		t.Fatalf("evidence = %d, want a trimmed non-empty prefix of %d", len(out.Evidence), len(evidence))
	}
	// The rebuilt prompt cites only surviving evidence — the dropped ids must be gone.
	dropped := evidence[len(out.Evidence)].StableID
	if strings.Contains(out.SynthesisPrompt, dropped) {
		t.Fatalf("rebuilt prompt still cites dropped evidence %q:\n%s", dropped, out.SynthesisPrompt)
	}
	if !strings.Contains(out.SynthesisPrompt, out.Evidence[0].StableID) {
		t.Fatalf("rebuilt prompt dropped a surviving citation %q", out.Evidence[0].StableID)
	}
}

// ---------------------------------------------------------------------------
// cmdPrep — CLI error/flag paths
// ---------------------------------------------------------------------------

// TestMt_CmdPrepFlagParseError: an unknown flag fails flag parsing loudly.
func TestMt_CmdPrepFlagParseError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	if err := mtRunPrepErr(t, "--bogus"); !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %v, want a flag-parse error naming --bogus", err)
	}
}

// TestMt_CmdPrepBadAt: a non-RFC3339 --at is rejected with an actionable message.
func TestMt_CmdPrepBadAt(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	if err := mtRunPrepErr(t, "--at", "not-a-time"); !strings.Contains(err.Error(), "invalid --at") {
		t.Fatalf("err = %v, want an invalid --at error", err)
	}
}

// TestMt_CmdPrepLoadConfigError: a config.toml that is a DIRECTORY (unreadable as a
// file) makes loadConfig surface a non-NotExist error rather than defaulting.
func TestMt_CmdPrepLoadConfigError(t *testing.T) {
	withTempHome(t)
	cfgPath := filepath.Join(defaultConfig().ConfigDir, "config.toml")
	if err := os.MkdirAll(cfgPath, 0o755); err != nil { // config.toml is now a directory
		t.Fatalf("mkdir config.toml-as-dir: %v", err)
	}
	if err := mtRunPrepErr(t, "--at", "2026-06-14T12:00:00Z"); err == nil {
		t.Fatal("want a loadConfig error when config.toml is unreadable")
	}
}

// TestMt_CmdPrepNamedFilterResolveError: `mora prep <name>` over a broken index
// surfaces the entity-resolution error (the name → resolveEntityFilter branch).
func TestMt_CmdPrepNamedFilterResolveError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, eventMemFull("evt", "Sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya"}, "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	mtBreakIndex(t, cfg)
	if err := mtRunPrepErr(t, "--at", now.Format(time.RFC3339), "Riya"); err == nil {
		t.Fatal("want an entity-resolution error over a broken index")
	}
}

// TestMt_CmdPrepNamedSuccess: `mora prep <name>` resolves the attendee filter and
// preps the meeting WITH that person (the name → filter happy path).
func TestMt_CmdPrepNamedSuccess(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, personMemNamed("e1", "gmail", "riya@a.com", "Riya Karode", now.Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, eventMemFull("evt", "Acme sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya Karode"}, "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"prep", "--at", now.Format(time.RFC3339), "Riya Karode"}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("named prep failed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Acme sync") || !strings.Contains(out.String(), "Riya Karode") {
		t.Fatalf("named prep missing meeting/attendee:\n%s", out.String())
	}
}

// TestMt_CmdPrepBuildError: with an event present but a broken index, buildMeetingPrep
// fails and cmdPrep surfaces the humanized error (covers the buildMeetingPrep error
// return AND the ensureIndexDB failure inside it).
func TestMt_CmdPrepBuildError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, eventMemFull("evt", "Sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya"}, "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	mtBreakIndex(t, cfg)
	if err := mtRunPrepErr(t, "--at", now.Format(time.RFC3339)); err == nil {
		t.Fatal("want a build error over a broken index")
	}
}

// ---------------------------------------------------------------------------
// MeetingGaps.lines / printMeetingPrep
// ---------------------------------------------------------------------------

// TestMt_MeetingGapsLinesAll: lines() flattens every gap bucket including the two
// boolean sentinels (NoAttendees / SelfUnknown).
func TestMt_MeetingGapsLinesAll(t *testing.T) {
	g := MeetingGaps{
		UnknownAttendees: []string{"The vault has no record of X."},
		ThinAttendees:    []string{"Only 1 memory about Y — coverage is thin."},
		NoEvidence:       []string{"No recent context with Z."},
		NoAttendees:      true,
		SelfUnknown:      true,
	}
	lines := g.lines()
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"no record of X", "coverage is thin", "No recent context with Z",
		"no other attendees", "Self identity is unknown"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("lines() missing %q:\n%s", want, joined)
		}
	}
}

// TestMt_PrintMeetingPrepNilEvent: the no-meeting print path emits the honest
// header and each gap line.
func TestMt_PrintMeetingPrepNilEvent(t *testing.T) {
	var buf bytes.Buffer
	printMeetingPrep(&buf, MeetingPrepResult{Event: nil, Gaps: MeetingGaps{NoAttendees: true}})
	out := buf.String()
	if !strings.Contains(out, "No upcoming meeting found.") {
		t.Fatalf("missing no-meeting header:\n%s", out)
	}
	if !strings.Contains(out, "no other attendees") {
		t.Fatalf("nil-event print did not emit gap lines:\n%s", out)
	}
}

// TestMt_PrintMeetingPrepFallbackNote: a forgiving-fallback note is printed above
// the meeting header.
func TestMt_PrintMeetingPrepFallbackNote(t *testing.T) {
	var buf bytes.Buffer
	mp := MeetingPrepResult{
		Event:           &MeetingEvent{Title: "Team sync", OccurredAt: "2026-06-14T14:00:00Z", Source: "google"},
		Attendees:       []PrepAttendee{{Display: "Bob"}},
		Evidence:        []ThinkEvidence{{StableID: "m1", Scope: "personal", CreatedAt: "2026-06-10T00:00:00Z", Title: "t", Snippet: "s"}},
		FallbackNote:    "No upcoming meeting with Riya — showing your next meeting instead.",
		SynthesisPrompt: "PROMPT",
	}
	printMeetingPrep(&buf, mp)
	out := buf.String()
	if !strings.Contains(out, "showing your next meeting instead") {
		t.Fatalf("fallback note not printed:\n%s", out)
	}
	if !strings.Contains(out, "Next meeting: Team sync") || !strings.Contains(out, "Attendees: Bob") {
		t.Fatalf("meeting header/attendees not printed:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// buildAliasIndex / evidenceIDsFor — DB error branches (real sqlite)
// ---------------------------------------------------------------------------

// TestMt_BuildAliasIndexQueryError: a closed DB fails the entities query.
func TestMt_BuildAliasIndexQueryError(t *testing.T) {
	if _, err := buildAliasIndex(context.Background(), mtClosedDB(t)); err == nil {
		t.Fatal("buildAliasIndex on a closed db returned nil error")
	}
}

// TestMt_BuildAliasIndexScanError: a mention_count that is non-numeric TEXT fails
// the Scan into the int column.
func TestMt_BuildAliasIndexScanError(t *testing.T) {
	db := mtScratchDB(t)
	if _, err := db.Exec(`CREATE TABLE entities (id TEXT, display_name TEXT, aliases TEXT, mention_count TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO entities VALUES ('person:x@a.com','Xavier','[]','not-an-int')`); err != nil {
		t.Fatal(err)
	}
	if _, err := buildAliasIndex(context.Background(), db); err == nil {
		t.Fatal("buildAliasIndex did not surface the mention_count Scan error")
	}
}

// TestMt_EvidenceIDsForQueryError: a closed DB fails the edges query.
func TestMt_EvidenceIDsForQueryError(t *testing.T) {
	if _, err := evidenceIDsFor(context.Background(), mtClosedDB(t), "person:x"); err == nil {
		t.Fatal("evidenceIDsFor on a closed db returned nil error")
	}
}

// TestMt_EvidenceIDsForScanError: a NULL evidence_id fails the Scan into string.
func TestMt_EvidenceIDsForScanError(t *testing.T) {
	db := mtScratchDB(t)
	if _, err := db.Exec(`CREATE TABLE edges (dst TEXT, evidence_id TEXT, invalidated_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO edges (dst, evidence_id, invalidated_at) VALUES ('person:x', NULL, NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := evidenceIDsFor(context.Background(), db, "person:x"); err == nil {
		t.Fatal("evidenceIDsFor did not surface the NULL evidence_id Scan error")
	}
}

// ---------------------------------------------------------------------------
// buildMeetingPrep — error/empty/branch paths
// ---------------------------------------------------------------------------

// TestMt_BuildMeetingPrepDefaultsLimit: a non-positive per-attendee limit is
// replaced by the default (evidence still surfaces).
func TestMt_BuildMeetingPrepDefaultsLimit(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, personMemNamed("e1", "gmail", "riya@a.com", "Riya Karode", now.Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, eventMemFull("evt", "Sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya Karode"}, "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mp, err := buildMeetingPrep(ctx, cfg, now, "", nil, 0) // limit<=0 → default
	if err != nil {
		t.Fatal(err)
	}
	if mp.Event == nil || len(mp.Evidence) == 0 {
		t.Fatalf("default-limit prep produced no event/evidence: %+v", mp.Event)
	}
}

// TestMt_BuildMeetingPrepAllMemoryFilesError: an unreadable memories subtree makes
// the file scan fail (surfaced, never a silently-shrunk index).
func TestMt_BuildMeetingPrepAllMemoryFilesError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	mtMakeMemoriesUnreadable(t, cfg)
	if _, err := buildMeetingPrep(context.Background(), cfg, time.Now(), "", nil, 8); err == nil {
		t.Fatal("buildMeetingPrep did not surface the unreadable-memories walk error")
	}
}

// TestMt_BuildMeetingPrepSkipsDeleted: a tombstoned memory is skipped in the parse
// loop and never leaks into evidence.
func TestMt_BuildMeetingPrepSkipsDeleted(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, eventMemFull("evt", "Sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya"}, "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	// A deleted person memory for riya — must be skipped (m.DeletedAt != "").
	del := personMemNamed("dead", "gmail", "riya@a.com", "Riya", now.Add(-48*time.Hour))
	del.DeletedAt = now.Add(-24 * time.Hour).Format(time.RFC3339)
	if err := writeMemory(cfg, del); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mp, err := buildMeetingPrep(ctx, cfg, now, "", nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range mp.Evidence {
		if e.StableID == "dead" {
			t.Fatalf("tombstoned memory leaked into evidence: %+v", mp.Evidence)
		}
	}
}

// TestMt_BuildMeetingPrepForgivingFallbackNoName: a filter with no name and no
// matching event falls back to the next meeting, wording the note as "that person".
func TestMt_BuildMeetingPrepForgivingFallbackNoName(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, eventMemFull("team", "Team sync", now.Add(3*time.Hour).Format(time.RFC3339),
		map[string]string{"bob@z.com": "Bob"}, "bob@z.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// name="" but a filter that matches no event → forgiving fallback with "that person".
	mp, err := buildMeetingPrep(ctx, cfg, now, "", map[string]bool{"person:riya@a.com": true}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if mp.Event == nil || mp.Event.StableID != "team" {
		t.Fatalf("event = %+v, want fallback to 'team'", mp.Event)
	}
	if !strings.Contains(mp.FallbackNote, "that person") {
		t.Fatalf("FallbackNote = %q, want the anonymous 'that person' wording", mp.FallbackNote)
	}
}

// TestMt_BuildMeetingPrepUnknownAttendee: an event whose attendee count exceeds the
// person fan-out cap leaves the capped-away attendees with no entity, so they are
// flagged as unknown in the gaps and the prompt.
func TestMt_BuildMeetingPrepUnknownAttendee(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	atts := make([]string, 0, maxParticipantFanout+6)
	for i := 0; i < maxParticipantFanout+6; i++ {
		atts = append(atts, "att"+strconv.Itoa(i)+"@x.com")
	}
	if err := writeMemory(cfg, eventMem("big", "Big all-hands", now.Add(2*time.Hour).Format(time.RFC3339), atts...)); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mp, err := buildMeetingPrep(ctx, cfg, now, "", nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(mp.Gaps.UnknownAttendees) == 0 {
		t.Fatalf("expected some capped-away attendees to be unknown, got none (attendees=%d)", len(mp.Attendees))
	}
	if !strings.Contains(mp.SynthesisPrompt, "no record of") {
		t.Fatalf("prompt missing unknown-attendee gap line:\n%s", mp.SynthesisPrompt)
	}
}

// TestMt_BuildMeetingPrepEvidenceTieBreak: two evidence memories with identical
// timestamps exercise the deterministic StableID tie-break in the recency sort;
// both survive.
func TestMt_BuildMeetingPrepEvidenceTieBreak(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	same := now.Add(-48 * time.Hour)
	if err := writeMemory(cfg, personMemNamed("ev-aaa", "gmail", "riya@a.com", "Riya Karode", same)); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, personMemNamed("ev-bbb", "gmail", "riya@a.com", "Riya Karode", same)); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, eventMemFull("evt", "Sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya Karode"}, "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mp, err := buildMeetingPrep(ctx, cfg, now, "", nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, e := range mp.Evidence {
		ids[e.StableID] = true
	}
	if !ids["ev-aaa"] || !ids["ev-bbb"] {
		t.Fatalf("both equal-timestamp evidence memories should survive: %v", ids)
	}
}

// TestMt_BuildMeetingPrepPastSameSeriesExcluded: a PAST recurring sibling of the
// selected meeting's own series is excluded from evidence (it's not context for
// itself), while genuine past correspondence survives.
func TestMt_BuildMeetingPrepPastSameSeriesExcluded(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, personMemNamed("past-email", "gmail", "riya@a.com", "Riya", now.Add(-24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, eventSeriesMem("evt-now", "Standup", now.Add(2*time.Hour), "series-abc", "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	// A PAST sibling of the SAME series (not future) — hits the same-series continue.
	if err := writeMemory(cfg, eventSeriesMem("evt-past", "Standup", now.Add(-48*time.Hour), "series-abc", "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mp, err := buildMeetingPrep(ctx, cfg, now, "", nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	if mp.Event == nil || mp.Event.StableID != "evt-now" {
		t.Fatalf("selected %+v, want evt-now", mp.Event)
	}
	ids := map[string]bool{}
	for _, e := range mp.Evidence {
		ids[e.StableID] = true
	}
	if ids["evt-past"] {
		t.Errorf("PAST same-series sibling leaked into evidence: %v", ids)
	}
	if !ids["past-email"] {
		t.Errorf("genuine past correspondence missing from evidence: %v", ids)
	}
}

// TestMt_BuildMeetingPrepOpenLoopsError: a live-tasks.md that is a DIRECTORY makes
// the open-loops join fail, and buildMeetingPrep surfaces that error.
func TestMt_BuildMeetingPrepOpenLoopsError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if err := writeMemory(cfg, personMemNamed("e1", "gmail", "riya@a.com", "Riya Karode", now.Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, eventMemFull("evt", "Sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya Karode"}, "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(cfg.VaultDir, "live-tasks.md")
	_ = os.Remove(live)
	if err := os.Mkdir(live, 0o755); err != nil { // live-tasks.md is now a directory
		t.Fatalf("mkdir live-tasks.md-as-dir: %v", err)
	}
	if _, err := buildMeetingPrep(ctx, cfg, now, "", nil, 8); err == nil {
		t.Fatal("buildMeetingPrep did not surface the open-loops listTasks error")
	}
}

// ---------------------------------------------------------------------------
// meetingPrepPrompt — full gap rendering
// ---------------------------------------------------------------------------

// TestMt_MeetingPrepPromptAllGaps: every gap bucket (including the two boolean
// sentinels) is rendered into the KNOWN GAPS section.
func TestMt_MeetingPrepPromptAllGaps(t *testing.T) {
	ev := MeetingEvent{Title: "Sync", OccurredAt: "2026-06-14T14:00:00Z", Source: "google"}
	gaps := MeetingGaps{
		UnknownAttendees: []string{"The vault has no record of Ada."},
		ThinAttendees:    []string{"Only 1 memory about Bo — coverage is thin."},
		NoEvidence:       []string{"No recent context with Cy."},
		NoAttendees:      true,
		SelfUnknown:      true,
	}
	prompt := meetingPrepPrompt(ev, []PrepAttendee{{Display: "Ada"}}, nil, gaps, nil)
	for _, want := range []string{"no record of Ada", "coverage is thin", "No recent context with Cy",
		"no other attendees", "Self identity is unknown"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing gap %q:\n%s", want, prompt)
		}
	}
}

// ---------------------------------------------------------------------------
// eventStart / selectNextEvent — parse + tie-break branches
// ---------------------------------------------------------------------------

// TestMt_EventStartNoParseableCandidate: an event with no occurred_at and an empty
// CreatedAt yields ok=false (both the empty-candidate continue and the final
// no-parse return).
func TestMt_EventStartNoParseableCandidate(t *testing.T) {
	if ts, ok := eventStart(Memory{}); ok {
		t.Fatalf("eventStart(empty) ok=true (%v), want false", ts)
	}
	// occurred_at present but unparseable, CreatedAt empty → still false.
	if _, ok := eventStart(Memory{Meta: map[string]any{"occurred_at": "nope"}}); ok {
		t.Fatal("eventStart with an unparseable occurred_at and no CreatedAt should be false")
	}
}

// TestMt_SelectNextEventSkipsUnparseable: an event-typed memory whose timestamps do
// not parse is skipped; a valid event is still selected.
func TestMt_SelectNextEventSkipsUnparseable(t *testing.T) {
	mems := []Memory{
		{ID: "bad", Type: "event", Meta: map[string]any{"occurred_at": "not-a-time"}, CreatedAt: "also-bad"},
		eventMem("good", "Good", "2026-06-14T14:00:00Z"),
	}
	ev := selectNextEvent(mems, mpNow, nil)
	if ev == nil || ev.StableID != "good" {
		t.Fatalf("got %+v, want 'good' (unparseable 'bad' skipped)", ev)
	}
}

// TestMt_SelectNextEventCurrentTieBreak: two in-progress events with identical
// starts break the tie deterministically on the lowest StableID, regardless of
// input order.
func TestMt_SelectNextEventCurrentTieBreak(t *testing.T) {
	a := eventMem("aaa", "A", "2026-06-14T11:45:00Z") // 15m ago, within grace
	b := eventMem("bbb", "B", "2026-06-14T11:45:00Z") // same start
	ev1 := selectNextEvent([]Memory{a, b}, mpNow, nil)
	ev2 := selectNextEvent([]Memory{b, a}, mpNow, nil) // reversed input
	if ev1 == nil || ev1.StableID != "aaa" || ev2 == nil || ev2.StableID != "aaa" {
		t.Fatalf("current-event tie-break must be lowest StableID 'aaa': %+v / %+v", ev1, ev2)
	}
}
