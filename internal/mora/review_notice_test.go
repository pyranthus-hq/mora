package mora

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

func TestReviewNoticeText(t *testing.T) {
	cases := []struct {
		name        string
		saved, held int
		err         error
		want        string
	}{
		{"none", 0, 0, nil, ""},
		{"saved only", 2, 0, nil, `Agent notes not reviewed: 2 saved, 0 held. The owner can run "mora review".`},
		{"held only", 0, 3, nil, `Agent notes not reviewed: 0 saved, 3 held. The owner can run "mora review".`},
		{"count error", 4, 4, os.ErrPermission, ""},
	}
	for _, c := range cases {
		if got := reviewNoticeText(c.saved, c.held, c.err); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// noticeSurfaces reads the notice from every place it appears. Every surface
// is always present in the map ("" when it shows no notice), so a test that
// looks a surface up by name fails when that surface loses its notice.
func noticeSurfaces(t *testing.T, cfg Config) map[string]string {
	t.Helper()
	out := map[string]string{}
	pick := func(name, text string) {
		out[name] = ""
		for _, l := range strings.Split(text, "\n") {
			if i := strings.Index(l, "Agent notes not reviewed:"); i >= 0 {
				out[name] = strings.TrimSpace(l[i:])
			}
		}
	}
	pick("brief", runBrief(t))

	var bj struct {
		ReviewNotice string `json:"review_notice"`
	}
	if err := json.Unmarshal([]byte(runBrief(t, "--json")), &bj); err != nil {
		t.Fatal(err)
	}
	out["brief json"] = bj.ReviewNotice

	b, _ := json.Marshal(mustBrief(t, cfg))
	var mj struct {
		ReviewNotice string `json:"review_notice"`
	}
	_ = json.Unmarshal(b, &mj)
	out["mcp brief"] = mj.ReviewNotice

	enableUnboundSessionBrief(t)
	restore := stubHookBrief(t, "today's local brief")
	defer restore()
	var hook bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"session-start"}, &hook, testStderr, strings.NewReader(`{}`)); err != nil {
		t.Fatal(err)
	}
	pick("hook", decodeHookOutput(t, hook.String()).HookSpecificOutput.AdditionalContext)

	dj, _ := runErr(t, "doctor", "--json")
	var d struct {
		Tentative int `json:"tentative_memories"`
		Pending   int `json:"pending_mcp_proposals"`
	}
	if i := strings.Index(dj, "{"); i >= 0 {
		_ = json.Unmarshal([]byte(dj[i:]), &d)
	}
	out["doctor json"] = fmt.Sprintf("%d,%d", d.Tentative, d.Pending)
	dt, _ := runErr(t, "doctor")
	pick("doctor", dt)
	return out
}

func TestReviewNoticeLifecycle(t *testing.T) {
	withTestVerifiedOwnerAction(t)
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	surfaces := []string{"brief", "brief json", "mcp brief", "hook", "doctor", "doctor json"}
	check := func(stage, notice, counts string) {
		t.Helper()
		got := noticeSurfaces(t, cfg)
		for _, name := range surfaces {
			v, ok := got[name]
			if !ok {
				t.Fatalf("%s: surface %q not read", stage, name)
			}
			want := notice
			if name == "doctor json" {
				want = counts
			}
			if v != want {
				t.Fatalf("%s: %s = %q, want %q", stage, name, v, want)
			}
		}
		if b, _ := json.Marshal(mustBrief(t, cfg)); strings.Contains(string(b), "review_notice") != (notice != "") {
			t.Fatalf("%s: mcp brief review_notice field presence wrong", stage)
		}
	}
	check("empty", "", "0,0")

	saved := tentativeServe(t, map[string]any{"title": "Zebrafish owner", "text": "The zebrafish module belongs to the indexer team.", "type": "fact", "scope": "project:demo"})
	id := tentativeMemoryID(t, saved)
	if want := `Agent notes not reviewed: 1 saved, 0 held. The owner can run "mora review".`; saved["review_notice"] != want {
		t.Fatalf("saved write notice = %v, want %q", saved["review_notice"], want)
	}
	held := tentativeServe(t, map[string]any{"title": "Passport copy", "text": "The passport scan is in the shared drive.", "type": "fact", "scope": "project:demo"})
	want := `Agent notes not reviewed: 1 saved, 1 held. The owner can run "mora review".`
	if held["review_notice"] != want {
		t.Fatalf("held write notice = %v, want %q", held["review_notice"], want)
	}
	check("one saved one held", want, "1,1")

	// Held-only: the doctor line is still the shared notice.
	run(t, "confirm", id)
	check("held only", `Agent notes not reviewed: 0 saved, 1 held. The owner can run "mora review".`, "0,1")

	ps, err := listMCPWriteProposals(cfg)
	if err != nil || len(ps) != 1 {
		t.Fatalf("proposals = %v, %v", ps, err)
	}
	run(t, "mcp", "proposals", "reject", ps[0].ID)
	check("after review", "", "0,0")
}

