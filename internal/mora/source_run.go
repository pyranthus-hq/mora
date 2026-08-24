package mora

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

type sourceRunRequest struct {
	Config   Config
	Selector string
	Filtered bool
	Output   io.Writer
	run      func(Config, Source, io.Writer) (int, error)
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
	Plans    []sourceRunPlan
	Notices  []sourceRunNotice
	Trace    []string
	Items    int
	Failures int
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
	Err           error
	Cancelled     bool
	LastSuccessAt string
	LastAttemptAt string
	Stale         bool
}

type sourceRunReceipt struct {
	Source        string `json:"source"`
	Status        string `json:"status"`
	Usable        bool   `json:"usable"`
	Items         int    `json:"items"`
	ErrorCode     string `json:"error_code,omitempty"`
	ErrorClass    string `json:"error_class,omitempty"`
	Retryable     bool   `json:"retryable"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	Stale         bool   `json:"stale"`
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
			Source: plan.Key, Items: outcome.Items, LastSuccessAt: outcome.LastSuccessAt,
			LastAttemptAt: outcome.LastAttemptAt, Stale: outcome.Stale,
		}
		switch {
		case outcome.Cancelled:
			receipt.Status = sourceRunStatusCancelled
		case outcome.Err != nil:
			receipt.Status = sourceRunStatusFailed
			receipt.ErrorCode = connectorErrorCodeFor(outcome.Err)
			receipt.ErrorClass = connectorErrorClassOf(receipt.ErrorCode)
			receipt.Retryable = retryableForErrorCode(receipt.ErrorCode)
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
	sort.Slice(receipts, func(i, j int) bool { return receipts[i].Source < receipts[j].Source })

	aggregate := sourceRunAggregate{Sources: receipts, SystemNotices: notices}
	for _, receipt := range receipts {
		switch receipt.Status {
		case sourceRunStatusSuccess, sourceRunStatusEmpty:
			aggregate.SuccessfulSources++
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
	case aggregate.FailedSources > 0 && aggregate.SuccessfulSources > 0:
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

type sourceRunCoordinatorFunc func(context.Context, sourceRunRequest) (sourceRunResult, error)

var sourceRunCoordinatorFn sourceRunCoordinatorFunc = sourceRunCoordinator

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
	if run == nil {
		run = ingestSourceFn
	}
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Trace = append(result.Trace, "constructed:"+plan.Key, "started:"+plan.Key)
		n, runErr := run(req.Config, plan.Source, req.Output)
		result.Items += n
		if runErr != nil {
			result.Failures++
			warnf(req.Output, "%s sync incomplete; the brief reflects last good data (run `mora sync status`): %v", plan.Key, runErr)
			continue
		}
		result.Trace = append(result.Trace, "completed:"+plan.Key)
	}
	if _, rebuildErr := rebuildIndex(ctx, req.Config); rebuildErr != nil {
		return result, rebuildErr
	}
	if result.Failures > 0 {
		return result, fmt.Errorf("%d source(s) failed to sync; data may be stale (run `mora sync status`)", result.Failures)
	}
	return result, nil
}
