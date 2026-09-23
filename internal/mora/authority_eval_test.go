package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	configstore "github.com/pyranthus-hq/mora/internal/config"
)

// The authority eval measures one thing: what a read surface tells an agent
// about where a claim came from and who stands behind it.
//
// Each case names a query, the records that ought to lead, the records that
// ought to follow, and the labels a human gave those records after reading the
// primary evidence. The harness runs the real search_memory and context_memory
// handlers and compares what they emit against those labels. It never writes a
// memory, never reorders results, and never decides that a record is true. A
// failing check means the surface said more, or less, than the label supports.
//
// The live case file is gitignored because it names real records. The synthetic
// fixtures under testdata/authority exercise the same loader and the same five
// checks on a temporary vault, so the harness itself stays under test.

// Check names, so the report and the test log agree on one vocabulary.
const (
	authorityCheckRank    = "RANK"
	authorityCheckLater   = "LATER"
	authorityCheckProv    = "PROV"
	authorityCheckAdopted = "ADOPTED"
	authorityCheckAssert  = "ASSERT"
)

const (
	authoritySurfaceSearch  = "search_memory"
	authoritySurfaceContext = "context_memory"
)

// Wording the read surfaces use for adoption. The harness matches on these
// exact phrases so a reworded surface fails loudly instead of quietly passing.
const (
	authorityAdoptedUnchecked = "no record either way"
	authorityAdoptedReported  = "reported by the writer, unchecked"
)

// authorityCase is one hand-labelled retrieval case. Every field is a human
// judgement made after reading the primary evidence, never a model output.
type authorityCase struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Query    string   `json:"query"`
	Surfaces []string `json:"surfaces"`
	// Prefer lists the records a correct answer should lean on; Demote lists
	// the records that should not lead. Both may be empty.
	Prefer []string `json:"prefer"`
	Demote []string `json:"demote"`
	// MustNotAssert holds phrases the surface must not present as settled,
	// unless the record carrying the phrase also points at a later record.
	MustNotAssert []string `json:"must_not_assert"`
	// Provenance maps a record id to the origin label a human gave it:
	// evidence, document, or authored.
	Provenance map[string]string `json:"provenance"`
	// DecisionAdopted maps a decision record id to what the label says about
	// user adoption: unverified, reported-by-writer, or user.
	DecisionAdopted map[string]string `json:"decision_adopted"`
	// AttributionSupported records whether the labeller found the user's own
	// words behind the claim. The harness reports it; no check reads it yet.
	AttributionSupported *bool  `json:"attribution_supported"`
	LabelSource          string `json:"label_source"`
	// Evidence is what the labeller read before labelling: either a plain note
	// or a record reference with a path and an excerpt. The harness keeps it
	// verbatim so a human can retrace a label; no check reads it.
	Evidence   []json.RawMessage `json:"evidence"`
	Notes      string            `json:"notes"`
	SkipReason string            `json:"skip_reason"`
}

var authorityProvenanceKinds = map[string]bool{"evidence": true, "document": true, "authored": true}

var authorityAdoptionLabels = map[string]bool{"unverified": true, "reported-by-writer": true, "user": true}

// loadAuthorityCases reads the labelled case file. It is strict on purpose: a
// typo in a label would otherwise turn into a silent pass.
func loadAuthorityCases(path string) ([]authorityCase, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []authorityCase
	seen := map[string]bool{}
	for i, line := range strings.Split(string(raw), "\n") {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var c authorityCase
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		if err := validateAuthorityCase(c); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("%s line %d: duplicate case id %q", path, i+1, c.ID)
		}
		seen[c.ID] = true
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("%s: no cases", path)
	}
	return cases, nil
}

func validateAuthorityCase(c authorityCase) error {
	if c.ID == "" || c.Kind == "" || c.Query == "" {
		return fmt.Errorf("case needs id, kind and query")
	}
	if len(c.Surfaces) == 0 {
		return fmt.Errorf("case %q names no surface", c.ID)
	}
	for _, s := range c.Surfaces {
		if s != authoritySurfaceSearch && s != authoritySurfaceContext {
			return fmt.Errorf("case %q: unknown surface %q", c.ID, s)
		}
	}
	for id, kind := range c.Provenance {
		if !authorityProvenanceKinds[kind] {
			return fmt.Errorf("case %q: record %q has unknown provenance label %q", c.ID, id, kind)
		}
	}
	for id, adopted := range c.DecisionAdopted {
		if !authorityAdoptionLabels[adopted] {
			return fmt.Errorf("case %q: record %q has unknown adoption label %q", c.ID, id, adopted)
		}
	}
	if c.LabelSource == "" {
		return fmt.Errorf("case %q: label_source is required, so a reader can tell who labelled it", c.ID)
	}
	return nil
}

