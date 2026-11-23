package mora

import (
	"fmt"
	"sort"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func listDesktopProjectRules(cfg Config, request desktopReviewFrame) desktopReviewFrameResponse {
	if request.Client != "codex" {
		return desktopReviewFrameFailure("ERR_INVALID_REQUEST", "invalid project rule client")
	}
	bindings, err := loadProjectBindings(cfg)
	if err != nil {
		return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rule bindings are unavailable")
	}
	if request.ProjectDir != "" {
		binding, found, err := lookupProjectBinding(cfg, request.ProjectDir)
		if err != nil || !found {
			return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rule binding is unavailable")
		}
		bindings = []projectBinding{binding}
	}
	installation, enrolled, err := existingNativeOwnerInstallationID(cfg)
	if err != nil {
		return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rule state is unavailable")
	}
	var rows []reviewRow
	var projects []desktopReviewProject
	for _, binding := range bindings {
		projects = append(projects, desktopReviewProject{Root: binding.Dir, Scope: binding.Scope})
		mems, err := listMemories(cfg, binding.Scope, 0)
		if err != nil {
			return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rules are unavailable")
		}
		selected := map[string]reviewRow{}
		for _, m := range mems {
			if _, err := loadProjectRuleRevision(cfg, binding, m.ID); err == nil {
				selected[m.ID] = reviewRow{ID: m.ID, Kind: desktopReviewProjectRule, Scope: m.Scope, Type: m.Type, Title: m.Title}
			}
		}
		if enrolled {
			decisions, err := latestProjectRuleDecisions(cfg, binding, installation, "codex")
			if err != nil {
				return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rule state is unavailable")
			}
			for id := range decisions {
				if _, found := selected[id]; !found {
					selected[id] = reviewRow{ID: id, Kind: desktopReviewProjectRule, Scope: binding.Scope, Title: "Previously activated rule"}
				}
			}
		}
		ids := make([]string, 0, len(selected))
		for id := range selected {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			rows = append(rows, selected[id])
		}
	}
	return desktopReviewFrameResponse{Projects: projects, Rules: rows}
}

func showDesktopProjectRule(cfg Config, request desktopReviewFrame) desktopReviewFrameResponse {
	if request.Client != "codex" || (request.Action != "" && request.Action != desktopReviewActivate && request.Action != desktopReviewRetire) {
		return desktopReviewFrameFailure("ERR_INVALID_REQUEST", "invalid project rule preview")
	}
	binding, found, err := lookupProjectBinding(cfg, request.ProjectDir)
	if err != nil || !found {
		return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rule binding is unavailable")
	}
	_, loader, _, err := selectCodexProjectInstructions(binding)
	if err != nil {
		return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rule loader is unavailable")
	}
	installation, enrolled, err := existingNativeOwnerInstallationID(cfg)
	if err != nil {
		return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rule state is unavailable")
	}
	var latest projectRuleDecision
	var decided bool
	if enrolled {
		latest, decided, err = latestProjectRuleDecision(cfg, binding, installation, "codex", request.ID)
		if err != nil {
			return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rule state is unavailable")
		}
	}
	rule, sourceErr := loadProjectRuleRevision(cfg, binding, request.ID)
	// A narrowing retirement remains reviewable when its source is gone or changed.
	retiring := request.Action == desktopReviewRetire || (request.Action == "" && sourceErr != nil && decided)
	display := desktopReviewDisplay{ID: request.ID, Kind: desktopReviewProjectRule, Scope: binding.Scope, Complete: true, Details: []desktopReviewField{{Label: "project root", Value: binding.Dir}, {Label: "loader", Value: loader}}}
	var revision string
	if retiring {
		if !decided {
			return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rule activation is unavailable")
		}
		revision = latest.RevisionSHA256
		display.Title, display.State = "Previously activated rule", "active"
		if latest.Action == "retire" {
			display.State = "retired"
		}
		display.Text = "The previously approved source is unavailable or has changed. Retirement removes this exact prior activation; it does not approve the current source."
		if sourceErr == nil && rule.RevisionSHA256 == revision {
			display.Text = rule.Text
		}
		display.Details = append(display.Details, desktopReviewField{Label: "approved revision", Value: revision})
	} else {
		if sourceErr != nil {
			return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rule is unavailable")
		}
		m, err := findMemory(cfg, rule.SourceMemoryID)
		if err != nil {
			return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rule is unavailable")
		}
		revision = rule.RevisionSHA256
		display.Title, display.Text, display.Type, display.Source, display.Provenance = m.Title, rule.Text, m.Type, m.Source, rule.Provenance
		display.State = "owner_confirmed"
		if decided && latest.RevisionSHA256 == revision {
			display.State = "active"
			if latest.Action == "retire" {
				display.State = "retired"
			}
		}
		active := []projectRuleRevision(nil)
		if enrolled {
			active, err = activeProjectRuleRevisions(cfg, binding, installation, "codex")
			if err != nil {
				return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rule state is unavailable")
			}
		}
		seen := false
		for _, current := range active {
			seen = seen || (current.RuleID == rule.RuleID && current.RevisionSHA256 == rule.RevisionSHA256)
		}
		if !seen {
			active = append(active, rule)
		}
		_, _, omitted := renderProjectRuleRegion(active)
		display.Details = append(display.Details, desktopReviewField{Label: "omitted rules", Value: fmt.Sprint(omitted)})
	}
	if decided {
		display.Details = append(display.Details, desktopReviewField{Label: "activation state", Value: latest.Action}, desktopReviewField{Label: "activation revision", Value: latest.RevisionSHA256})
	}
	if display.Provenance == "" {
		display.Provenance = memory.ProvenanceAuthored
	}
	action := desktopReviewActivate
	if retiring {
		action = desktopReviewRetire
	}
	display.Details = append(display.Details, desktopReviewField{Label: "action", Value: string(action)})
	preview, err := newDesktopReviewPreview(display, revision)
	if err != nil {
		return desktopReviewFrameFailure("ERR_REVIEW_UNAVAILABLE", "project rule preview is unavailable")
	}
	return desktopReviewFrameResponse{Preview: &preview}
}