func mustBrief(t *testing.T, cfg Config) any {
	t.Helper()
	raw, err := mcpBrief(testCtx(t), cfg, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A count failure must not fail the write or the brief.
func TestReviewNoticeCountFailureNeverFailsCaller(t *testing.T) {
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	// A file where the proposals directory belongs makes the held count fail.
	dir := mcppkg.ProposalDir(cfg)
	_ = os.RemoveAll(dir)
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := unreviewedAgentNoteCounts(testCtx(t), cfg); err == nil {
		t.Skip("proposals path did not make the count fail on this platform")
	}
	res := tentativeServe(t, map[string]any{"title": "Zebrafish owner", "text": "The zebrafish module belongs to the indexer team.", "type": "fact", "scope": "project:demo"})
	tentativeMemoryID(t, res)
	if _, ok := res["review_notice"]; ok {
		t.Fatalf("notice present despite a count error: %v", res["review_notice"])
	}
	if raw, _ := json.Marshal(mustBrief(t, cfg)); strings.Contains(string(raw), "review_notice") {
		t.Fatalf("brief carries a notice despite a count error")
	}
	runBrief(t)
}

// A brief at the size limit still fits with a non-zero notice: its bytes come
// out of the item budget, for the plain and the envelope shape.
func TestReviewNoticeBriefStaysWithinBudget(t *testing.T) {
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now()
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	for i := 0; i < 400; i++ {
		digestSeedUnindexed(t, cfg, "gmail", "Item"+string(rune('0'+i%10))+string(rune('A'+i/26%26))+string(rune('a'+i%26)), time.Duration(i+1)*time.Minute, now)
	}
	rebuildDigestIndex(t, cfg)
	tentativeMemoryID(t, tentativeServe(t, map[string]any{"title": "Zebrafish owner", "text": "The zebrafish module belongs to the indexer team.", "type": "fact", "scope": "project:demo"}))
	tentativeServe(t, map[string]any{"title": "Passport copy", "text": "The passport scan is in the shared drive.", "type": "fact", "scope": "project:demo"})

	for _, args := range []string{`{"max_tokens":20000}`, `{"max_tokens":20000,"envelope":true}`} {
		p := briefMCPStructured(t, args)
		b, _ := json.Marshal(p)
		if !strings.Contains(string(b), "review_notice") {
			t.Fatalf("%s: brief lacks the notice", args)
		}
		if len(b) > maxContextTokens*charsPerToken {
			t.Fatalf("%s: payload %d B exceeds the ceiling %d B", args, len(b), maxContextTokens*charsPerToken)
		}
	}
}

// The saved count matches memory.IsTentative: exact, case-sensitive tags.
func TestReviewNoticeTagMatch(t *testing.T) {
	cases := []struct {
		tags string
		want int
	}{
		{"mora-tentative", 1},
		{"mora-tentative,other", 1},
		{"a,mora-tentative,b", 1},
		{"a,mora-tentative", 1},
		{"not-mora-tentative", 0},
		{"mora-tentative-2", 0},
		{"MORA-TENTATIVE", 0},
		{"", 0},
	}
	for _, c := range cases {
		subRun(t, c.tags, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			legacyOpenPolicy(t)
			cfg := mustConfig(t)
			run(t, "write", "--title", "T", "--text", "body", "--scope", "project:demo", "--tags", c.tags)
			saved, held, err := unreviewedAgentNoteCounts(testCtx(t), cfg)
			if err != nil || saved != c.want || held != 0 {
				t.Fatalf("tags %q: saved=%d held=%d err=%v, want saved=%d", c.tags, saved, held, err, c.want)
			}
		})
	}
}

// A write whose index update failed gets no notice: the count may be wrong.
// The result still carries the existing stale signal.
func TestReviewNoticeSilentWhenIndexUpdateFails(t *testing.T) {
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	poisonInserts(t, cfg)
	res := tentativeServe(t, map[string]any{"title": "Zebrafish owner", "text": "The zebrafish module belongs to the indexer team.", "type": "fact", "scope": "project:demo"})
	if _, ok := res["review_notice"]; ok {
		t.Fatalf("notice shown with an unsettled index: %v", res["review_notice"])
	}
	if stale, _ := res["index_stale"].(bool); !stale || res["health"] == nil {
		t.Fatalf("existing stale signal lost: %v", res)
	}
}

// A locked index must not hold a held write: the notice is dropped within its
// budget instead of waiting out the 15 s busy timeout.
// (shortcut: the write's own health read can still wait on that lock.)
func TestReviewNoticeDroppedFastOnLockedIndex(t *testing.T) {
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	tentativeMemoryID(t, tentativeServe(t, map[string]any{"title": "Zebrafish owner", "text": "The zebrafish module belongs to the indexer team.", "type": "fact", "scope": "project:demo"}))
	waitForAuthoredReconciles(30 * time.Second)

	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA locking_mode=EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE memories SET title = title`); err != nil {
		t.Fatal(err)
	}
	// The held and saved write paths call reviewNotice; the rest of a write
	// under this lock (the health read) predates the notice and has its own
	// timeout, so the bound is asserted on the notice itself.
	start := time.Now()
	notice := reviewNotice(testCtx(t), cfg)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("notice took %v with a locked index, budget is %v", d, reviewNoticeBudget)
	}
	if notice != "" {
		if _, _, cerr := unreviewedAgentNoteCounts(testCtx(t), cfg); cerr == nil {
			t.Skip("this SQLite build lets readers through an exclusive lock")
		}
		t.Fatalf("notice shown despite the lock: %q", notice)
	}
}
