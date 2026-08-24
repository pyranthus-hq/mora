package mora

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/genericutil"
)

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
