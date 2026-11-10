package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSyncFailureIsDurableAndCannotReportSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	root := t.TempDir()
	t.Setenv("MORA_TENTACLES_STATE_DIR", root)
	bin := filepath.Join(t.TempDir(), "mora")
	script := `#!/bin/sh
if [ "$2" = list ]; then
 printf '%s\n' '{"payload":{"publishes":[{"name":"devbox-test"}],"subscriptions":[{"name":"from-devbox-test"}]}}'
 exit 0
fi
if [ "$2" = push ]; then exit 42; fi
exit 0
`
	if e := os.WriteFile(bin, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("MORA_BIN", bin)
	previous := "2026-11-01T00:00:00Z"
	path := filepath.Join(root, "mac-devbox.json")
	if e := atomicJSON(path, Receipt{Schema: 1, LastSuccess: previous}); e != nil {
		t.Fatal(e)
	}
	if e := syncShares("mac-devbox"); e == nil {
		t.Fatal("failed push swallowed")
	}
	b, _ := os.ReadFile(path)
	var r Receipt
	_ = json.Unmarshal(b, &r)
	if r.State != "failed" || r.Operations != 1 || r.LastSuccess != previous || !strings.Contains(r.Detail, "push devbox-test") {
		t.Fatalf("bad receipt: %+v", r)
	}
	if c := readReceipt(path, "test", time.Hour); c.State != "failed" {
		t.Fatalf("bad channel: %+v", c)
	}

	script = strings.ReplaceAll(script, "exit 42", "exit 0")
	_ = os.WriteFile(bin, []byte(script), 0700)
	if e := syncShares("mac-devbox"); e != nil {
		t.Fatal(e)
	}
	if c := readReceipt(path, "test", time.Hour); c.State != "healthy" {
		t.Fatalf("successful run not confirmed: %+v", c)
	}
}
func TestEmptyWrongAndUnsafeShareListsFailClosed(t *testing.T) {
	for _, s := range []string{`{}`, `{"publishes":[],"subscriptions":[]}`, `{"publishes":[{"name":"gw-test"}]}`, `{"publishes":[{"name":"devbox-;evil"}],"subscriptions":[{"name":"from-devbox-test"}]}`, `{"publishes":{},"subscriptions":[]}`} {
		if _, e := planOperations("mac-devbox", []byte(s)); e == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}
func TestReceiptAgesAndInterruptedRun(t *testing.T) {
	oldClock := clock
	defer func() { clock = oldClock }()
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	clock = func() time.Time { return now }
	path := filepath.Join(t.TempDir(), "test.json")
	for _, tt := range []struct {
		r    Receipt
		want string
	}{
		{Receipt{Schema: 1, State: "running", StartedAt: now.Add(-time.Hour).Format(time.RFC3339)}, "failed"},
		{Receipt{Schema: 1, State: "running", StartedAt: now.Format(time.RFC3339)}, "running"},
		{Receipt{Schema: 1, State: "succeeded", Operations: 1, LastSuccess: "2026-11-08T07:00:00Z", FinishedAt: "2026-11-08T07:00:00Z"}, "stale"},
		{Receipt{Schema: 1, State: "succeeded", Operations: 1, LastSuccess: "2026-11-09T07:00:00Z", FinishedAt: "2026-11-09T07:00:00Z"}, "unknown"},
		{Receipt{Schema: 1, State: "succeeded", Operations: 0}, "unknown"},
	} {
		_ = atomicJSON(path, tt.r)
		if c := readReceipt(path, "test", time.Hour); c.State != tt.want {
			t.Fatalf("%+v: got %+v want %s", tt.r, c, tt.want)
		}
	}
	_ = os.WriteFile(path, []byte("broken"), 0600)
	if c := readReceipt(path, "test", time.Hour); c.State != "unknown" {
		t.Fatal(c)
	}
}
func TestSSHConfigRejectsOptionsAndInjection(t *testing.T) {
	for _, s := range []string{"-oProxyCommand=evil", "host;echo bad", "host\nname", "$(evil)"} {
		if e := validateConfig(Config{Peers: []Peer{{ID: "test", Name: "Test", SSH: s, Profile: "devbox"}}}); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
	if e := validateConfig(Config{Peers: []Peer{{ID: "windows", Name: "Windows", SSH: "white-nova-linux", HostName: "100.127.143.81", HostKeyAlias: "[white-nova]:2222", Profile: "windows"}}}); e != nil {
		t.Fatal(e)
	}
}
func TestCommandOutputBoundAndTimeout(t *testing.T) {
	var b bounded
	_, _ = b.Write(make([]byte, 2*1024*1024))
	if !b.overflow || b.Len() != 1024*1024 {
		t.Fatal("output not bounded")
	}
	if runtime.GOOS == "windows" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, e := command(ctx, "/bin/sleep", "5"); e == nil {
		t.Fatal("deadline ignored")
	}
}

func TestSourceHealthAcceptsUnhealthyDoctorExit2AndPreservesUnknownState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	bin := filepath.Join(t.TempDir(), "mora")
	output := `{"schema":"mora.doctor.report","schema_version":1,"sources":[{"key":"gmail:work","state":"failed","last_success_at":"2026-11-08T07:12:00Z","last_error":"PRIVATE_FOREIGN_ACCOUNT /private/other-project","error_code":"connector.unavailable"},{"key":"calendar","state":"paused-by-policy"}],"index":{"state":"dirty","indexed_at":"2026-11-08T06:54:30Z","pending_ops":46}}`
	script := "#!/bin/sh\nprintf '%s\\n' '" + output + "'\nexit 2\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORA_BIN", bin)
	sources, detail, index, _, _ := sourceHealth()
	if detail != "" || len(sources) != 2 {
		t.Fatalf("doctor unhealthy report was rejected: sources=%+v detail=%q", sources, detail)
	}
	if sources[0].State != "failed" || sources[0].LastSuccessAt != "2026-11-08T07:12:00Z" || sources[0].Detail != "connector.unavailable" {
		t.Fatalf("source details lost: %+v", sources[0])
	}
	if strings.Contains(sources[0].Detail, "PRIVATE_FOREIGN_ACCOUNT") {
		t.Fatal("private source error prose leaked into the compact status snapshot")
	}
	if sources[1].State != "paused-by-policy" {
		t.Fatalf("unknown state was normalized or treated as healthy: %+v", sources[1])
	}
	if index.State != "stale" || index.LastSuccess != "2026-11-08T06:54:30Z" || !strings.Contains(index.Detail, "46 operations") {
		t.Fatalf("index status was lost or overclaimed: %+v", index)
	}
}

