package mora

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestIngestRunAllPartialSuccessOneRebuild(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{
		{Name: "bad", Type: "filesystem", Path: t.TempDir(), Enabled: genericutil.Ptr(true)},
		{Name: "good", Type: "filesystem", Path: t.TempDir(), Enabled: genericutil.Ptr(true)},
	}); err != nil {
		t.Fatal(err)
	}
	origIngest, origRebuild := ingestSourceFn, rebuildIngestIndexFn
	t.Cleanup(func() { ingestSourceFn, rebuildIngestIndexFn = origIngest, origRebuild })
	ingestSourceFn = func(_ context.Context, _ Config, source Source, _ io.Writer) (sourceIngestResult, error) {
		if source.Name == "bad" {
			return sourceIngestResult{}, newCodedError(errCodeConnectorUnavailable, nil, "offline")
		}
		return sourceIngestResult{Examined: 2, Materialized: 2}, nil
	}
	var rebuilds atomic.Int32
	rebuildIngestIndexFn = func(context.Context, Config) (int, error) {
		rebuilds.Add(1)
		return 2, nil
	}
	var output bytes.Buffer
	if err := cmdIngest(context.Background(), []string{"run", "--all", "--json"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if rebuilds.Load() != 1 {
		t.Fatalf("covering rebuilds = %d, want 1", rebuilds.Load())
	}
}

func TestIngestRunAllGlobalCancellationNonzero(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{{Name: "blocked", Type: "filesystem", Path: t.TempDir(), Enabled: genericutil.Ptr(true)}}); err != nil {
		t.Fatal(err)
	}
	original := ingestSourceFn
	t.Cleanup(func() { ingestSourceFn = original })
	ingestSourceFn = func(ctx context.Context, _ Config, _ Source, _ io.Writer) (sourceIngestResult, error) {
		<-ctx.Done()
		return sourceIngestResult{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	var output bytes.Buffer
	err := cmdIngest(ctx, []string{"run", "--all", "--json"}, &output, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cmdIngest error = %v, want context.Canceled", err)
	}
}

func TestReingestRetriesOnlyFailedSources(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	sources := []Source{
		{Name: "retry", Type: "filesystem", Path: t.TempDir(), Enabled: genericutil.Ptr(true)},
		{Name: "malformed", Type: "filesystem", Path: t.TempDir(), Enabled: genericutil.Ptr(true)},
		{Name: "healthy", Type: "filesystem", Path: t.TempDir(), Enabled: genericutil.Ptr(true)},
	}
	if err := saveSources(cfg, sources); err != nil {
		t.Fatal(err)
	}
	if err := memory.SaveStatus(syncStatusPathFor(cfg, sources[0]), &memory.SyncStatus{LastError: "offline", ErrorCode: errCodeConnectorUnavailable}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SaveStatus(syncStatusPathFor(cfg, sources[1]), &memory.SyncStatus{LastError: "bad data", ErrorCode: errCodeConnectorMalformed}); err != nil {
		t.Fatal(err)
	}
	original := ingestSourceFn
	t.Cleanup(func() { ingestSourceFn = original })
	var called []string
	ingestSourceFn = func(_ context.Context, _ Config, source Source, _ io.Writer) (sourceIngestResult, error) {
		called = append(called, source.Name)
		return sourceIngestResult{}, nil
	}
	if err := cmdReingest(context.Background(), []string{"--failed"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(called, []string{"retry"}) {
		t.Fatalf("retried sources = %v, want [retry]", called)
	}
}

func TestPulseFilteredSyncFirstUsesOnlyRequestedSources(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{
		{Name: "gmail", Type: "gmail", Enabled: genericutil.Ptr(true)},
		{Name: "applecalendar", Type: "applecalendar", Enabled: genericutil.Ptr(true)},
	}); err != nil {
		t.Fatal(err)
	}
	digestSeed(t, cfg, "gmail", "Requested Gmail evidence", time.Hour, time.Now())
	original := sourceRunCoordinatorFn
	t.Cleanup(func() { sourceRunCoordinatorFn = original })
	var keys []string
	sourceRunCoordinatorFn = func(ctx context.Context, req sourceRunRequest) (sourceRunResult, error) {
		req.run = func(_ context.Context, _ Config, source Source, _ io.Writer) (int, error) {
			keys = append(keys, instanceKeyForSource(source))
			return 0, nil
		}
		return sourceRunCoordinator(ctx, req)
	}
	var output bytes.Buffer
	if err := cmdPulse(context.Background(), []string{"--digest", "--sync", "--source", "gmail"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys, []string{"gmail"}) {
		t.Fatalf("sync keys = %v, want [gmail]", keys)
	}
}

func TestPulseAllSourcePartialSnapshotRemainsUsable(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	digestSeed(t, cfg, "gmail", "Last known good evidence", time.Hour, time.Now())
	original := sourceRunCoordinatorFn
	t.Cleanup(func() { sourceRunCoordinatorFn = original })
	sourceRunCoordinatorFn = func(context.Context, sourceRunRequest) (sourceRunResult, error) {
		return sourceRunResult{}, errors.New("one sibling unavailable")
	}
	var output bytes.Buffer
	if err := cmdPulse(context.Background(), []string{"--digest", "--sync"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Last known good evidence") {
		t.Fatalf("partial snapshot lost healthy evidence:\n%s", output.String())
	}
}