// authorityRow is one record as a surface actually emitted it.
type authorityRow struct {
	ID string
	// Provenance is the origin kind the surface showed, or empty when it
	// showed none.
	Provenance string
	// LaterID is the record id named by a later-related pointer, or empty.
	LaterID string
	// Adopted is the adoption wording the surface printed, or empty.
	Adopted string
	// Block is the emitted text for this record, on the surfaces that emit prose.
	Block string
}

// authorityView is what one surface emitted for one case.
type authorityView struct {
	Rows []authorityRow
	Text string
}

func (v authorityView) rank(id string) int {
	for i, r := range v.Rows {
		if r.ID == id {
			return i
		}
	}
	return -1
}

func (v authorityView) row(id string) (authorityRow, bool) {
	for _, r := range v.Rows {
		if r.ID == id {
			return r, true
		}
	}
	return authorityRow{}, false
}

// authorityCheck is one pass or fail with the reason in plain English.
// Applicable is false when the case could not exercise the check at all,
// usually because no labelled record came back. Those rows still count as a
// pass so a missing record is not double-counted as a defect, but the flag
// keeps the summary honest about how much was really measured.
type authorityCheck struct {
	Name       string `json:"name"`
	Pass       bool   `json:"pass"`
	Applicable bool   `json:"applicable"`
	Detail     string `json:"detail"`
}

type authorityRunReport struct {
	Case    string           `json:"case"`
	Kind    string           `json:"kind"`
	Surface string           `json:"surface"`
	Checks  []authorityCheck `json:"checks"`
}

type authorityReport struct {
	GeneratedAt string               `json:"generated_at"`
	CaseFile    string               `json:"case_file"`
	Cases       int                  `json:"cases"`
	Skipped     []string             `json:"skipped,omitempty"`
	Runs        []authorityRunReport `json:"runs"`
	// MeasuredNothing lists cases where no check was applicable on any
	// surface. Such a case is green by construction and is measuring nothing;
	// it is listed so a set cannot quietly lose coverage while staying green.
	MeasuredNothing []string                         `json:"cases_measuring_nothing,omitempty"`
	Summary         map[string]authoritySummaryEntry `json:"summary"`
}

// authoritySummaryEntry counts one check across every run. Pass and Fail count
// only applicable checks; a check that could not run is counted under
// NotApplicable and never as a pass.
type authoritySummaryEntry struct {
	Pass          int `json:"pass"`
	Fail          int `json:"fail"`
	NotApplicable int `json:"not_applicable"`
	Applicable    int `json:"applicable"`
}

func authorityPass(name, detail string) authorityCheck {
	return authorityCheck{Name: name, Pass: true, Applicable: true, Detail: detail}
}

func authorityFail(name, detail string) authorityCheck {
	return authorityCheck{Name: name, Pass: false, Applicable: true, Detail: detail}
}

func authorityNA(name, detail string) authorityCheck {
	return authorityCheck{Name: name, Pass: true, Applicable: false, Detail: detail}
}

// checkAuthorityRank asks whether the records a human preferred came back
// ahead of the records a human demoted.
func checkAuthorityRank(c authorityCase, v authorityView) authorityCheck {
	var preferSeen, demoteSeen []string
	for _, id := range c.Prefer {
		if v.rank(id) >= 0 {
			preferSeen = append(preferSeen, id)
		}
	}
	for _, id := range c.Demote {
		if v.rank(id) >= 0 {
			demoteSeen = append(demoteSeen, id)
		}
	}
	if len(c.Prefer) == 0 && len(c.Demote) == 0 {
		return authorityNA(authorityCheckRank, "the case names no preferred or demoted record")
	}
	// A case that names only records to demote measures nothing when none of
	// them came back: there is no ordering to judge. Calling that a pass would
	// let a case keep scoring green after its records stopped being retrieved.
	if len(c.Prefer) == 0 && len(demoteSeen) == 0 {
		return authorityNA(authorityCheckRank, fmt.Sprintf("no demoted record came back (wanted one of %v), so there is no ordering to judge", c.Demote))
	}
	if len(c.Prefer) > 0 && len(preferSeen) == 0 {
		return authorityFail(authorityCheckRank, fmt.Sprintf("no preferred record came back (wanted one of %v)", c.Prefer))
	}
	for _, p := range preferSeen {
		for _, d := range demoteSeen {
			if v.rank(p) > v.rank(d) {
				return authorityFail(authorityCheckRank, fmt.Sprintf("%s ranked %d, behind demoted %s at %d", p, v.rank(p)+1, d, v.rank(d)+1))
			}
		}
	}
	if len(demoteSeen) == 0 {
		return authorityPass(authorityCheckRank, fmt.Sprintf("preferred %v came back; no demoted record did", preferSeen))
	}
	return authorityPass(authorityCheckRank, fmt.Sprintf("preferred %v ranked ahead of demoted %v", preferSeen, demoteSeen))
}