func TestSourceHealthMalformedAndUnavailableAreExplicit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	bin := filepath.Join(t.TempDir(), "mora")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '{broken'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORA_BIN", bin)
	if sources, detail, index, _, _ := sourceHealth(); len(sources) != 0 || detail == "" || index.State != "unknown" {
		t.Fatalf("malformed response was not marked unavailable: %+v, %q", sources, detail)
	}
	t.Setenv("MORA_BIN", filepath.Join(t.TempDir(), "missing"))
	if sources, detail, index, _, _ := sourceHealth(); len(sources) != 0 || detail == "" || index.State != "unknown" {
		t.Fatalf("missing helper was not marked unavailable: %+v, %q", sources, detail)
	}
}

func TestSourceHealthReviewCounts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	const head = `{"schema":"mora.doctor.report","schema_version":1,"sources":[]`
	cases := []struct {
		name, output string
		want         *Review
	}{
		{"both present", head + `,"tentative_memories":3,"pending_mcp_proposals":2}`, &Review{3, 2}},
		{"both absent", head + `}`, &Review{0, 0}},
		{"one present", head + `,"pending_mcp_proposals":4}`, &Review{0, 4}},
		{"invalid report", `{broken`, nil},
		{"wrong schema", `{"schema":"other","schema_version":1,"sources":[],"tentative_memories":3}`, nil},
	}
	for _, c := range cases {
		bin := filepath.Join(t.TempDir(), "mora")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '"+c.output+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MORA_BIN", bin)
		_, _, _, got, _ := sourceHealth()
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Fatalf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
	t.Setenv("MORA_BIN", filepath.Join(t.TempDir(), "missing"))
	if _, _, _, got, _ := sourceHealth(); got != nil {
		t.Fatalf("unavailable doctor must omit review: %+v", got)
	}
}

func TestSourceHealthInvalidReviewCountsKeepHealth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	run := func(output string) ([]SourceHealth, string, Channel, *Review, *Given) {
		bin := filepath.Join(t.TempDir(), "mora")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '"+output+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MORA_BIN", bin)
		return sourceHealth()
	}
	const head = `{"schema":"mora.doctor.report","schema_version":1,"sources":[{"key":"gmail","state":"fresh","last_success_at":"2026-11-08T07:45:00Z"}],"index":{"state":"fresh","indexed_at":"2026-11-08T08:54:30Z"}`
	wantSources, wantDetail, wantIndex, _, _ := run(head + `}`)
	if wantDetail != "" || wantIndex.State != "healthy" {
		t.Fatalf("baseline report not healthy: %q %+v", wantDetail, wantIndex)
	}
	for name, field := range map[string]string{
		"string":         `"tentative_memories":"3"`,
		"fractional":     `"tentative_memories":1.5`,
		"overflow":       `"pending_mcp_proposals":99999999999999999999`,
		"negative":       `"pending_mcp_proposals":-1`,
		"negative saved": `"tentative_memories":-2,"pending_mcp_proposals":1`,
	} {
		sources, detail, index, review, _ := run(head + "," + field + "}")
		if review != nil {
			t.Fatalf("%s: review must be omitted, got %+v", name, review)
		}
		if detail != wantDetail || index != wantIndex || len(sources) != len(wantSources) || sources[0] != wantSources[0] {
			t.Fatalf("%s: health changed: %+v %q %+v", name, sources, detail, index)
		}
	}
}

