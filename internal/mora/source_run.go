package mora

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

type sourceRunRequest struct {
	Config   Config
	Selector string
	Filtered bool
	Output   io.Writer
	run      func(context.Context, Config, Source, io.Writer) (int, error)
}

type sourceRunPlan struct {
	Key    string
	Source Source
}

type sourceRunNotice struct {
	Source string `json:"source"`
	Status string `json:"status"`
}

type sourceRunResult struct {
	Plans     []sourceRunPlan
	Notices   []sourceRunNotice
	Trace     []string
	Items     int
	Failures  int
	Outcomes  []sourceRunOutcome
	Aggregate sourceRunAggregate
}

const defaultSourceRunConcurrency = 4
const defaultSourceTimeout = 15 * time.Minute

type sourceRunOptions struct {
	Concurrency   int
	SourceTimeout time.Duration
	Run           func(context.Context, sourceRunPlan) sourceRunOutcome
}

type synchronizedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *synchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

const (
	sourceRunStatusSuccess   = "success"
	sourceRunStatusPartial   = "partial"
	sourceRunStatusEmpty     = "empty"
	sourceRunStatusFailed    = "failed"
	sourceRunStatusCancelled = "cancelled"
)

type sourceRunOutcome struct {
	Key           string
	Items         int
	Examined      int
	Materialized  int
	Failed        int
	Unchanged     int
	Missing       int
	Err           error
	Cancelled     bool
	TimedOut      bool
	LastSuccessAt string
	LastAttemptAt string
	Stale         bool
	Stages        memory.IngestStages
	Incremental   bool
}

type sourceRunReceipt struct {
	Source        string              `json:"source"`
	Status        string              `json:"status"`
	Usable        bool                `json:"usable"`
	Items         int                 `json:"items"`
	Examined      int                 `json:"examined"`
	Materialized  int                 `json:"materialized"`
	Failed        int                 `json:"failed"`
	Unchanged     int                 `json:"unchanged"`
	Missing       int                 `json:"missing"`
	ErrorCode     string              `json:"error_code,omitempty"`
	ErrorClass    string              `json:"error_class,omitempty"`
	Retryable     bool                `json:"retryable"`
	LastSuccessAt string              `json:"last_success_at,omitempty"`
	LastAttemptAt string              `json:"last_attempt_at,omitempty"`
	Stale         bool                `json:"stale"`
	Stages        memory.IngestStages `json:"stages"`
	Incremental   bool                `json:"incremental"`
}

type sourceRunAggregate struct {
	Status            string             `json:"status"`
	Usable            bool               `json:"usable"`
	SuccessfulSources int                `json:"successful_sources"`
	FailedSources     int                `json:"failed_sources"`
	CancelledSources  int                `json:"cancelled_sources"`
	Sources           []sourceRunReceipt `json:"sources"`
	SystemNotices     []sourceRunNotice  `json:"system_notices,omitempty"`
}

