package health

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/leasefile"
)

func TestProducerSuccessRingAndAdoption(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var ring []string
	for i := 0; i < 12; i++ {
		ring = AppendSuccessTime(ring, base.Add(time.Duration(i)*24*time.Hour))
	}
	if len(ring) != SuccessRing || ring[0] != base.Add(2*24*time.Hour).Format(time.RFC3339) {
		t.Fatal(ring)
	}
	cases := []struct {
		name string
		raw  []string
		want int
		ok   bool
	}{{"too few", ring[:2], 0, false}, {"invalid", []string{"bad", "also bad", "no"}, 0, false}, {"burst", []string{base.Format(time.RFC3339), base.Add(time.Minute).Format(time.RFC3339), base.Add(48 * time.Hour).Format(time.RFC3339)}, 0, false}, {"same days", []string{base.Format(time.RFC3339), base.Add(time.Hour).Format(time.RFC3339), base.Add(2 * time.Hour).Format(time.RFC3339)}, 0, false}, {"daily median", []string{base.Add(48 * time.Hour).Format(time.RFC3339), base.Format(time.RFC3339), base.Add(24 * time.Hour).Format(time.RFC3339)}, 86400, true}, {"even median", []string{base.Format(time.RFC3339), base.Add(24 * time.Hour).Format(time.RFC3339), base.Add(72 * time.Hour).Format(time.RFC3339), base.Add(144 * time.Hour).Format(time.RFC3339)}, 48 * 3600, true}, {"ceiling", []string{base.Format(time.RFC3339), base.Add(10 * 24 * time.Hour).Format(time.RFC3339), base.Add(20 * 24 * time.Hour).Format(time.RFC3339)}, 7 * 24 * 3600, true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AdoptInterval(tc.raw)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got=(%d,%v) want=(%d,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
	if medianDuration(nil) != 0 {
		t.Fatal("empty median")
	}
}
func TestClassifyProducers(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	expected := map[string]ExpectedProducer{"future": {Name: "future", IntervalSeconds: 3600}, "fresh": {Name: "fresh", IntervalSeconds: 3600, Source: "scheduled"}, "failed": {Name: "failed", IntervalSeconds: 3600}, "stale": {Name: "stale", IntervalSeconds: 3600}, "never": {Name: "never", IntervalSeconds: 3600}, "invalid": {Name: "invalid", IntervalSeconds: 3600}, "fallback": {Name: "fallback"}}
	status := map[string]ProducerStatus{"future": {LastSuccessAt: at(time.Hour)}, "fresh": {LastSuccessAt: at(-time.Hour)}, "failed": {LastSuccessAt: at(-time.Hour), LastAttemptAt: at(-30 * time.Minute), LastError: "boom"}, "stale": {LastSuccessAt: at(-2 * time.Hour)}, "invalid": {LastSuccessAt: "bad"}, "fallback": {LastSuccessAt: at(-3 * time.Hour)}}
	got := ClassifyProducers(expected, status, now, func(string) int { return 7200 })
	if len(got) != 7 {
		t.Fatal(got)
	}
	by := map[string]Producer{}
	var names []string
	for _, p := range got {
		by[p.Name] = p
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "failed,fallback,fresh,future,invalid,never,stale" {
		t.Fatal(names)
	}
	for name, want := range map[string]string{"future": ProducerFresh, "fresh": ProducerFresh, "failed": ProducerFailed, "stale": ProducerStale, "never": ProducerNever, "invalid": ProducerNever, "fallback": ProducerFresh} {
		if by[name].State != want {
			t.Fatalf("%s=%+v", name, by[name])
		}
	}
	if by["future"].AgeHours != 0 || by["fallback"].IntervalSeconds != 7200 || by["fresh"].Subject != ProducerSubjectProducer {
		t.Fatal(by)
	}
	failure := ProducerLedgerFailure(errors.New("corrupt"))
	if len(failure) != 1 || failure[0].Subject != ProducerSubjectLedger || failure[0].LastError != "corrupt" {
		t.Fatal(failure)
	}
}
func TestAttemptAfterSuccessBranches(t *testing.T) {
	if AttemptAfterSuccess(ProducerStatus{}) {
		t.Fatal("empty")
	}
	if !AttemptAfterSuccess(ProducerStatus{LastError: "boom"}) {
		t.Fatal("invalid attempt error")
	}
	if !AttemptAfterSuccess(ProducerStatus{LastAttemptAt: "2026-01-01T01:00:00Z", LastSuccessAt: "bad"}) {
		t.Fatal("invalid success")
	}
	if AttemptAfterSuccess(ProducerStatus{LastAttemptAt: "2026-01-01T00:00:00Z", LastSuccessAt: "2026-01-01T01:00:00Z"}) {
		t.Fatal("older attempt")
	}
}
func TestBriefArtifactFresh(t *testing.T) {
	vault := t.TempDir()
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	if ok, present := BriefArtifactFresh(vault, now); !ok || present {
		t.Fatal(ok, present)
	}
	dir := filepath.Join(vault, "briefs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk-brief.md"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if ok, present := BriefArtifactFresh(vault, now); !ok || present {
		t.Fatal(ok, present)
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-01-10-brief.md"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if ok, present := BriefArtifactFresh(vault, now); !ok || !present {
		t.Fatal(ok, present)
	}
	if err := os.Remove(filepath.Join(dir, "2026-01-10-brief.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-01-08-brief.md"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if ok, present := BriefArtifactFresh(vault, now); ok || !present {
		t.Fatal(ok, present)
	}
}
func TestProducerStorePathsBytesAndCorruption(t *testing.T) {
	store := ProducerStore{StateDir: t.TempDir()}
	if !strings.HasSuffix(store.StatusPath(), filepath.Join("producers", "status.json")) || !strings.HasSuffix(store.ExpectedPath(), filepath.Join("producers", "expected.json")) || store.LockPath() != store.StatusPath()+".lock" {
		t.Fatal(store)
	}
	status, err := store.LoadStatus()
	if err != nil || len(status) != 0 {
		t.Fatal(status, err)
	}
	exp, err := store.LoadExpected()
	if err != nil || len(exp) != 0 {
		t.Fatal(exp, err)
	}
	status = map[string]ProducerStatus{"b": {Name: "b", SuccessTimes: []string{}}, "a": {Name: "a", LastError: "boom", SuccessTimes: []string{"x"}}}
	if err := store.SaveStatus(status); err != nil {
		t.Fatal(err)
	}
	exp = map[string]ExpectedProducer{"a": {Name: "a", IntervalSeconds: 3, Source: "adopted", AdoptedAt: "at"}}
	if err := store.SaveExpected(exp); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{store.StatusPath(), store.ExpectedPath()} {
			info, err := os.Stat(p)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("%s mode=%v err=%v", p, info.Mode().Perm(), err)
			}
		}
	}
	if got, err := store.LoadStatus(); err != nil || got["a"].LastError != "boom" {
		t.Fatal(got, err)
	}
	if got, err := store.LoadExpected(); err != nil || got["a"].AdoptedAt != "at" {
		t.Fatal(got, err)
	}
	if err := os.WriteFile(store.StatusPath(), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadStatus(); err == nil || !strings.Contains(err.Error(), "producer ledger "+store.StatusPath()+" is corrupt") {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.ExpectedPath(), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadExpected(); err == nil || !strings.Contains(err.Error(), "producer expectation "+store.ExpectedPath()+" is corrupt") {
		t.Fatal(err)
	}
}
func TestProducerStoreMutateSerializesAndAborts(t *testing.T) {
	// This witness tests reload-under-lock serialization, not the independent
	// two-second acquisition deadline. Freeze only wall time so a deliberately
	// simultaneous 20-writer stampede cannot consume that production budget on
	// a slow CI filesystem before the last legitimate waiter gets its turn.
	wall := time.Now()
	store := ProducerStore{StateDir: t.TempDir(), WallNow: func() time.Time { return wall }}
	if err := store.MutateStatus(func(map[string]ProducerStatus) error { return errors.New("stop") }); err == nil {
		t.Fatal("mutation error swallowed")
	}
	if _, err := os.Stat(store.StatusPath()); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.MutateStatus(func(m map[string]ProducerStatus) error { v := m["x"]; v.IntervalSeconds++; m["x"] = v; return nil })
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.LoadStatus()
	if err != nil || got["x"].IntervalSeconds != n {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
func TestProducerStoreAcquireDeadlineAndStaleReap(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wall := base
	store := ProducerStore{StateDir: dir, WallNow: func() time.Time { return wall }, Sleep: func(d time.Duration) { wall = wall.Add(d) }, Backoff: func(int) time.Duration { return time.Second }, PID: func() int { return 42 }}
	release, err := store.Acquire(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.LockPath()); err != nil {
		t.Fatal(err)
	}
	_, err = store.Acquire(base)
	if err == nil || !strings.Contains(err.Error(), "producer ledger is locked by another mora process ("+store.LockPath()+"); retry in a moment") {
		t.Fatal(err)
	}
	wall = base
	over := ProducerStore{StateDir: dir, WallNow: func() time.Time { return wall }, Sleep: func(d time.Duration) { wall = wall.Add(d + 2*time.Second) }, Backoff: func(int) time.Duration { return time.Millisecond }}
	if _, err := over.Acquire(base); err == nil {
		t.Fatal("overslept deadline allowed retry")
	}
	release()
	release()
	if _, err := os.Stat(store.LockPath()); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	wall = base
	release, err = store.Acquire(base)
	if err != nil {
		t.Fatal(err)
	}
	wall = base.Add(31 * time.Second)
	release2, err := store.Acquire(base.Add(31 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	release()
	release2()
}

func TestProducerStoreErrorBranches(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (ProducerStore{StateDir: filepath.Join(parent, "state")}).Acquire(time.Now()); err == nil {
		t.Fatal("mkdir error swallowed")
	}
	if err := (ProducerStore{StateDir: filepath.Join(parent, "state")}).MutateStatus(func(map[string]ProducerStatus) error { return nil }); err == nil {
		t.Fatal("mutate acquire error swallowed")
	}
	boom := errors.New("boom")
	if _, err := (ProducerStore{StateDir: t.TempDir(), Publish: func(string, []byte) (bool, error) { return false, boom }}).Acquire(time.Now()); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wall := base
	store := ProducerStore{StateDir: t.TempDir(), WallNow: func() time.Time { return wall }, Sleep: func(d time.Duration) { wall = wall.Add(d) }, Backoff: func(int) time.Duration { return time.Second }, Publish: func(string, []byte) (bool, error) { return false, nil }, Reap: func(string, time.Time, time.Duration, leasefile.RemovalOptions) (bool, error) { return false, boom }}
	// A synthetic non-retryable reap error is surfaced once publish reports contention.
	_, err := store.Acquire(base)
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	dirStore := ProducerStore{StateDir: t.TempDir()}
	if err := os.MkdirAll(dirStore.StatusPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := dirStore.LoadStatus(); err == nil {
		t.Fatal("status read error swallowed")
	}
	if err := os.RemoveAll(dirStore.StatusPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirStore.ExpectedPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := dirStore.LoadExpected(); err == nil {
		t.Fatal("expected read error swallowed")
	}
	saveStore := ProducerStore{StateDir: t.TempDir()}
	if err := saveStore.MutateStatus(func(map[string]ProducerStatus) error { return os.Mkdir(saveStore.StatusPath(), 0700) }); err == nil {
		t.Fatal("save error swallowed")
	}
	corruptStore := ProducerStore{StateDir: t.TempDir()}
	if err := os.MkdirAll(corruptStore.Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptStore.StatusPath(), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := corruptStore.MutateStatus(func(map[string]ProducerStatus) error { return nil }); err == nil {
		t.Fatal("load error swallowed")
	}
}
