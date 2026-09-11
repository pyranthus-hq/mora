package mora

// companion_read.go is the CLI surface of the companion reader (graph node
// N12's third caller, alongside the phone listener and the tests). The
// desktop app drives these three read verbs directly against the same
// companionReader the loopback listener uses, so the CLI and the phone cannot
// drift: each verb emits exactly the document the listener serves on
// /v1/companion/<verb>.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode"

	"github.com/pyranthus-hq/mora/internal/readable"
	"github.com/pyranthus-hq/mora/internal/segments"

	"github.com/pyranthus-hq/mora/internal/companion"
)

const companionHealthUsage = "usage: mora companion health [--json]"
const companionTodayUsage = "usage: mora companion today [--json]"
const companionContextUsage = "usage: mora companion context --mode <think|search|meeting_prep> --query <text> [--scope <scope>] --json"

func cmdCompanionHealth(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("companion health", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"%s (unexpected argument %q)", companionHealthUsage, fs.Arg(0))
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	out, err := newCompanionReader(cfg).Health(ctx)
	if err != nil {
		return newCodedError(errCodeInternalUnexpected, err, "companion health: %v", err)
	}
	if *jsonOut {
		return emitCompanionDocument(stdout, struct {
			companion.HealthProjection
			ReadsInFlight []connectReadInFlight `json:"reads_in_flight"`
		}{out, connectReadsInFlight(cfg, cfg.OperationClock())})
	}
	fmt.Fprintf(stdout, "state\t%s\n", out.State)
	fmt.Fprintf(stdout, "policy\t%s\n", out.Policy)
	fmt.Fprintf(stdout, "index\t%s\t%d memories\n", out.Index.State, out.Index.Memories)
	for _, s := range out.Sources {
		fmt.Fprintf(stdout, "source\t%s\t%s\n", s.Key, s.State)
	}
	return nil
}

func cmdCompanionToday(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("companion today", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"%s (unexpected argument %q)", companionTodayUsage, fs.Arg(0))
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	out, err := newCompanionReader(cfg).Today(ctx)
	if err != nil {
		return newCodedError(errCodeInternalUnexpected, err, "companion today: %v", err)
	}
	if *jsonOut {
		return emitCompanionDocument(stdout, out)
	}
	for _, item := range out.Items {
		fmt.Fprintf(stdout, "%s\t%s\n", item.Kind, item.Title)
	}
	if out.Truncated {
		fmt.Fprintln(stdout, "truncated\ttrue")
	}
	return nil
}

func cmdCompanionContext(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("companion context", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	mode := fs.String("mode", "", "think, search or meeting_prep")
	query := fs.String("query", "", "the question or search text")
	scope := fs.String("scope", "", "optional memory scope")
	jsonOut := fs.Bool("json", false, "emit JSON (required)")
	if err := fs.Parse(args); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"%s (unexpected argument %q)", companionContextUsage, fs.Arg(0))
	}
	if !*jsonOut {
		return newCodedError(errCodeUsageMissingArgument, nil,
			"%s (--json is required; context has no human rendering)", companionContextUsage)
	}
	if *query == "" {
		return newCodedError(errCodeUsageMissingArgument, nil, "%s (--query is required)", companionContextUsage)
	}
	req := companion.NewContextRequest()
	req.Mode = companion.ContextMode(*mode)
	req.Query = *query
	req.Scope = *scope
	if err := req.Validate(); err != nil {
		return newCodedError(errCodeUsageUnknownValue, err, "%s (%v)", companionContextUsage, err)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	out, err := newCompanionReader(cfg).Context(ctx, req)
	if err != nil {
		return newCodedError(errCodeInternalUnexpected, err, "companion context: %v", err)
	}
	return emitCompanionDocument(stdout, out)
}

// emitCompanionDocument prints one wire document. The envelope is the
// projection's own Header, so emitReceipt (which stamps a CLI receipt schema)
// is deliberately not used here.
func emitCompanionDocument(w io.Writer, doc any) error {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

func companionSourceLabels(cfg Config, rows []companion.SourceFreshness) []companion.SourceCoverage {
	labels := map[string]string{}
	for _, s := range loadSourcesOrEmpty(cfg) {
		labels[companionSourceKey(instanceKeyForSource(s))] = sourceLabel(s)
	}
	out := make([]companion.SourceCoverage, 0, len(rows))
	for _, row := range rows {
		label, ok := labels[row.Key]
		if !ok {
			label = row.Key
		}
		out = append(out, companion.SourceCoverage{SourceFreshness: row, Label: companionText(label, companion.MaxLabelBytes)})
	}
	return out
}

// companionTodayDocument completes the shared document before validation.
func companionTodayDocument(cfg Config, out companion.TodayProjection) companion.TodayProjection {
	out.Coverage = companionSourceLabels(cfg, out.Freshness)
	for i := range out.Items {
		if len(out.Items[i].Evidence) > 0 {
			out.Items[i].Snippet = companionText(out.Items[i].Evidence[0].Snippet, companion.MaxSnippetBytes)
		}
	}
	return out
}

// An absent or invalid message projection has no eligible incoming row; never
// substitute the raw transcript, which could quote self or an attachment.
func companionReadingSnippet(m Memory, fallback string) string {
	text := fallback
	kind := m.Provider
	if kind == "" {
		kind = m.Type
	}
	switch kind {
	case "imessage", "whatsapp":
		text = ""
		rows, _ := segments.Derive(m)
		for i := len(rows) - 1; i >= 0; i-- {
			row := rows[i]
			if segments.Direction(row.BlockRefs) != "incoming" {
				continue
			}
			candidate := strings.TrimSpace(row.Text)
			if strings.TrimSpace(strings.ReplaceAll(candidate, "[attachment]", "")) == "" {
				continue
			}
			text = candidate
			break
		}
	case "gmail":
		text = ""
		for _, paragraph := range strings.Split(readable.Email(m.Text).Text, "\n\n") {
			paragraph = strings.TrimSpace(paragraph)
			if strings.IndexFunc(paragraph, unicode.IsLetter) < 0 {
				continue
			}
			u, err := url.Parse(paragraph)
			if err == nil && u.Scheme != "" && !strings.ContainsAny(paragraph, " \t\n") {
				continue
			}
			text = paragraph
			break
		}
	}
	text = strings.TrimSpace(text)
	for strings.HasPrefix(text, "…") || strings.HasPrefix(text, "...") {
		text = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "…"), "..."))
	}
	if kind == "imessage" || kind == "whatsapp" || kind == "gmail" {
		text, _ = readable.Bound(text, 220)
	}
	return text
}