func aggregateSourceRuns(plans []sourceRunPlan, outcomes []sourceRunOutcome, notices []sourceRunNotice, infrastructureErr error) (sourceRunAggregate, error) {
	byKey := make(map[string]sourceRunOutcome, len(outcomes))
	for _, outcome := range outcomes {
		byKey[outcome.Key] = outcome
	}
	receipts := make([]sourceRunReceipt, 0, len(plans))
	for _, plan := range plans {
		outcome, found := byKey[plan.Key]
		if !found {
			outcome = sourceRunOutcome{Key: plan.Key, Err: errors.New("source produced no terminal outcome")}
		}
		receipt := sourceRunReceipt{
			Source: plan.Key, Items: outcome.Items, Examined: outcome.Examined,
			Materialized: outcome.Materialized, Failed: outcome.Failed, Unchanged: outcome.Unchanged, Missing: outcome.Missing,
			LastSuccessAt: outcome.LastSuccessAt,
			LastAttemptAt: outcome.LastAttemptAt, Stale: outcome.Stale,
			Stages:      outcome.Stages,
			Incremental: outcome.Incremental,
		}
		switch {
		case outcome.Cancelled:
			receipt.Status = sourceRunStatusCancelled
		case outcome.TimedOut:
			receipt.Status = sourceRunStatusFailed
			receipt.ErrorCode = connectorErrorCode(context.DeadlineExceeded)
			receipt.ErrorClass = connectorErrorClassOf(receipt.ErrorCode)
			receipt.Retryable = retryableForErrorCode(receipt.ErrorCode)
		case outcome.Err != nil && outcome.Materialized > 0:
			receipt.Status = sourceRunStatusPartial
			receipt.Usable = true
			receipt.ErrorCode = connectorErrorCodeFor(outcome.Err)
			receipt.ErrorClass = connectorErrorClassOf(receipt.ErrorCode)
			receipt.Retryable = retryableForErrorCode(receipt.ErrorCode)
		case outcome.Err != nil:
			receipt.Status = sourceRunStatusFailed
			receipt.ErrorCode = connectorErrorCodeFor(outcome.Err)
			receipt.ErrorClass = connectorErrorClassOf(receipt.ErrorCode)
			receipt.Retryable = retryableForErrorCode(receipt.ErrorCode)
		case outcome.Incremental || outcome.Unchanged > 0:
			receipt.Status = sourceRunStatusSuccess
			receipt.Usable = true
		case outcome.Items == 0:
			receipt.Status = sourceRunStatusEmpty
			receipt.Usable = true
			receipt.ErrorCode = errCodeConnectorEmpty
			receipt.ErrorClass = connectorErrorClassOf(receipt.ErrorCode)
			receipt.Retryable = retryableForErrorCode(receipt.ErrorCode)
		default:
			receipt.Status = sourceRunStatusSuccess
			receipt.Usable = true
		}
		receipts = append(receipts, receipt)
	}
	aggregate := sourceRunAggregate{Sources: receipts, SystemNotices: notices}
	for _, receipt := range receipts {
		switch receipt.Status {
		case sourceRunStatusSuccess, sourceRunStatusEmpty:
			aggregate.SuccessfulSources++
		case sourceRunStatusPartial:
			// A partial source contributed usable materialized work and also had a
			// failed attempt. Preserve both aggregate facts; the source receipt is
			// still the canonical one-per-source terminal classification.
			aggregate.SuccessfulSources++
			aggregate.FailedSources++
		case sourceRunStatusFailed:
			aggregate.FailedSources++
		case sourceRunStatusCancelled:
			aggregate.CancelledSources++
		}
	}
	aggregate.Usable = aggregate.SuccessfulSources > 0 || len(receipts) == 0
	switch {
	case infrastructureErr != nil:
		aggregate.Status = sourceRunStatusFailed
		aggregate.Usable = false
		for i := range aggregate.Sources {
			aggregate.Sources[i].Usable = false
		}
		return aggregate, infrastructureErr
	case aggregate.CancelledSources > 0:
		aggregate.Status = sourceRunStatusCancelled
		return aggregate, fmt.Errorf("source run cancelled")
	case aggregate.FailedSources > 0 && aggregate.SuccessfulSources > 0 || hasPartialSource(receipts):
		aggregate.Status = sourceRunStatusPartial
		return aggregate, nil
	case aggregate.FailedSources > 0:
		aggregate.Status = sourceRunStatusFailed
		return aggregate, fmt.Errorf("every requested source failed")
	default:
		aggregate.Status = sourceRunStatusSuccess
		return aggregate, nil
	}
}

func hasPartialSource(receipts []sourceRunReceipt) bool {
	for _, receipt := range receipts {
		if receipt.Status == sourceRunStatusPartial {
			return true
		}
	}
	return false
}

type sourceRunCoordinatorFunc func(context.Context, sourceRunRequest) (sourceRunResult, error)

var sourceRunCoordinatorFn sourceRunCoordinatorFunc = sourceRunCoordinator

// runSourcePlan executes immutable plan slots through a bounded worker pool.
// Slots begin cancelled and are replaced only by a terminal worker outcome, so
// global cancellation accounts for both started and never-started sources in
// plan order. The function does not return until every started worker has exited.
func runSourcePlan(ctx context.Context, plans []sourceRunPlan, opts sourceRunOptions) []sourceRunOutcome {
	results := make([]sourceRunOutcome, len(plans))
	for i := range plans {
		results[i] = sourceRunOutcome{Key: plans[i].Key, Cancelled: true, Err: context.Canceled}
	}
	if len(plans) == 0 || opts.Run == nil {
		return results
	}
	workers := opts.Concurrency
	if workers <= 0 {
		workers = defaultSourceRunConcurrency
	}
	if workers > len(plans) {
		workers = len(plans)
	}
	timeout := opts.SourceTimeout
	if timeout <= 0 {
		timeout = defaultSourceTimeout
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					continue
				}
				sourceCtx, cancel := context.WithTimeout(ctx, timeout)
				outcome := opts.Run(sourceCtx, plans[index])
				sourceErr := sourceCtx.Err()
				cancel()
				outcome.Key = plans[index].Key
				switch {
				case ctx.Err() != nil:
					outcome.Cancelled = true
					outcome.TimedOut = false
					outcome.Err = ctx.Err()
				case errors.Is(sourceErr, context.DeadlineExceeded):
					outcome.TimedOut = true
					outcome.Err = context.DeadlineExceeded
				}
				results[index] = outcome
			}
		}()
	}

