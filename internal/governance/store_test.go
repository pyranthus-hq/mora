package governance

import (
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testStore(t *testing.T) Store {
	t.Helper()
	now := time.Date(2026, 8, 1, 2, 3, 4, 0, time.FixedZone("local", -7*3600))
	var ids atomic.Int64
	return Store{VaultDir: t.TempDir(), NewID: func() string { return fmt.Sprintf("mem_%d", ids.Add(1)) }, Now: func() time.Time { return now }, CreatedBy: func() string { return "mora test" }}
}
func TestGovernance_ItemAtomIsExactStableID(t *testing.T) {
	a := ItemAtom("gmail", "gmail_thread/x@work")
	if a.Kind != "stable_id" || a.Value != "gmail_thread/x@work" || a.Provider != "gmail" {
		t.Fatalf("atom=%+v", a)
	}
}
func TestGovernance_LoadAbsentIsEmptyNoError(t *testing.T) {
	s := testStore(t)
	g, err := s.Load()
	if err != nil || g.Schema != SchemaVersion || len(g.Entries) != 0 {
		t.Fatalf("g=%+v err=%v", g, err)
	}
}
func TestGovernance_CorruptFailsClosed(t *testing.T) {
	s := testStore(t)
	if err := os.WriteFile(s.Path(), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil || !strings.Contains(err.Error(), "unreadable (corrupt JSON)") {
		t.Fatalf("err=%v", err)
	}
}
func TestGovernance_AppendRevokeRoundTripAndBytes(t *testing.T) {
	s := testStore(t)
	e, err := s.Append(Entry{Kind: "forget", Action: "suppress", Atom: Atom{Provider: "imessage", Kind: "handle", Value: "+14155550123"}})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "gov_1" || e.CreatedAt != "2026-08-01T02:03:04-07:00" || e.CreatedBy != "mora test" {
		t.Fatalf("entry=%+v", e)
	}
	body, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(body), "\n") || !strings.Contains(string(body), `"schema": 1`) {
		t.Fatalf("bytes=%q", body)
	}
	if info, _ := os.Stat(s.Path()); runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v", info.Mode())
	}
	found, err := s.Revoke(e.ID)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	g, err := s.Load()
	if err != nil || g.Entries[0].RevokedAt == "" {
		t.Fatalf("g=%+v err=%v", g, err)
	}
	found, err = s.Revoke(e.ID)
	if err != nil || found {
		t.Fatalf("second found=%v err=%v", found, err)
	}
}
func TestGovernance_ConcurrentAppendsNoLostUpdate(t *testing.T) {
	s := testStore(t)
	s.PostLoad = func() {
		for i := 0; i < 500; i++ {
			runtime.Gosched()
		}
	}
	const n = 16
	release := make(chan struct{})
	var ready, wg sync.WaitGroup
	ready.Add(n)
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-release
			_, err := s.Append(Entry{Kind: "forget", Action: "suppress", Atom: Atom{Kind: "handle", Value: fmt.Sprint(i)}})
			errs <- err
		}(i)
	}
	ready.Wait()
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	g, err := s.Load()
	if err != nil || len(g.Entries) != n {
		t.Fatalf("entries=%d err=%v", len(g.Entries), err)
	}
}
func TestGovernanceLeaseRejectsLiveAndReapsStale(t *testing.T) {
	s := testStore(t)
	now := s.Now()
	release, err := s.Acquire(now)
	if err != nil {
		t.Fatal(err)
	}
	wall := now
	s.WallNow = func() time.Time { wall = wall.Add(3 * time.Second); return wall }
	s.Backoff = func(int) time.Duration { return 0 }
	s.Sleep = func(time.Duration) {}
	if _, err := s.Acquire(now); err == nil {
		t.Fatal("live lease acquired")
	}
	release()
	wall = now
	s.WallNow = func() time.Time { wall = wall.Add(time.Millisecond); return wall }
	if err := os.WriteFile(s.LockPath(), []byte(`{"pid":1,"acquired_at":"2026-07-01T00:00:00Z"}`), 0600); err != nil {
		t.Fatal(err)
	}
	release, err = s.Acquire(now)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	release()
}
func TestGovernanceNormalizeAndProvider(t *testing.T) {
	if got := NormalizeIdentity("address", " A@EXAMPLE.COM "); got != "a@example.com" {
		t.Fatalf("got=%q", got)
	}
	if got := NormalizeIdentity("handle", " +123 "); got != "+123" {
		t.Fatalf("got=%q", got)
	}
	if !ProviderMatches("", "gmail") || ProviderMatches("calendar", "gmail") {
		t.Fatal("provider mismatch")
	}
}
func TestMain(m *testing.M) {
	if os.Getenv("MORA_TEST_REAL_FSYNC") != "1" {
		atomicio.MarkerSyncFn = func(*os.File) error { return nil }
		atomicio.SyncDirFn = func(string) error { return nil }
	}
	os.Exit(m.Run())
}
