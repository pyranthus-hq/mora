package mora

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// These tests keep the authority harness honest without the real vault. They
// use invented records, so they are safe to check in, and they cover two
// things the live run cannot: that the loader refuses a bad label, and that
// each check actually fails when the surface misbehaves.

func TestAuthorityLoaderReadsTheSyntheticCases(t *testing.T) {
	cases, err := loadAuthorityCases(filepath.Join("testdata", "authority", "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Fatalf("read %d cases, want 2", len(cases))
	}
	first := cases[0]
	if first.ID != "widget-pilot-correction" || first.Provenance["mem_widget_pilot"] != "authored" {
		t.Fatalf("first case came back wrong: %+v", first)
	}
	if first.DecisionAdopted["mem_widget_pilot"] != "unverified" {
		t.Fatalf("adoption label came back wrong: %+v", first.DecisionAdopted)
	}
	if len(first.Evidence) != 2 {
		t.Fatalf("evidence should keep both a note and a record reference, got %d", len(first.Evidence))
	}
}

func TestAuthorityLoaderRefusesAnUnknownLabel(t *testing.T) {
	_, err := loadAuthorityCases(filepath.Join("testdata", "authority", "bad-cases.jsonl"))
	if err == nil {
		t.Fatal("the loader accepted an unknown provenance label")
	}
	if !strings.Contains(err.Error(), "probably-authored") {
		t.Fatalf("the error should name the bad label, got: %v", err)
	}
}

// authorityTestCase is the shape every check test starts from.
func authorityTestCase() authorityCase {
	return authorityCase{
		ID: "synthetic", Kind: "stale-vs-correction", Query: "widget pilot",
		Surfaces:      []string{authoritySurfaceContext},
		Prefer:        []string{"new"},
		Demote:        []string{"old"},
		MustNotAssert: []string{"the pilot is running"},
		Provenance:    map[string]string{"old": "authored"},
		LabelSource:   "synthetic",
	}
}

func TestAuthorityRankCheckCatchesAStaleRecordInFront(t *testing.T) {
	c := authorityTestCase()
	good := authorityView{Rows: []authorityRow{{ID: "new"}, {ID: "old"}}}
	if got := checkAuthorityRank(c, good); !got.Pass {
		t.Fatalf("the right order failed: %s", got.Detail)
	}
	bad := authorityView{Rows: []authorityRow{{ID: "old"}, {ID: "new"}}}
	if got := checkAuthorityRank(c, bad); got.Pass {
		t.Fatal("the stale record led and the check still passed")
	}
	missing := authorityView{Rows: []authorityRow{{ID: "old"}}}
	if got := checkAuthorityRank(c, missing); got.Pass {
		t.Fatal("the preferred record never came back and the check still passed")
	}
}

func TestAuthorityLaterCheckWantsAPointerAtThePreferredRecord(t *testing.T) {
	c := authorityTestCase()
	good := authorityView{Rows: []authorityRow{{ID: "new"}, {ID: "old", LaterID: "new"}}}
	if got := checkAuthorityLater(c, good); !got.Pass {
		t.Fatalf("a correct pointer failed: %s", got.Detail)
	}
	none := authorityView{Rows: []authorityRow{{ID: "new"}, {ID: "old"}}}
	if got := checkAuthorityLater(c, none); got.Pass {
		t.Fatal("a stale record with no pointer still passed")
	}
	wrong := authorityView{Rows: []authorityRow{{ID: "new"}, {ID: "old", LaterID: "unrelated"}}}
	if got := checkAuthorityLater(c, wrong); got.Pass {
		t.Fatal("a pointer at an unrelated record still passed")
	}
}

func TestAuthorityProvenanceCheckComparesAgainstTheLabel(t *testing.T) {
	c := authorityTestCase()
	good := authorityView{Rows: []authorityRow{{ID: "old", Provenance: "authored"}}}
	if got := checkAuthorityProvenance(c, good); !got.Pass {
		t.Fatalf("a matching kind failed: %s", got.Detail)
	}
	wrong := authorityView{Rows: []authorityRow{{ID: "old", Provenance: "evidence"}}}
	if got := checkAuthorityProvenance(c, wrong); got.Pass {
		t.Fatal("an agent note shown as evidence still passed")
	}
	silent := authorityView{Rows: []authorityRow{{ID: "old"}}}
	if got := checkAuthorityProvenance(c, silent); got.Pass {
		t.Fatal("a row with no provenance at all still passed")
	}
}

func TestAuthorityAdoptedCheckRefusesToLetTheSurfaceOverstate(t *testing.T) {
	c := authorityTestCase()
	c.DecisionAdopted = map[string]string{"old": "unverified"}
	good := authorityView{Rows: []authorityRow{{ID: "old", Adopted: authorityAdoptedUnchecked}}}
	if got := checkAuthorityAdopted(c, good, authoritySurfaceContext); !got.Pass {
		t.Fatalf("the unchecked wording failed: %s", got.Detail)
	}
	overstated := authorityView{Rows: []authorityRow{{ID: "old", Adopted: "yes"}}}
	if got := checkAuthorityAdopted(c, overstated, authoritySurfaceContext); got.Pass {
		t.Fatal("a surface claiming adoption still passed")
	}
	// A case may record that the writer claimed the user confirmed a decision.
	// Mora does not read that claim, so the surface must still say only that
	// adoption is unchecked, and repeating the writer's claim is a failure.
	c.DecisionAdopted = map[string]string{"old": "reported-by-writer"}
	plain := authorityView{Rows: []authorityRow{{ID: "old", Adopted: authorityAdoptedUnchecked}}}
	if got := checkAuthorityAdopted(c, plain, authoritySurfaceContext); !got.Pass {
		t.Fatalf("the unchecked wording failed for a writer-reported label: %s", got.Detail)
	}
	echoed := authorityView{Rows: []authorityRow{{ID: "old", Adopted: authorityAdoptedReported}}}
	if got := checkAuthorityAdopted(c, echoed, authoritySurfaceContext); got.Pass {
		t.Fatal("a surface repeating the writer's confirmation claim still passed")
	}
	// Even a label saying the user's own words were found does not license a
	// stronger line, because no field in the vault records adoption.
	c.DecisionAdopted = map[string]string{"old": "user"}
	if got := checkAuthorityAdopted(c, plain, authoritySurfaceContext); !got.Pass {
		t.Fatalf("the unchecked wording failed for a user-supported label: %s", got.Detail)
	}
	if got := checkAuthorityAdopted(c, echoed, authoritySurfaceContext); got.Pass {
		t.Fatal("a user-supported label still licensed the retired writer-report wording")
	}
	if got := checkAuthorityAdopted(c, plain, authoritySurfaceSearch); got.Applicable {
		t.Fatal("search_memory prints no adoption wording, so the check cannot apply there")
	}
}

func TestAuthorityAssertCheckAllowsAClaimThatPointsForward(t *testing.T) {
	c := authorityTestCase()
	unqualified := authorityView{
		Text: "the pilot is running",
		Rows: []authorityRow{{ID: "old", Block: "\n# Widget pilot\nthe pilot is running\n"}},
	}
	if got := checkAuthorityAssert(c, unqualified, authoritySurfaceContext); got.Pass {
		t.Fatal("an unqualified stale claim still passed")
	}
	qualified := authorityView{
		Text: "the pilot is running",
		Rows: []authorityRow{{ID: "old", LaterID: "new", Block: "\n# Widget pilot\nLater related record: new\nthe pilot is running\n"}},
	}
	if got := checkAuthorityAssert(c, qualified, authoritySurfaceContext); !got.Pass {
		t.Fatalf("a claim that points at the newer record failed: %s", got.Detail)
	}
}

// TestAuthorityHarnessOnASyntheticVault runs the whole harness end to end
// against a vault built for this test, so the surface adapters — the ones that
// read provenance back out of a search row and out of context prose — stay
// under test without the live vault.
func TestAuthorityHarnessOnASyntheticVault(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	seed := []Memory{
		{
			ID: "mem_widget_pilot", Scope: "global", Type: "decision", Source: "mcp",
			Title: "Widget pilot", Text: "the pilot is running and we should keep contacting the pilot partner",
			CreatedAt: "2026-06-04T10:00:00Z",
			Decision:  &DecisionValidity{AsOf: "2026-06-04T10:00:00Z", Durability: "standing", Complete: true},
		},
		{
			ID: "mem_widget_pilot_ended", Scope: "global", Type: "fact", Source: "mcp",
			Title: "Widget pilot ended", Text: "the widget pilot ended on 30 July and nobody should contact the partner again",
			CreatedAt: "2026-07-30T10:00:00Z",
		},
		{
			ID: "gmail_thread/widget-invoice", Scope: "global", Type: "email",
			Provider: "gmail", ProviderID: "widget-invoice", Source: "gmail",
			Title: "Widget pilot invoice", Text: "the final widget pilot invoice is attached",
			CreatedAt: "2026-07-31T10:00:00Z", ContentHash: "widget-invoice",
		},
		{
			ID: "mem_widget_rollout", Scope: "global", Type: "decision", Source: "user-confirmed 2026-08-12",
			Title: "Widget rollout ordering", Text: "we roll out the widget by region, smallest first",
			CreatedAt: "2026-08-12T10:00:00Z",
			Decision:  &DecisionValidity{AsOf: "2026-08-12T10:00:00Z", Durability: "standing", Complete: true},
		},
	}
	for _, m := range seed {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed %s: %v", m.ID, err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	caseFile := filepath.Join("testdata", "authority", "cases.jsonl")
	cases, err := loadAuthorityCases(caseFile)
	if err != nil {
		t.Fatal(err)
	}
	report := runAuthorityEval(t, testCtx(t), cfg, caseFile, cases)
	if len(report.Runs) != 3 {
		t.Fatalf("expected three case-and-surface runs, got %d", len(report.Runs))
	}
	// RANK depends on retrieval order, which this test is not about; the check
	// tests above cover it directly. Everything the read surfaces control must
	// pass here.
	for _, run := range report.Runs {
		for _, check := range run.Checks {
			if check.Name == authorityCheckRank {
				continue
			}
			if !check.Pass {
				t.Errorf("%s / %s / %s failed: %s", run.Case, run.Surface, check.Name, check.Detail)
			}
		}
	}
	measured := 0
	for _, run := range report.Runs {
		for _, check := range run.Checks {
			if check.Name == authorityCheckProv && check.Applicable {
				measured++
			}
		}
	}
	if len(report.MeasuredNothing) > 0 {
		t.Fatalf("cases measuring nothing on any surface: %v", report.MeasuredNothing)
	}
	if measured == 0 {
		t.Fatal("no run measured provenance, so the surface adapters were never exercised")
	}
}
