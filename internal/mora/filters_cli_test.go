package mora

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func personMemNamed(id, provider, from, name string, created time.Time) Memory {
	m := personMem(id, provider, from, created)
	m.Meta["names"] = map[string]string{from: name}
	return m
}

func runBriefErr(t *testing.T, args ...string) error {
	var out bytes.Buffer
	full := append([]string{"brief"}, args...)
	return Run(testCtx(t), full, &out, &out, strings.NewReader(""))
}

// TestCmdPulseDigestEntityFilter: `mora pulse --digest --since-hours N --entity <addr>`
// threads the entity filter (window path). cmdPulse uses time.Now() (no clock seam),
// so the memories are seeded relative to real time.
func TestCmdPulseDigestEntityFilter(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	recent := time.Now().Add(-2 * time.Hour)
	if err := writeMemory(cfg, personMem("riya-p", "gmail", "riya@a.com", recent)); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, personMem("bob-p", "gmail", "bob@z.com", recent)); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	out := runPulse(t, "--digest", "--since-hours", "24", "--entity", "riya@a.com")
	if !strings.Contains(out, "riya-p") || strings.Contains(out, "bob-p") {
		t.Fatalf("pulse entity filter wrong:\n%s", out)
	}
}

// TestCmdBriefEntityFilter: `mora brief --entity <addr>` surfaces only that
// person's items (resolved through the graph, then threaded into resolveBrief).
func TestCmdBriefEntityFilter(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	pinBriefClock(t)
	now := briefFixedNow
	if err := writeMemory(cfg, personMem("riya-call", "gmail", "riya@a.com", now.Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, personMem("bob-call", "gmail", "bob@z.com", now.Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil { // entity resolution reads the index
		t.Fatal(err)
	}
	out := runBrief(t, "--entity", "riya@a.com")
	if !strings.Contains(out, "riya-call") || strings.Contains(out, "bob-call") {
		t.Fatalf("entity-filtered brief wrong:\n%s", out)
	}
}

// TestCmdBriefEntityNoMatch: an unknown entity exits non-zero (never a silently
// empty brief that reads as "nothing's up").
func TestCmdBriefEntityNoMatch(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	pinBriefClock(t)
	if err := writeMemory(cfg, personMem("riya-call", "gmail", "riya@a.com", briefFixedNow.Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	err := runBriefErr(t, "--entity", "ghost@nowhere.com")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "no entity") {
		t.Fatalf("no-match: err=%v, want a non-nil 'no entity' error", err)
	}
}

// TestCmdBriefEntityAmbiguous: a name matching two people exits non-zero with
// disambiguation guidance.
func TestCmdBriefEntityAmbiguous(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	pinBriefClock(t)
	now := briefFixedNow
	if err := writeMemory(cfg, personMemNamed("r1", "gmail", "riya.k@alpha.com", "Riya", now.Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, personMemNamed("r2", "gmail", "riya.s@beta.com", "Riya", now.Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	err := runBriefErr(t, "--entity", "Riya")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "ambiguous") {
		t.Fatalf("ambiguous: err=%v, want a non-nil 'ambiguous' error", err)
	}
}

// TestCmdBriefNegativeSinceDaysClampedNotEmpty (P1-D): `--since-days -7` is clamped
// to 0 (all-time), NOT a future cutoff that empties the brief.
func TestCmdBriefNegativeSinceDaysClampedNotEmpty(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	pinBriefClock(t)
	now := briefFixedNow
	if err := writeMemory(cfg, personMem("riya-call", "gmail", "riya@a.com", now.Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	out := runBrief(t, "--since-days", "-7")
	if !strings.Contains(out, "riya-call") {
		t.Fatalf("negative since-days should be a no-op (all-time), got empty:\n%s", out)
	}
}
