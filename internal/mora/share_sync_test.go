package mora

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

type shareSyncCall struct {
	verb string
	args []string
}

// withShareSyncVerbs replaces the push and pull verbs that `share sync` drives,
// recording every call. fail names the shares whose verb should return an error.
func withShareSyncVerbs(t *testing.T, fail map[string]error) *[]shareSyncCall {
	t.Helper()
	origPush, origPull := shareSyncPush, shareSyncPull
	t.Cleanup(func() { shareSyncPush, shareSyncPull = origPush, origPull })
	calls := []shareSyncCall{}
	shareSyncPush = func(_ context.Context, _ Config, args []string, stdout io.Writer, _ io.Reader, _ execFunc) error {
		calls = append(calls, shareSyncCall{verb: "push", args: append([]string(nil), args...)})
		if err := fail[args[0]]; err != nil {
			return err
		}
		_, _ = io.WriteString(stdout, "preview line\nshare \""+args[0]+"\" pushed — no changes.\n")
		return nil
	}
	shareSyncPull = func(_ context.Context, _ Config, args []string, stdout io.Writer, _ execFunc) error {
		calls = append(calls, shareSyncCall{verb: "pull", args: append([]string(nil), args...)})
		if err := fail[args[0]]; err != nil {
			return err
		}
		_, _ = io.WriteString(stdout, "share \""+args[0]+"\": 1 new/updated, 0 removed, 3 total.\n")
		return nil
	}
	return &calls
}