func TestIndexChannelUsesFreshnessWithoutOverclaimingDirtySnapshots(t *testing.T) {
	fresh := indexChannel(&doctorIndex{State: "fresh", IndexedAt: "2026-11-08T08:54:30Z"})
	if fresh.State != "healthy" || fresh.LastSuccess != "2026-11-08T08:54:30Z" {
		t.Fatalf("valid fresh index not represented: %+v", fresh)
	}
	malformed := indexChannel(&doctorIndex{State: "fresh", IndexedAt: "not-a-time"})
	if malformed.State != "unknown" {
		t.Fatalf("fresh index with malformed timestamp overclaims health: %+v", malformed)
	}
	dirty := indexChannel(&doctorIndex{State: "dirty", IndexedAt: "2026-11-08T08:54:30Z", PendingOps: 46})
	if dirty.State != "stale" || !strings.Contains(dirty.Detail, "last committed snapshot") || !strings.Contains(dirty.Detail, "46 operations") {
		t.Fatalf("dirty index is not visibly stale: %+v", dirty)
	}
	for _, index := range []*doctorIndex{{State: "failed"}, {State: "fresh", Blocked: true}} {
		if got := indexChannel(index); got.State != "failed" {
			t.Fatalf("failed or blocked index not reported as failed: %+v", got)
		}
	}
	if got := indexChannel(nil); got.State != "unknown" {
		t.Fatalf("missing index not unknown: %+v", got)
	}
	if got := indexChannel(&doctorIndex{State: "refreshing"}); got.State != "unknown" {
		t.Fatalf("unrecognized index state was normalized: %+v", got)
	}
}

func TestBotReturnReceiptReportsBoundedProposalIntakeHealth(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MORA_TENTACLES_STATE_DIR", root)
	oldClock := clock
	defer func() { clock = oldClock }()
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	clock = func() time.Time { return now }
	path := filepath.Join(root, "bot-return.json")
	if got := botReturnChannel(); got.State != "unknown" {
		t.Fatalf("missing receipt: %+v", got)
	}
	for _, tc := range []struct {
		name    string
		receipt Receipt
		state   string
	}{
		{"fresh", Receipt{Schema: 1, Job: "bot-return", State: "succeeded", StartedAt: now.Format(time.RFC3339), FinishedAt: now.Format(time.RFC3339), LastSuccess: now.Format(time.RFC3339), Operations: 1}, "healthy"},
		{"stale", Receipt{Schema: 1, Job: "bot-return", State: "succeeded", StartedAt: now.Add(-20 * time.Minute).Format(time.RFC3339), FinishedAt: now.Add(-16 * time.Minute).Format(time.RFC3339), LastSuccess: now.Add(-16 * time.Minute).Format(time.RFC3339), Operations: 1}, "stale"},
		{"failed", Receipt{Schema: 1, Job: "bot-return", State: "failed", StartedAt: now.Format(time.RFC3339), FinishedAt: now.Format(time.RFC3339), Operations: 1, Detail: "intake failed"}, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := atomicJSON(path, tc.receipt); err != nil {
				t.Fatal(err)
			}
			got := botReturnChannel()
			if got.State != tc.state {
				t.Fatalf("got %+v, want %s", got, tc.state)
			}
			if tc.state == "healthy" && !strings.Contains(got.Detail, "await owner approval") {
				t.Fatalf("success overclaims publication: %+v", got)
			}
		})
	}
	if err := atomicJSON(path, Receipt{Schema: 1, Job: "mac-devbox", State: "succeeded", StartedAt: now.Format(time.RFC3339), FinishedAt: now.Format(time.RFC3339), LastSuccess: now.Format(time.RFC3339), Operations: 1}); err != nil {
		t.Fatal(err)
	}
	if got := botReturnChannel(); got.State != "unknown" {
		t.Fatalf("wrong job receipt accepted: %+v", got)
	}
}