schedule:
	for index := range plans {
		select {
		case jobs <- index:
		case <-ctx.Done():
			break schedule
		}
	}
	close(jobs)
	wg.Wait()
	return results
}

func planSourceRuns(cfg Config, selector string, filtered bool, now time.Time) ([]sourceRunPlan, []sourceRunNotice, error) {
	selected := ""
	if filtered {
		filters, err := parseSearchFilters(map[string]any{"source": selector}, now)
		if err != nil {
			return nil, nil, err
		}
		selected = filters.NormalizedSource()
	}

	sources, err := loadSources(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("load sources: %w", err)
	}
	byKey := make(map[string]Source)
	for _, source := range sources {
		if !source.IsEnabled() {
			continue
		}
		key := instanceKeyForSource(source)
		if filtered && !digestSourceMatches(key, selected) {
			continue
		}
		if _, exists := byKey[key]; !exists {
			byKey[key] = source
		}
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	plans := make([]sourceRunPlan, 0, len(keys))
	for _, key := range keys {
		plans = append(plans, sourceRunPlan{Key: key, Source: byKey[key]})
	}

	var notices []sourceRunNotice
	if filtered {
		for _, health := range sourceHealthAll(cfg, now) {
			if digestSourceMatches(health.Key, selected) || health.State == healthFresh {
				continue
			}
			notices = append(notices, sourceRunNotice{Source: health.Key, Status: health.State})
		}
	}
	return plans, notices, nil
}

func sourceRunCoordinator(ctx context.Context, req sourceRunRequest) (sourceRunResult, error) {
	plans, notices, err := planSourceRuns(req.Config, req.Selector, req.Filtered, briefClock())
	result := sourceRunResult{Plans: plans, Notices: notices}
	if err != nil {
		return result, err
	}
	for _, plan := range plans {
		result.Trace = append(result.Trace, "planned:"+plan.Key)
	}
	run := req.run
	output := req.Output
	if output != nil {
		output = &synchronizedWriter{w: output}
	}
	if run == nil {
		run = func(runCtx context.Context, cfg Config, source Source, output io.Writer) (int, error) {
			result, err := ingestSourceFn(runCtx, cfg, source, output)
			return result.Materialized, err
		}
	}
	var traceMu sync.Mutex
	result.Outcomes = runSourcePlan(ctx, plans, sourceRunOptions{Run: func(runCtx context.Context, plan sourceRunPlan) sourceRunOutcome {
		traceMu.Lock()
		result.Trace = append(result.Trace, "constructed:"+plan.Key, "started:"+plan.Key)
		traceMu.Unlock()
		n, runErr := run(runCtx, req.Config, plan.Source, output)
		outcome := sourceRunOutcome{Key: plan.Key, Items: n, Materialized: n, Err: runErr}
		traceMu.Lock()
		result.Items += n
		if runErr != nil {
			result.Failures++
			warnf(output, "%s sync incomplete; the brief reflects last good data (run `mora sync status`): %v", plan.Key, runErr)
		} else {
			result.Trace = append(result.Trace, "completed:"+plan.Key)
		}
		traceMu.Unlock()
		return outcome
	}})
	var rebuildErr error
	if sourceOutcomesMaterialized(result.Outcomes) > 0 && ctx.Err() == nil {
		_, rebuildErr = rebuildIndex(ctx, req.Config)
	}
	for _, notice := range notices {
		warnf(output, "source health notice: %s is %s", notice.Source, notice.Status)
	}
	result.Aggregate, err = aggregateSourceRuns(plans, result.Outcomes, notices, rebuildErr)
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, err
}

func sourceOutcomesMaterialized(outcomes []sourceRunOutcome) int {
	total := 0
	for _, outcome := range outcomes {
		total += outcome.Materialized
	}
	return total
}