// checkAuthorityLater asks whether a demoted record warns the reader that a
// preferred record exists. The pointer is a reason to read both, never a claim
// that the older record is dead.
func checkAuthorityLater(c authorityCase, v authorityView) authorityCheck {
	if len(c.Prefer) == 0 {
		return authorityNA(authorityCheckLater, "the case names no preferred record to point at")
	}
	preferred := map[string]bool{}
	for _, id := range c.Prefer {
		preferred[id] = true
	}
	checked := 0
	for _, id := range c.Demote {
		row, ok := v.row(id)
		if !ok {
			continue
		}
		checked++
		if row.LaterID == "" {
			return authorityFail(authorityCheckLater, fmt.Sprintf("%s came back with no later related record", id))
		}
		if !preferred[row.LaterID] {
			return authorityFail(authorityCheckLater, fmt.Sprintf("%s points at %s, which the case does not prefer", id, row.LaterID))
		}
	}
	if checked == 0 {
		return authorityNA(authorityCheckLater, "no demoted record came back")
	}
	return authorityPass(authorityCheckLater, fmt.Sprintf("%d demoted record(s) point at a preferred record", checked))
}

// checkAuthorityProvenance asks whether the surface named the origin a human
// gave the record.
func checkAuthorityProvenance(c authorityCase, v authorityView) authorityCheck {
	ids := sortedKeys(c.Provenance)
	checked := 0
	for _, id := range ids {
		row, ok := v.row(id)
		if !ok {
			continue
		}
		checked++
		want := c.Provenance[id]
		if row.Provenance == "" {
			return authorityFail(authorityCheckProv, fmt.Sprintf("%s carries no provenance; the label says %s", id, want))
		}
		if row.Provenance != want {
			return authorityFail(authorityCheckProv, fmt.Sprintf("%s shows %s; the label says %s", id, row.Provenance, want))
		}
	}
	if checked == 0 {
		return authorityNA(authorityCheckProv, "no labelled record came back")
	}
	return authorityPass(authorityCheckProv, fmt.Sprintf("%d record(s) show the labelled origin", checked))
}

// checkAuthorityAdopted asks whether the surface described user adoption the
// way the label allows. No field in the vault records adoption today, so the
// surface may only ever say unchecked, or that the writer reported it. A label
// of "user" means the labeller found the user's own words; the surface cannot
// show that yet, and saying "unchecked" about it is honest, so either wording
// passes and the detail records which one appeared.
func checkAuthorityAdopted(c authorityCase, v authorityView, surface string) authorityCheck {
	if surface != authoritySurfaceContext {
		return authorityNA(authorityCheckAdopted, "only context_memory prints adoption wording")
	}
	ids := sortedKeys(c.DecisionAdopted)
	checked := 0
	for _, id := range ids {
		row, ok := v.row(id)
		if !ok {
			continue
		}
		checked++
		label := c.DecisionAdopted[id]
		if row.Adopted == "" {
			return authorityFail(authorityCheckAdopted, fmt.Sprintf("%s prints no adoption line; the label says %s", id, label))
		}
		switch label {
		case "unverified":
			if row.Adopted != authorityAdoptedUnchecked {
				return authorityFail(authorityCheckAdopted, fmt.Sprintf("%s prints %q; an unverified decision must print %q", id, row.Adopted, authorityAdoptedUnchecked))
			}
		case "reported-by-writer":
			// The labeller judged that the writer claimed the user confirmed
			// this. Mora does not read that claim, so the surface must still
			// say only that adoption is unchecked; claiming more would be the
			// invented authority this eval exists to catch.
			if row.Adopted != authorityAdoptedUnchecked {
				return authorityFail(authorityCheckAdopted, fmt.Sprintf("%s prints %q; no field records adoption, so it must print %q", id, row.Adopted, authorityAdoptedUnchecked))
			}
		case "user":
			// Even where the labeller found the user's own words, the vault
			// holds no field recording adoption, so the surface may say only
			// that it is unchecked. A reader who needs the user's agreement
			// must go and find it.
			if row.Adopted != authorityAdoptedUnchecked {
				return authorityFail(authorityCheckAdopted, fmt.Sprintf("%s prints %q, which claims more than any field records", id, row.Adopted))
			}
		}
	}
	if checked == 0 {
		return authorityNA(authorityCheckAdopted, "no labelled decision came back")
	}
	return authorityPass(authorityCheckAdopted, fmt.Sprintf("%d decision(s) describe adoption within what the vault records", checked))
}

