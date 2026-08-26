package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestIsolationConcurrencyBound(t *testing.T) {
	plans := make([]sourceRunPlan, 20)
	for i := range plans {
		plans[i].Key = fmt.Sprintf("source-%02d", i)
	}
	var active, maximum atomic.Int32
	outcomes := runSourcePlan(testCtx(t), plans, sourceRunOptions{
		SourceTimeout: time.Second,
		Run: func(context.Context, sourceRunPlan) sourceRunOutcome {
			n := active.Add(1)
			defer active.Add(-1)
			for {
				old := maximum.Load()
				if n <= old || maximum.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			return sourceRunOutcome{Items: 1, Materialized: 1}
		},
	})
	if len(outcomes) != len(plans) || maximum.Load() > defaultSourceRunConcurrency {
		t.Fatalf("outcomes=%d maximum=%d, bound=%d", len(outcomes), maximum.Load(), defaultSourceRunConcurrency)
	}
}

func TestIsolationSourceTimeoutDoesNotCancelSibling(t *testing.T) {
	plans := []sourceRunPlan{{Key: "blocked"}, {Key: "healthy"}}
	outcomes := runSourcePlan(testCtx(t), plans, sourceRunOptions{
		Concurrency: 2, SourceTimeout: 25 * time.Millisecond,
		Run: func(ctx context.Context, plan sourceRunPlan) sourceRunOutcome {
			if plan.Key == "blocked" {
				<-ctx.Done()
				return sourceRunOutcome{Err: ctx.Err()}
			}
			return sourceRunOutcome{Items: 1, Materialized: 1}
		},
	})
	if !outcomes[0].TimedOut || outcomes[0].Cancelled || outcomes[1].Err != nil || outcomes[1].Materialized != 1 {
		t.Fatalf("outcomes = %+v", outcomes)
	}
}

func TestIsolationGlobalCancellationStopsScheduling(t *testing.T) {
	plans := []sourceRunPlan{{Key: "started"}, {Key: "queued-a"}, {Key: "queued-b"}}
	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int32
	outcomes := runSourcePlan(ctx, plans, sourceRunOptions{
		Concurrency: 1, SourceTimeout: time.Second,
		Run: func(ctx context.Context, _ sourceRunPlan) sourceRunOutcome {
			started.Add(1)
			cancel()
			<-ctx.Done()
			return sourceRunOutcome{Err: ctx.Err()}
		},
	})
	if started.Load() != 1 {
		t.Fatalf("started %d sources after global cancellation", started.Load())
	}
	for _, outcome := range outcomes {
		if !outcome.Cancelled {
			t.Fatalf("unfinished source not cancelled: %+v", outcomes)
		}
	}
	aggregate, err := aggregateSourceRuns(plans, outcomes, nil, nil)
	if err == nil || aggregate.Status != sourceRunStatusCancelled {
		t.Fatalf("aggregate=%+v err=%v", aggregate, err)
	}
}

func TestIsolationStableReceiptOrderForSimultaneousCompletion(t *testing.T) {
	plans := []sourceRunPlan{{Key: "z"}, {Key: "a"}, {Key: "m"}}
	ready := sync.WaitGroup{}
	ready.Add(len(plans))
	release := make(chan struct{})
	go func() {
		ready.Wait()
		close(release)
	}()
	outcomes := runSourcePlan(testCtx(t), plans, sourceRunOptions{
		Concurrency: len(plans), SourceTimeout: time.Second,
		Run: func(context.Context, sourceRunPlan) sourceRunOutcome {
			ready.Done()
			<-release
			return sourceRunOutcome{Items: 1, Materialized: 1}
		},
	})
	aggregate, err := aggregateSourceRuns(plans, outcomes, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, receipt := range aggregate.Sources {
		if receipt.Source != plans[i].Key {
			t.Fatalf("receipt order = %+v, want plan order", aggregate.Sources)
		}
	}
}

func TestIsolationNoWorkAfterReturn(t *testing.T) {
	plans := []sourceRunPlan{{Key: "a"}, {Key: "b"}, {Key: "c"}}
	ctx, cancel := context.WithCancel(context.Background())
	var active atomic.Int32
	time.AfterFunc(20*time.Millisecond, cancel)
	runSourcePlan(ctx, plans, sourceRunOptions{
		Concurrency: 3, SourceTimeout: time.Second,
		Run: func(ctx context.Context, _ sourceRunPlan) sourceRunOutcome {
			active.Add(1)
			defer active.Add(-1)
			<-ctx.Done()
			return sourceRunOutcome{Err: ctx.Err()}
		},
	})
	if active.Load() != 0 {
		t.Fatalf("%d source workers remained active after return", active.Load())
	}
}

type isolationPageFetcher struct{ items []memory.Item }

func (f isolationPageFetcher) FetchPage(memory.ItemKind, memory.FetchWindow, string) (memory.Page, error) {
	return memory.Page{Items: f.items}, nil
}

func TestIsolationPartialSuccessReceipt(t *testing.T) {
	plans := make([]sourceRunPlan, 8)
	outcomes := make([]sourceRunOutcome, 8)
	for i := range plans {
		key := string(rune('a' + i))
		plans[i] = sourceRunPlan{Key: key, Source: Source{Name: key, Type: "gmail"}}
		outcomes[i] = sourceRunOutcome{Key: key, Items: 1}
	}
	outcomes[7].Items = 0
	outcomes[7].Err = newCodedError(errCodeConnectorUnavailable, errors.New("offline"), "offline")
	aggregate, err := aggregateSourceRuns(plans, outcomes, nil, nil)
	if err != nil {
		t.Fatalf("usable partial result returned error: %v", err)
	}
	if aggregate.Status != sourceRunStatusPartial || !aggregate.Usable || aggregate.SuccessfulSources != 7 || aggregate.FailedSources != 1 || len(aggregate.Sources) != 8 {
		t.Fatalf("aggregate = %+v", aggregate)
	}
	failed := aggregate.Sources[7]
	if failed.Source != "h" || failed.ErrorCode != errCodeConnectorUnavailable || !failed.Retryable {
		t.Fatalf("failed receipt = %+v", failed)
	}
}

func TestIsolationPartialWriteCounts(t *testing.T) {
	plans := []sourceRunPlan{{Key: "gmail:work"}}
	outcomes := []sourceRunOutcome{{
		Key: "gmail:work", Items: 73, Examined: 100, Materialized: 73, Failed: 27, Missing: 27,
		Stages: memory.IngestStages{FetchMS: 12, MapWriteMS: 34, TotalMS: 46, Pages: 2, Bytes: 8192, Retries: 1},
		Err:    newCodedError(errCodeConnectorMalformed, nil, "27 malformed items"),
	}}
	aggregate, err := aggregateSourceRuns(plans, outcomes, nil, nil)
	if err != nil {
		t.Fatalf("usable partial source: %v", err)
	}
	if aggregate.Status != sourceRunStatusPartial || !aggregate.Usable || len(aggregate.Sources) != 1 {
		t.Fatalf("aggregate = %+v", aggregate)
	}
	receipt := aggregate.Sources[0]
	if receipt.Status != sourceRunStatusPartial || !receipt.Usable || receipt.Examined != 100 || receipt.Materialized != 73 || receipt.Failed != 27 || receipt.Missing != 27 {
		t.Fatalf("receipt = %+v", receipt)
	}
	if receipt.Stages.TotalMS != 46 || receipt.Stages.Pages != 2 || receipt.Stages.Bytes != 8192 || receipt.Stages.Retries != 1 {
		t.Fatalf("stage receipt = %+v", receipt.Stages)
	}
}

func TestIncrementalNoChangesIsSuccessNotEmptyCorpus(t *testing.T) {
	aggregate, err := aggregateSourceRuns([]sourceRunPlan{{Key: "gmail"}}, []sourceRunOutcome{{Key: "gmail", Incremental: true}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregate.Sources) != 1 || aggregate.Sources[0].Status != sourceRunStatusSuccess || aggregate.Sources[0].ErrorCode != "" || !aggregate.Sources[0].Incremental {
		t.Fatalf("no-change incremental receipt = %+v", aggregate.Sources)
	}
}

func TestIsolationPartialWriteSearchableAfterRebuild(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	items := make([]memory.Item, 100)
	for i := range items {
		items[i] = memory.Item{
			Kind: "gmail_thread", ProviderID: fmt.Sprintf("partial-%03d", i),
			Title: fmt.Sprintf("Partial %03d", i), Body: "partialwrite73token", OccurredAt: time.Now(),
		}
	}
	attempted := 0
	result, ingestErr := memory.Ingest(memory.IngestParams{
		Fetcher: isolationPageFetcher{items: items}, Kind: "gmail_thread", Scope: "global",
		Status: &memory.SyncStatus{Source: "gmail"},
		Write: func(mapped memory.MappedMemory) error {
			attempted++
			if attempted <= 27 {
				return errors.New("injected item failure")
			}
			return writeMappedMemory(cfg, mapped)
		},
	})
	if ingestErr == nil {
		t.Fatal("partial attempt must remain non-nil")
	}
	if result.Examined != 100 || result.Materialized != 73 || result.Failed != 27 || result.Missing != 27 {
		t.Fatalf("counts = %+v", result)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("covering rebuild: %v", err)
	}
	hits, err := searchMemories(context.Background(), cfg, "partialwrite73token", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 73 {
		t.Fatalf("searchable materialized records = %d, want 73", len(hits))
	}
}

func TestIsolationPartialAttemptCountsPersistInState(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	original := ingestSourceDispatchFn
	t.Cleanup(func() { ingestSourceDispatchFn = original })
	ingestSourceDispatchFn = func(context.Context, Config, Source, io.Writer) (sourceIngestResult, error) {
		return sourceIngestResult{Examined: 100, Materialized: 73, Failed: 27, Missing: 27},
			newCodedError(errCodeConnectorMalformed, nil, "partial mapping failure")
	}
	result, err := ingestSourceDetailed(context.Background(), cfg, Source{Name: "mail", Type: "gmail", Scope: "global"}, io.Discard)
	if err == nil || result.Materialized != 73 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	activities := operationActivities(cfg, time.Now().Add(time.Second), func(int) bool { return false })
	var counts operationCounts
	found := false
	for _, activity := range activities {
		if activity.Kind == operationKindIngest {
			counts = activity.Counts
			found = true
		}
	}
	if !found {
		t.Fatalf("ingest activity absent: %+v", activities)
	}
	if counts.Examined != 100 || counts.Materialized != 73 || counts.Errors != 27 || counts.Missing != 27 {
		t.Fatalf("durable counts = %+v", counts)
	}
}

func TestIsolationAggregateEdges(t *testing.T) {
	plans := []sourceRunPlan{
		{Key: "gmail", Source: Source{Name: "gmail", Type: "gmail"}},
		{Key: "imessage", Source: Source{Name: "imessage", Type: "imessage"}},
	}
	subRun(t, "all failed", func(t *testing.T) {
		outcomes := []sourceRunOutcome{
			{Key: "gmail", Err: newCodedError(errCodeConnectorUnavailable, nil, "offline")},
			{Key: "imessage", Err: newCodedError(errCodeConnectorMalformed, nil, "bad payload")},
		}
		aggregate, err := aggregateSourceRuns(plans, outcomes, nil, nil)
		if err == nil || aggregate.Status != sourceRunStatusFailed || aggregate.Usable || aggregate.FailedSources != 2 || len(aggregate.Sources) != 2 {
			t.Fatalf("aggregate=%+v err=%v", aggregate, err)
		}
	})
	subRun(t, "clean empty", func(t *testing.T) {
		aggregate, err := aggregateSourceRuns(plans[:1], []sourceRunOutcome{{Key: "gmail"}}, nil, nil)
		if err != nil || !aggregate.Usable || aggregate.FailedSources != 0 || len(aggregate.Sources) != 1 {
			t.Fatalf("aggregate=%+v err=%v", aggregate, err)
		}
		receipt := aggregate.Sources[0]
		if receipt.Status != sourceRunStatusEmpty || receipt.ErrorCode != errCodeConnectorEmpty || receipt.ErrorClass != connectorClassEmpty || receipt.Retryable {
			t.Fatalf("empty receipt = %+v", receipt)
		}
	})
	subRun(t, "failed zero is not empty", func(t *testing.T) {
		aggregate, err := aggregateSourceRuns(plans[:1], []sourceRunOutcome{{Key: "gmail", Err: errors.New("boom")}}, nil, nil)
		if err == nil || aggregate.Sources[0].Status != sourceRunStatusFailed || aggregate.Sources[0].ErrorCode != errCodeConnectorUnclassified {
			t.Fatalf("aggregate=%+v err=%v", aggregate, err)
		}
	})
}

func TestIsolationTypedSourceFailures(t *testing.T) {
	cases := []struct {
		name, code, class string
		retryable         bool
	}{
		{"timeout", errCodeConnectorUnavailable, connectorClassUnavailable, true},
		{"malformed", errCodeConnectorMalformed, connectorClassMalformed, false},
		{"unauthorized", errCodeConnectorUnauthorized, connectorClassUnauthorized, false},
		{"database", errCodeConnectorUnavailable, connectorClassUnavailable, true},
		{"unclassified", errCodeConnectorUnclassified, connectorClassUnclassified, false},
	}
	for _, tc := range cases {
		subRun(t, tc.name, func(t *testing.T) {
			plans := []sourceRunPlan{{Key: "failed"}, {Key: "healthy"}}
			outcomes := []sourceRunOutcome{
				{Key: "failed", Err: newCodedError(tc.code, nil, "%s", tc.name)},
				{Key: "healthy", Items: 2},
			}
			aggregate, err := aggregateSourceRuns(plans, outcomes, nil, nil)
			if err != nil {
				t.Fatalf("healthy peer should keep result usable: %v", err)
			}
			var failed, healthy sourceRunReceipt
			for _, receipt := range aggregate.Sources {
				switch receipt.Source {
				case "failed":
					failed = receipt
				case "healthy":
					healthy = receipt
				}
			}
			if failed.ErrorCode != tc.code || failed.ErrorClass != tc.class || failed.Retryable != tc.retryable || failed.Status != sourceRunStatusFailed {
				t.Fatalf("failed receipt = %+v", failed)
			}
			if healthy.Status != sourceRunStatusSuccess || !healthy.Usable || healthy.ErrorCode != "" {
				t.Fatalf("healthy receipt contaminated by peer = %+v", healthy)
			}
		})
	}
}

func TestIsolationReceiptOrdering(t *testing.T) {
	plans := []sourceRunPlan{{Key: "a"}, {Key: "b"}, {Key: "c"}, {Key: "d"}}
	base := []sourceRunOutcome{{Key: "a", Items: 1}, {Key: "b", Items: 1}, {Key: "c", Items: 1}, {Key: "d", Items: 1}}
	var want []byte
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 100; i++ {
		outcomes := append([]sourceRunOutcome(nil), base...)
		rng.Shuffle(len(outcomes), func(i, j int) { outcomes[i], outcomes[j] = outcomes[j], outcomes[i] })
		aggregate, err := aggregateSourceRuns(plans, outcomes, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(aggregate)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			want = raw
		} else if !bytes.Equal(raw, want) {
			t.Fatalf("permutation %d changed receipt order:\n%s\n%s", i, want, raw)
		}
	}
}

func TestIsolationSourceSelectorPlanning(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{
		{Name: "gmail", Type: "gmail", Enabled: genericutil.Ptr(true)},
		{Name: "gmail-work", Type: "gmail", Account: "work", Enabled: genericutil.Ptr(true)},
		{Name: "calendar", Type: "calendar", Enabled: genericutil.Ptr(true)},
	}); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"", "gmail:", "gmail:work:extra", "unknown"} {
		subRun(t, "invalid_"+strings.ReplaceAll(selector, ":", "_"), func(t *testing.T) {
			plans, _, err := planSourceRuns(cfg, selector, true, time.Now())
			if err == nil || len(plans) != 0 {
				t.Fatalf("selector %q: plans=%v err=%v, want pre-construction failure", selector, plans, err)
			}
			var stdout bytes.Buffer
			if err := cmdPulse(testCtx(t), []string{"--digest", "--sync", "--source", selector}, &stdout, io.Discard); err == nil {
				t.Fatalf("selector %q: command succeeded; invalid scope must fail before construction", selector)
			}
		})
	}
	plans, _, err := planSourceRuns(cfg, "gmail", true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := planKeys(plans); !reflect.DeepEqual(got, []string{"gmail", "gmail:work"}) {
		t.Fatalf("bare family plans = %v, want every enabled Gmail instance", got)
	}
	plans, _, err = planSourceRuns(cfg, "gmail:work", true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := planKeys(plans); !reflect.DeepEqual(got, []string{"gmail:work"}) {
		t.Fatalf("account selector plans = %v, want gmail:work", got)
	}
}

func TestIsolationFilteredNoticeHasBroadStatusOnly(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{
		{Name: "gmail", Type: "gmail", Enabled: genericutil.Ptr(true)},
		{Name: "applecalendar", Type: "applecalendar", Enabled: genericutil.Ptr(true)},
	}); err != nil {
		t.Fatal(err)
	}
	seedSyncStatusFull(t, cfg, "applecalendar", &memory.SyncStatus{
		Source: "applecalendar", LastAttemptAt: time.Now().UTC().Format(time.RFC3339),
		LastError: "private database path and remediation must not leak", ErrorCount: 1,
	})
	_, notices, err := planSourceRuns(cfg, "gmail", true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0].Source != "applecalendar" {
		t.Fatalf("notices = %+v, want broad Apple Calendar notice", notices)
	}
	raw, err := json.Marshal(notices[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields["source"] != "applecalendar" || fields["status"] == "" {
		t.Fatalf("notice fields = %s, want exactly source and status", raw)
	}
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "remediation") {
		t.Fatalf("notice leaked detailed error: %s", raw)
	}
}

func TestIsolationZeroEnabledSourcesIsSuccessfulEmptyPlan(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{{Name: "gmail", Type: "gmail", Enabled: genericutil.Ptr(false)}}); err != nil {
		t.Fatal(err)
	}
	called := false
	result, err := sourceRunCoordinator(testCtx(t), sourceRunRequest{
		Config: cfg,
		run: func(context.Context, Config, Source, io.Writer) (int, error) {
			called = true
			return 0, nil
		},
	})
	if err != nil {
		t.Fatalf("zero enabled sources: %v", err)
	}
	if called || len(result.Plans) != 0 || len(result.Trace) != 0 || result.Items != 0 || result.Failures != 0 {
		t.Fatalf("empty execution = %+v called=%v", result, called)
	}
}

func planKeys(plans []sourceRunPlan) []string {
	keys := make([]string, len(plans))
	for i, plan := range plans {
		keys[i] = plan.Key
	}
	return keys
}

func TestIsolationFilteredPulseConstructsOnlyRequestedSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{
		{Name: "gmail", Type: "gmail", Enabled: genericutil.Ptr(true)},
		{Name: "applecalendar", Type: "applecalendar", Enabled: genericutil.Ptr(true)},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seedSyncStatus(t, cfg, "gmail", now.Add(-time.Hour))
	digestSeed(t, cfg, "gmail", "Requested Gmail evidence", time.Hour, now)

	orig := sourceRunCoordinatorFn
	t.Cleanup(func() { sourceRunCoordinatorFn = orig })
	var trace []string
	sourceRunCoordinatorFn = func(ctx context.Context, req sourceRunRequest) (sourceRunResult, error) {
		req.run = func(_ context.Context, _ Config, source Source, _ io.Writer) (int, error) {
			if source.Type == "applecalendar" {
				return 0, errors.New("apple calendar constructor must remain unreachable")
			}
			return 1, nil
		}
		result, err := sourceRunCoordinator(ctx, req)
		trace = append(trace, result.Trace...)
		return result, err
	}

	var stdout bytes.Buffer
	if err := cmdPulse(testCtx(t), []string{"--digest", "--sync", "--source", "gmail"}, &stdout, io.Discard); err != nil {
		t.Fatalf("filtered pulse: %v\n%s", err, stdout.String())
	}
	want := []string{"planned:gmail", "constructed:gmail", "started:gmail", "completed:gmail"}
	if !reflect.DeepEqual(trace, want) {
		t.Fatalf("execution trace = %v, want %v", trace, want)
	}
	if !strings.Contains(stdout.String(), "Requested Gmail evidence") {
		t.Fatalf("filtered pulse omitted Gmail evidence:\n%s", stdout.String())
	}
}

func TestIsolationIssue381GmailDigestExcludesAppleCalendar(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{
		{Name: "gmail", Type: "gmail", Enabled: genericutil.Ptr(true)},
		{Name: "calendar", Type: "calendar", Enabled: genericutil.Ptr(true)},
		{Name: "applecalendar", Type: "applecalendar", Enabled: genericutil.Ptr(true)},
	}); err != nil {
		t.Fatal(err)
	}

	orig := sourceRunCoordinatorFn
	t.Cleanup(func() { sourceRunCoordinatorFn = orig })
	var ran []string
	sourceRunCoordinatorFn = func(ctx context.Context, req sourceRunRequest) (sourceRunResult, error) {
		req.run = func(_ context.Context, _ Config, source Source, _ io.Writer) (int, error) {
			ran = append(ran, instanceKeyForSource(source))
			if source.Type != "gmail" {
				return 0, errors.New("excluded provider sibling was accessed")
			}
			return 0, nil
		}
		return sourceRunCoordinator(ctx, req)
	}

	var stdout bytes.Buffer
	if err := cmdPulse(testCtx(t), []string{"--digest", "--sync", "--source", "gmail"}, &stdout, io.Discard); err != nil {
		t.Fatalf("filtered pulse: %v", err)
	}
	if !reflect.DeepEqual(ran, []string{"gmail"}) {
		t.Fatalf("constructed/started sources = %v, want only gmail", ran)
	}
}

func TestIsolationSourceContextReachesIngest(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{{Name: "gmail", Type: "gmail", Enabled: genericutil.Ptr(true)}}); err != nil {
		t.Fatal(err)
	}
	type contextKey string
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.WithValue(context.Background(), contextKey("source"), "gmail"), deadline)
	defer cancel()
	seen := false
	_, err := sourceRunCoordinator(ctx, sourceRunRequest{
		Config: cfg,
		run: func(runCtx context.Context, _ Config, _ Source, _ io.Writer) (int, error) {
			gotDeadline, ok := runCtx.Deadline()
			seen = ok && runCtx.Value(contextKey("source")) == "gmail" && gotDeadline.Equal(deadline)
			return 0, nil
		},
	})
	if err != nil || !seen {
		t.Fatalf("context reached source=%v err=%v", seen, err)
	}
}

