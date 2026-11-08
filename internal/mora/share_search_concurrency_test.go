package mora

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharedSearchWorkersOverlapBoundAndKeepOrder(t *testing.T) {
	subs := make([]shareSubscription, 12)
	for i := range subs {
		subs[i].Name = fmt.Sprint(i)
	}
	entered := make(chan string, len(subs))
	release := make(chan struct{})
	done := make(chan []shareSearchResult, 1)
	var active, peak atomic.Int32
	go func() {
		done <- searchSharedSubscriptions(subs, func(sub shareSubscription) shareSearchResult {
			n := active.Add(1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			entered <- sub.Name
			<-release
			active.Add(-1)
			return shareSearchResult{memories: []Memory{{Owner: sub.Name}}}
		})
	}()
	// Four workers must reach the barrier without waiting for an earlier result.
	for i := 0; i < shareSearchConcurrency; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("independent reads did not overlap")
		}
	}
	select {
	case <-entered:
		close(release)
		t.Fatal("worker limit exceeded")
	default:
	}
	close(release)
	var got []shareSearchResult
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("workers failed to join")
	}
	if peak.Load() != shareSearchConcurrency || active.Load() != 0 {
		t.Fatalf("peak=%d active=%d", peak.Load(), active.Load())
	}
	for i := range got {
		if got[i].memories[0].Owner != subs[i].Name {
			t.Fatalf("result %d reordered: %+v", i, got[i])
		}
	}
}

func TestSharedSearchMixedCorporaRepairAndReadOnly(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	names := []string{"zeta", "broken", "alpha"}
	for i, name := range names {
		registerSub(t, cfg, name)
		m := fixtureMemory(fmt.Sprintf("mem_20260601_00000%d_aaaaaaaa", i), "Zanzibar", "zanzibar harbor notes")
		publishGen(t, cfg, name, id, []Memory{m})
	}
	old := mustHead(t, cfg, "broken")
	if err := os.WriteFile(shareGenIndexPath(cfg, "broken", old.Gen), []byte("corrupt index"), 0600); err != nil {
		t.Fatal(err)
	}
	lists, err := searchSharedCorpora(withReadOnly(context.Background()), cfg, "zanzibar", "", 10)
	if !errors.Is(err, ErrReadOnlyRepairNeeded) {
		t.Fatalf("read-only refusal = %v", err)
	}
	if len(lists) != 2 || lists[0][0].Owner != "zeta" || lists[1][0].Owner != "alpha" {
		t.Fatalf("healthy corpus order = %+v", lists)
	}
	if head := mustHead(t, cfg, "broken"); head.Gen != old.Gen {
		t.Fatal("read-only search published a repair")
	}
	lists, err = searchSharedCorpora(context.Background(), cfg, "zanzibar", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != len(names) {
		t.Fatalf("repaired corpora = %+v", lists)
	}
	for i, name := range names {
		if lists[i][0].Owner != name {
			t.Fatalf("owner %d = %q, want %q", i, lists[i][0].Owner, name)
		}
	}
	if head := mustHead(t, cfg, "broken"); head.Gen == old.Gen {
		t.Fatal("writable search did not publish repair")
	}
}
