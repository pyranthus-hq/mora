package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
)

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
