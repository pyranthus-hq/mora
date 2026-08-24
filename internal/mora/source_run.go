package mora

import (
	"context"
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