func addSubscriptionEntry(t *testing.T, cfg Config, name string) {
	t.Helper()
	sf, err := loadShares(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sf.Subscriptions = append(sf.Subscriptions, shareSubscription{Name: name, Remote: "r", CreatedAt: "2026-07-01T00:00:00Z"})
	if err := saveShares(cfg, sf); err != nil {
		t.Fatal(err)
	}
}

var shareSyncLine = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z (push|pull) \S+: (ok|error): .+$`)

// TestShareSyncPullsEverySubscriptionAndPushesEveryPublish locks the F6 job
// contract: one run pulls every subscription and pushes every publish (push
// non-interactively, with --yes), and logs exactly one timestamped line per share.
func TestShareSyncPullsEverySubscriptionAndPushesEveryPublish(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	rec := testRecipient(t)
	setupPublish(t, cfg, "acme", "project:acme", rec)
	setupPublish(t, cfg, "beta", "project:beta", rec)
	addSubscriptionEntry(t, cfg, "neil")
	addSubscriptionEntry(t, cfg, "sam")
	calls := withShareSyncVerbs(t, nil)

	out := run(t, "share", "sync")

	want := []shareSyncCall{
		{verb: "pull", args: []string{"neil"}},
		{verb: "pull", args: []string{"sam"}},
		{verb: "push", args: []string{"acme", "--yes"}},
		{verb: "push", args: []string{"beta", "--yes"}},
	}
	if len(*calls) != len(want) {
		t.Fatalf("calls = %#v, want %#v", *calls, want)
	}
	for i, c := range *calls {
		if c.verb != want[i].verb || strings.Join(c.args, " ") != strings.Join(want[i].args, " ") {
			t.Fatalf("call %d = %#v, want %#v", i, c, want[i])
		}
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want exactly one log line per share (4), got %d:\n%s", len(lines), out)
	}
	for _, l := range lines {
		if !shareSyncLine.MatchString(l) {
			t.Fatalf("log line %q does not match the one-line-per-share format", l)
		}
	}
	if !strings.Contains(lines[0], "pull neil: ok: share \"neil\": 1 new/updated") {
		t.Fatalf("pull line should carry the verb's summary line, got %q", lines[0])
	}
	if !strings.Contains(lines[2], "push acme: ok: share \"acme\" pushed — no changes.") || strings.Contains(out, "preview line") {
		t.Fatalf("push line should carry only the verb's last line, got:\n%s", out)
	}
}

// TestShareSyncContinuesPastAFailureButExitsNonZero: one bad share must not stop
// the others from syncing, and the run must still fail loudly (honest snapshot:
// a scheduler has to see the failure, not a green exit).
func TestShareSyncContinuesPastAFailureButExitsNonZero(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	setupPublish(t, cfg, "acme", "project:acme", testRecipient(t))
	addSubscriptionEntry(t, cfg, "neil")
	addSubscriptionEntry(t, cfg, "sam")
	calls := withShareSyncVerbs(t, map[string]error{"neil": errors.New("git fetch: exit status 128\nfatal: could not read Username")})

	out, err := runErr(t, "share", "sync")
	if err == nil {
		t.Fatalf("share sync with a failed share must exit non-zero; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "1 of 3") {
		t.Fatalf("error should count failed shares, got %v", err)
	}
	if len(*calls) != 3 {
		t.Fatalf("a failed pull must not stop the remaining shares; calls=%#v", *calls)
	}
	var errLine string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "pull neil:") {
			errLine = l
		}
	}
	if !shareSyncLine.MatchString(errLine) || !strings.Contains(errLine, "error: git fetch: exit status 128 fatal: could not read Username") {
		t.Fatalf("failure must be logged on ONE line with the error text, got %q in:\n%s", errLine, out)
	}
}

func TestShareSyncWithNoSharesSaysSoAndSucceeds(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	calls := withShareSyncVerbs(t, nil)
	out := run(t, "share", "sync")
	if len(*calls) != 0 || !strings.Contains(out, "no publishes or subscriptions") {
		t.Fatalf("calls=%#v out=%q", *calls, out)
	}
}

func TestShareSyncRejectsArguments(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	withShareSyncVerbs(t, nil)
	if _, err := runErr(t, "share", "sync", "acme"); err == nil || !strings.Contains(err.Error(), "usage: mora share sync") {
		t.Fatalf("share sync acme = %v; want usage error", err)
	}
}

// TestShareSyncSkipsWhileAnotherRunHoldsTheLock: a slow run (big pull) must not
// overlap the next timer fire, or a hand-run sync. The second run says so on one
// line and exits 0 without touching any share.
func TestShareSyncSkipsWhileAnotherRunHoldsTheLock(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	addSubscriptionEntry(t, cfg, "neil")
	calls := withShareSyncVerbs(t, nil)

	lockPath := shareSyncLockPath(cfg)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := leasefile.TryLock(f); err != nil {
		t.Fatal(err)
	}

	out := run(t, "share", "sync")
	if len(*calls) != 0 {
		t.Fatalf("a locked run must not touch shares; calls=%#v", *calls)
	}
	if !strings.Contains(out, "skipped: another share sync is running") {
		t.Fatalf("skip must be reported, got %q", out)
	}
	if !strings.HasPrefix(lockPath, cfg.StateDir) {
		t.Fatalf("lock %s must live in the state dir %s, never the vault", lockPath, cfg.StateDir)
	}
}

// TestShareSyncFailsWhenTheLockItselfFails: only contention is a skip. A lock
// that cannot be taken at all must fail the run, or every scheduled fire would
// report success while nothing syncs.
func TestShareSyncFailsWhenTheLockItselfFails(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	addSubscriptionEntry(t, mustConfig(t), "neil")
	calls := withShareSyncVerbs(t, nil)
	orig := shareSyncTryLock
	shareSyncTryLock = func(*os.File) error { return errors.New("no locks available") }
	t.Cleanup(func() { shareSyncTryLock = orig })

	out, err := runErr(t, "share", "sync")
	if err == nil || !strings.Contains(err.Error(), "no locks available") {
		t.Fatalf("lock failure must fail the run, got err=%v out=%q", err, out)
	}
	if len(*calls) != 0 || strings.Contains(out, "skipped") {
		t.Fatalf("calls=%#v out=%q", *calls, out)
	}
}

// TestShareSyncPullsARealGitRemote runs the real pull verb end to end against a
// local git remote: the publisher adds a memory and the next sync imports it.
func TestShareSyncPullsARealGitRemote(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	remote := realGitShareRemote(t, id.Recipient(),
		[]Memory{fixtureMemory("mem_20260601_000000_aaaaaaaa", "Shared", "content")})
	var buf bytes.Buffer
	if err := shareSubscribe(context.Background(), cfg, []string{"neil", "--remote", remote}, &buf, realExec); err != nil {
		t.Fatalf("subscribe: %v\n%s", err, buf.String())
	}
	body, err := renderMemory(fixtureMemory("mem_20260601_000001_bbbbbbbb", "Second", "more"))
	if err != nil {
		t.Fatal(err)
	}
	ct, err := encryptShareBytes([]age.Recipient{id.Recipient()}, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "memories", "mem_20260601_000001_bbbbbbbb.md.age"), ct, 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, remote, "add", "-A")
	mustGit(t, remote, "commit", "-q", "-m", "add second")

	out := run(t, "share", "sync")
	if !strings.Contains(out, "pull neil: ok: share \"neil\": ") || !strings.Contains(out, "2 total.") {
		t.Fatalf("unexpected sync log:\n%s", out)
	}
	if _, ok := findSharedMemory(cfg, "mem_20260601_000001_bbbbbbbb"); !ok {
		t.Fatal("share sync did not import the publisher's new memory")
	}
}