func TestIsolationCancelledSourceReturnsTypedOutcome(t *testing.T) {
	plans := []sourceRunPlan{{Key: "done"}, {Key: "cancelled"}, {Key: "timeout"}}
	outcomes := []sourceRunOutcome{
		{Key: "done", Items: 2}, {Key: "cancelled", Cancelled: true}, {Key: "timeout", TimedOut: true},
	}
	aggregate, err := aggregateSourceRuns(plans, outcomes, nil, nil)
	if err == nil || aggregate.Status != sourceRunStatusCancelled {
		t.Fatalf("aggregate=%+v err=%v", aggregate, err)
	}
	bySource := map[string]sourceRunReceipt{}
	for _, receipt := range aggregate.Sources {
		bySource[receipt.Source] = receipt
	}
	if !bySource["done"].Usable || bySource["done"].Items != 2 {
		t.Fatalf("completed sibling changed=%+v", bySource["done"])
	}
	if bySource["cancelled"].Status != sourceRunStatusCancelled {
		t.Fatalf("cancelled receipt=%+v", bySource["cancelled"])
	}
	if timeout := bySource["timeout"]; timeout.Status != sourceRunStatusFailed || timeout.ErrorCode != errCodeConnectorUnavailable || !timeout.Retryable {
		t.Fatalf("timeout receipt=%+v", timeout)
	}
}

func TestIsolationFilteredPulseWithoutSyncConstructsNothing(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	orig := sourceRunCoordinatorFn
	t.Cleanup(func() { sourceRunCoordinatorFn = orig })
	called := false
	sourceRunCoordinatorFn = func(context.Context, sourceRunRequest) (sourceRunResult, error) {
		called = true
		return sourceRunResult{}, nil
	}
	var stdout bytes.Buffer
	if err := cmdPulse(testCtx(t), []string{"--digest", "--source", "gmail"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("pulse without --sync must not construct or coordinate a source")
	}
}
