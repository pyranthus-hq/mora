package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
)

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

func TestIsolationAggregateEdges(t *testing.T) {
	plans := []sourceRunPlan{
		{Key: "gmail", Source: Source{Name: "gmail", Type: "gmail"}},
		{Key: "imessage", Source: Source{Name: "imessage", Type: "imessage"}},
	}
	t.Run("all failed", func(t *testing.T) {
		outcomes := []sourceRunOutcome{
			{Key: "gmail", Err: newCodedError(errCodeConnectorUnavailable, nil, "offline")},
			{Key: "imessage", Err: newCodedError(errCodeConnectorMalformed, nil, "bad payload")},
		}
		aggregate, err := aggregateSourceRuns(plans, outcomes, nil, nil)
		if err == nil || aggregate.Status != sourceRunStatusFailed || aggregate.Usable || aggregate.FailedSources != 2 || len(aggregate.Sources) != 2 {
			t.Fatalf("aggregate=%+v err=%v", aggregate, err)
		}
	})
	t.Run("clean empty", func(t *testing.T) {
		aggregate, err := aggregateSourceRuns(plans[:1], []sourceRunOutcome{{Key: "gmail"}}, nil, nil)
		if err != nil || !aggregate.Usable || aggregate.FailedSources != 0 || len(aggregate.Sources) != 1 {
			t.Fatalf("aggregate=%+v err=%v", aggregate, err)
		}
		receipt := aggregate.Sources[0]
		if receipt.Status != sourceRunStatusEmpty || receipt.ErrorCode != errCodeConnectorEmpty || receipt.ErrorClass != connectorClassEmpty || receipt.Retryable {
			t.Fatalf("empty receipt = %+v", receipt)
		}
	})
	t.Run("failed zero is not empty", func(t *testing.T) {
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
		t.Run(tc.name, func(t *testing.T) {
			plans := []sourceRunPlan{{Key: "failed"}, {Key: "healthy"}}
			outcomes := []sourceRunOutcome{
				{Key: "failed", Err: newCodedError(tc.code, nil, tc.name)},
				{Key: "healthy", Items: 2},
			}
			aggregate, err := aggregateSourceRuns(plans, outcomes, nil, nil)
			if err != nil {
				t.Fatalf("healthy peer should keep result usable: %v", err)
			}
			var failed, healthy sourceRunReceipt
			for _, receipt := range aggregate.Sources {
				if receipt.Source == "failed" {
					failed = receipt
				} else if receipt.Source == "healthy" {
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
		t.Run("invalid_"+strings.ReplaceAll(selector, ":", "_"), func(t *testing.T) {
			plans, _, err := planSourceRuns(cfg, selector, true, time.Now())
			if err == nil || len(plans) != 0 {
				t.Fatalf("selector %q: plans=%v err=%v, want pre-construction failure", selector, plans, err)
			}
			var stdout bytes.Buffer
			if err := cmdPulse(context.Background(), []string{"--digest", "--sync", "--source", selector}, &stdout, io.Discard); err == nil {
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
	result, err := sourceRunCoordinator(context.Background(), sourceRunRequest{
		Config: cfg,
		run: func(Config, Source, io.Writer) (int, error) {
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
		req.run = func(_ Config, source Source, _ io.Writer) (int, error) {
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
	if err := cmdPulse(context.Background(), []string{"--digest", "--sync", "--source", "gmail"}, &stdout, io.Discard); err != nil {
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
		req.run = func(_ Config, source Source, _ io.Writer) (int, error) {
			ran = append(ran, instanceKeyForSource(source))
			if source.Type != "gmail" {
				return 0, errors.New("excluded provider sibling was accessed")
			}
			return 0, nil
		}
		return sourceRunCoordinator(ctx, req)
	}

	var stdout bytes.Buffer
	if err := cmdPulse(context.Background(), []string{"--digest", "--sync", "--source", "gmail"}, &stdout, io.Discard); err != nil {
		t.Fatalf("filtered pulse: %v", err)
	}
	if !reflect.DeepEqual(ran, []string{"gmail"}) {
		t.Fatalf("constructed/started sources = %v, want only gmail", ran)
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
	if err := cmdPulse(context.Background(), []string{"--digest", "--source", "gmail"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("pulse without --sync must not construct or coordinate a source")
	}
}