// checkAuthorityAssert asks whether the emitted prose states a claim the case
// says must not be stated. A record may still carry the phrase when it also
// points the reader at the later record.
func checkAuthorityAssert(c authorityCase, v authorityView, surface string) authorityCheck {
	if surface != authoritySurfaceContext {
		return authorityNA(authorityCheckAssert, "only context_memory emits prose")
	}
	if len(c.MustNotAssert) == 0 {
		return authorityNA(authorityCheckAssert, "the case names no phrase to watch for")
	}
	if strings.TrimSpace(v.Text) == "" {
		return authorityNA(authorityCheckAssert, "the surface emitted no text")
	}
	// Prose with no records behind it, such as "no open commitments matched",
	// cannot state a watched phrase and cannot clear one either. Calling that
	// a pass would let a case score green while reading nothing.
	if len(v.Rows) == 0 {
		return authorityNA(authorityCheckAssert, "the surface returned no records, so no phrase could be stated or qualified")
	}
	preferred := map[string]bool{}
	for _, id := range c.Prefer {
		preferred[id] = true
	}
	for _, row := range v.Rows {
		for _, phrase := range c.MustNotAssert {
			if phrase == "" || !strings.Contains(strings.ToLower(row.Block), strings.ToLower(phrase)) {
				continue
			}
			if row.LaterID != "" && preferred[row.LaterID] {
				continue
			}
			return authorityFail(authorityCheckAssert, fmt.Sprintf("%s states %q with no pointer to a preferred later record", row.ID, phrase))
		}
	}
	return authorityPass(authorityCheckAssert, fmt.Sprintf("no record states any of %d watched phrases unqualified", len(c.MustNotAssert)))
}