func TestAggregatedWindowsDropsUnverifiedReturnButStandaloneKeepsIt(t *testing.T) {
	channels := []Channel{{Name: "Personal archive", State: "healthy"}, {Name: "Windows → Mac", State: "unknown"}}
	if got := aggregateRemoteChannels("windows", append([]Channel(nil), channels...)); len(got) != 1 || got[0].Name != "Personal archive" {
		t.Fatalf("aggregate retained the unverified return channel: %+v", got)
	}
	if got := aggregateRemoteChannels("devbox", channels); len(got) != 2 {
		t.Fatalf("standalone/non-Windows status was changed: %+v", got)
	}
	if got := aggregateRemoteDetail("windows", "WSL backend · archive and gateway are separate"); got != "Personal archive pull + scoped gateway; full Windows-origin return not configured." {
		t.Fatalf("Windows detail overstates return coverage: %q", got)
	}
}

func TestJobLockRecoversAfterProcessDeath(t *testing.T) {
	if path := os.Getenv("MORA_LOCK_TEST_PATH"); path != "" {
		release, err := acquireJobLock(path)
		if err != nil {
			os.Exit(2)
		}
		defer release()
		_ = os.WriteFile(path+".ready", []byte("ready"), 0600)
		time.Sleep(30 * time.Second)
		return
	}
	path := filepath.Join(t.TempDir(), "job.lock")
	child := exec.Command(os.Args[0], "-test.run=^TestJobLockRecoversAfterProcessDeath$")
	child.Env = append(os.Environ(), "MORA_LOCK_TEST_PATH="+path)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	until := time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(path + ".ready"); e == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("child not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r, e := acquireJobLock(path); e == nil {
		r()
		t.Fatal("concurrent lock acquired")
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	r, e := acquireJobLock(path)
	if e != nil {
		t.Fatal("dead process wedged lock", e)
	}
	r()
}

func TestSourceHealthLastGiven(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	const head = `{"schema":"mora.doctor.report","schema_version":1,"sources":[{"key":"gmail","state":"fresh","last_success_at":"2026-11-08T07:45:00Z"}],"index":{"state":"fresh","indexed_at":"2026-11-08T08:54:30Z"},"tentative_memories":3`
	run := func(output string) ([]SourceHealth, string, Channel, *Review, *Given) {
		bin := filepath.Join(t.TempDir(), "mora")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '"+output+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MORA_BIN", bin)
		return sourceHealth()
	}
	three := 3
	cases := []struct {
		name, field string
		want        *Given
	}{
		{"valid with count", `"last_given":{"at":"2026-11-09T14:02:00Z","surface":"hook-session-start","count":3}`, &Given{"2026-11-09T14:02:00Z", "hook-session-start", &three}},
		{"valid without count", `"last_given":{"at":"2026-11-09T14:02:00Z","surface":"mcp:search_memory"}`, &Given{"2026-11-09T14:02:00Z", "mcp:search_memory", nil}},
		{"absent", `"x":1`, nil},
		{"bad at", `"last_given":{"at":"yesterday","surface":"hook-recall","count":1}`, nil},
		{"upper case surface", `"last_given":{"at":"2026-11-09T14:02:00Z","surface":"Hook-Recall"}`, nil},
		{"space in surface", `"last_given":{"at":"2026-11-09T14:02:00Z","surface":"hook recall"}`, nil},
		{"long surface", `"last_given":{"at":"2026-11-09T14:02:00Z","surface":"` + strings.Repeat("a", 200) + `"}`, nil},
		{"negative count", `"last_given":{"at":"2026-11-09T14:02:00Z","surface":"hook-recall","count":-1}`, nil},
		{"huge count", `"last_given":{"at":"2026-11-09T14:02:00Z","surface":"hook-recall","count":100001}`, nil},
		{"string count", `"last_given":{"at":"2026-11-09T14:02:00Z","surface":"hook-recall","count":"3"}`, nil},
	}
	for _, c := range cases {
		sources, detail, index, review, got := run(head + "," + c.field + "}")
		if (got == nil) != (c.want == nil) || (got != nil && (got.At != c.want.At || got.Surface != c.want.Surface || (got.Count == nil) != (c.want.Count == nil) || (got.Count != nil && *got.Count != *c.want.Count))) {
			t.Fatalf("%s: got %+v want %+v", c.name, got, c.want)
		}
		if detail != "" || index.State != "healthy" || len(sources) != 1 || review == nil || review.Saved != 3 {
			t.Fatalf("%s: health or review lost: %+v %q %+v %+v", c.name, sources, detail, index, review)
		}
	}
	b, _ := json.Marshal(Snapshot{Given: &Given{At: "2026-11-09T14:02:00Z", Surface: "hook-recall"}})
	if !strings.Contains(string(b), `"given":{"at":"2026-11-09T14:02:00Z","surface":"hook-recall"}`) {
		t.Fatalf("count must be omitted: %s", b)
	}
}