func runAuthorityChecks(c authorityCase, v authorityView, surface string) []authorityCheck {
	return []authorityCheck{
		checkAuthorityRank(c, v),
		checkAuthorityLater(c, v),
		checkAuthorityProvenance(c, v),
		checkAuthorityAdopted(c, v, surface),
		checkAuthorityAssert(c, v, surface),
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// authoritySearchView runs the real search_memory handler and reads back what
// it put on the wire. It reads the rows as JSON, exactly as an agent would, so
// the harness compiles and runs against a build that has no provenance field.
func authoritySearchView(ctx context.Context, cfg Config, query string) (authorityView, error) {
	res, err := mcpSearchMemory(authorityQuietCtx(ctx), cfg, map[string]any{"query": query, "limit": float64(10)})
	if err != nil {
		return authorityView{}, err
	}
	out, ok := res.(map[string]any)
	if !ok {
		return authorityView{}, fmt.Errorf("search_memory returned %T, want a result map", res)
	}
	raw, err := json.Marshal(out["results"])
	if err != nil {
		return authorityView{}, err
	}
	var rows []struct {
		ID         string `json:"id"`
		Provenance string `json:"provenance"`
		Later      *struct {
			ID string `json:"id"`
		} `json:"later_related_evidence"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return authorityView{}, err
	}
	view := authorityView{Text: string(raw)}
	for _, r := range rows {
		row := authorityRow{ID: r.ID, Provenance: r.Provenance}
		if r.Later != nil {
			row.LaterID = r.Later.ID
		}
		view.Rows = append(view.Rows, row)
	}
	return view, nil
}

// authorityContextView runs the real context_memory handler, then splits the
// emitted text back into one block per record. context_memory prints titles
// rather than ids, so the harness re-runs the same retrieval to learn which id
// each block belongs to. That second pass reads only; it never feeds the text.
func authorityContextView(ctx context.Context, cfg Config, query string) (authorityView, error) {
	res, err := mcpContextMemory(authorityQuietCtx(ctx), cfg, map[string]any{"query": query})
	if err != nil {
		return authorityView{}, err
	}
	out, ok := res.(map[string]any)
	if !ok {
		return authorityView{}, fmt.Errorf("context_memory returned %T, want a result map", res)
	}
	text, _ := out["context"].(string)
	now := briefClock()
	filters, err := parseSearchFilters(map[string]any{}, now)
	if err != nil {
		return authorityView{}, err
	}
	items, _, intent, err := contextQueryData(ctx, cfg, query, "", 10, filters, now)
	if err != nil {
		return authorityView{}, err
	}
	view := authorityView{Text: text}
	if intent == contextIntentOpenLoops {
		// The open-loops intent renders commitments, not records. There is no
		// per-record block to read, so the case reports no rows.
		return view, nil
	}
	for i, item := range items {
		start := strings.Index(text, "\n# "+item.Title+"\n")
		if start < 0 {
			continue // the budget cut this record before it was emitted
		}
		end := len(text)
		for j := i + 1; j < len(items); j++ {
			if next := strings.Index(text[start+1:], "\n# "+items[j].Title+"\n"); next >= 0 {
				end = start + 1 + next
				break
			}
		}
		block := text[start:end]
		view.Rows = append(view.Rows, authorityRow{
			ID:         item.ID,
			Provenance: authorityProvenanceFromBlock(block),
			LaterID:    authorityLaterIDFromBlock(block),
			Adopted:    authorityAdoptedFromBlock(block),
			Block:      block,
		})
	}
	return view, nil
}

// authorityQuietCtx suppresses usage logging so measuring the vault does not
// also write to it. Search still appends its own query-correlation trace line,
// exactly as any real search does.
func authorityQuietCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, mcpUsageTraceKey{}, &mcpUsageTrace{})
}

// authorityProvenanceFromBlock turns the printed provenance sentence back into
// the one-word kind the labels use.
func authorityProvenanceFromBlock(block string) string {
	line := authorityLineWithPrefix(block, "Provenance: ")
	if line == "" {
		return ""
	}
	body := strings.TrimPrefix(line, "Provenance: ")
	switch {
	case strings.HasPrefix(body, "evidence"):
		return "evidence"
	case strings.HasPrefix(body, "a file on disk"):
		return "document"
	case strings.HasPrefix(body, "agent-written"):
		return "authored"
	}
	return "unrecognized: " + body
}

func authorityAdoptedFromBlock(block string) string {
	line := authorityLineWithPrefix(block, "Adopted by user: ")
	if line == "" {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "Adopted by user: "))
}

func authorityLaterIDFromBlock(block string) string {
	line := authorityLineWithPrefix(block, "Later related record: ")
	if line == "" {
		return ""
	}
	rest := strings.TrimPrefix(line, "Later related record: ")
	if cut := strings.Index(rest, " "); cut >= 0 {
		return rest[:cut]
	}
	return rest
}

func authorityLineWithPrefix(block, prefix string) string {
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// runAuthorityEval runs every case on every surface it names and returns the
// report. It is shared by the live test and the synthetic one.
func runAuthorityEval(t *testing.T, ctx context.Context, cfg Config, caseFile string, cases []authorityCase) authorityReport {
	t.Helper()
	report := authorityReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		CaseFile:    caseFile,
		Cases:       len(cases),
		Summary:     map[string]authoritySummaryEntry{},
	}
	for _, c := range cases {
		if c.SkipReason != "" {
			report.Skipped = append(report.Skipped, c.ID+": "+c.SkipReason)
			t.Logf("SKIP %s — %s", c.ID, c.SkipReason)
			continue
		}
		for _, surface := range c.Surfaces {
			var view authorityView
			var err error
			switch surface {
			case authoritySurfaceSearch:
				view, err = authoritySearchView(ctx, cfg, c.Query)
			case authoritySurfaceContext:
				view, err = authorityContextView(ctx, cfg, c.Query)
			}
			if err != nil {
				t.Errorf("%s / %s: the handler failed: %v", c.ID, surface, err)
				continue
			}
			checks := runAuthorityChecks(c, view, surface)
			report.Runs = append(report.Runs, authorityRunReport{Case: c.ID, Kind: c.Kind, Surface: surface, Checks: checks})
			var parts []string
			for _, ch := range checks {
				status := "pass"
				if !ch.Pass {
					status = "FAIL"
				} else if !ch.Applicable {
					status = "n/a"
				}
				parts = append(parts, fmt.Sprintf("%s=%s (%s)", ch.Name, status, ch.Detail))
				entry := report.Summary[ch.Name]
				switch {
				case !ch.Applicable:
					entry.NotApplicable++
				case ch.Pass:
					entry.Pass++
					entry.Applicable++
				default:
					entry.Fail++
					entry.Applicable++
				}
				report.Summary[ch.Name] = entry
			}
			t.Logf("%-28s %-14s %s", c.ID, surface, strings.Join(parts, " | "))
		}
	}
	measured := map[string]bool{}
	for _, run := range report.Runs {
		for _, ch := range run.Checks {
			if ch.Applicable {
				measured[run.Case] = true
			}
		}
	}
	for _, c := range cases {
		if c.SkipReason == "" && !measured[c.ID] {
			report.MeasuredNothing = append(report.MeasuredNothing, c.ID)
			t.Logf("MEASURED NOTHING %s: no check was applicable on any surface", c.ID)
		}
	}
	return report
}

func writeAuthorityReport(t *testing.T, report authorityReport) {
	t.Helper()
	path := os.Getenv("MORA_AUTHORITY_REPORT")
	if path == "" {
		return
	}
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("encode the authority report: %v", err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Fatalf("write the authority report to %s: %v", path, err)
	}
	t.Logf("wrote the authority report to %s", path)
}

func logAuthoritySummary(t *testing.T, report authorityReport) {
	t.Helper()
	names := []string{authorityCheckRank, authorityCheckLater, authorityCheckProv, authorityCheckAdopted, authorityCheckAssert}
	for _, name := range names {
		entry := report.Summary[name]
		t.Logf("SUMMARY %-8s pass=%d fail=%d n/a=%d measured=%d", name, entry.Pass, entry.Fail, entry.NotApplicable, entry.Applicable)
	}
	t.Logf("SUMMARY cases=%d skipped=%d runs=%d", report.Cases, len(report.Skipped), len(report.Runs))
}

// TestAuthorityLive scores the real vault, read-only. It is gated on
// MORA_EVAL_LIVE exactly as the retrieval eval is, and it needs the gitignored
// case file, which names real records. It reports; it never fails the build on
// a case, because the point is to watch the numbers move across a change.
func TestAuthorityLive(t *testing.T) {
	cfg := liveCfgOrSkip(t)
	if cfg.VaultDir == "" {
		// MORA_EVAL_LIVE named a data directory only. The read surfaces also
		// need the vault: context_memory reads the control files there and the
		// governance ledger lives there too, so without it every record would
		// look visible and the no-query paths would come back empty.
		//
		// This reads the real user config on purpose, which loadConfigFor
		// refuses to do from a test. The refusal exists to stop a test from
		// mutating the developer's vault; this test only reads, and the whole
		// point of MORA_EVAL_LIVE is to score that vault. Set
		// MORA_AUTHORITY_VAULT to point somewhere else.
		resolved, err := configstore.Load()
		if err != nil {
			t.Fatalf("resolve the vault for MORA_EVAL_LIVE=%s: %v", cfg.DataDir, err)
		}
		resolved.DataDir = cfg.DataDir
		cfg = resolved
		if override := os.Getenv("MORA_AUTHORITY_VAULT"); override != "" {
			cfg.VaultDir = override
		}
	}
	caseFile := "authority_cases.jsonl"
	if _, err := os.Stat(caseFile); err != nil {
		t.Skipf("hand-label %s to run the authority eval; it names real records and is gitignored", caseFile)
	}
	cases, err := loadAuthorityCases(caseFile)
	if err != nil {
		t.Fatalf("load the cases: %v", err)
	}
	t.Logf("authority eval against vault=%s data=%s cases=%d", cfg.VaultDir, cfg.DataDir, len(cases))
	report := runAuthorityEval(t, testCtx(t), cfg, caseFile, cases)
	logAuthoritySummary(t, report)
	writeAuthorityReport(t, report)
}
