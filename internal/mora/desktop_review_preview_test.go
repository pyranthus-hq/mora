package mora

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	indexstore "github.com/pyranthus-hq/mora/internal/index"
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestDesktopReviewPreviewUsesCanonicalRead(t *testing.T) {
	cfg := reviewSetup(t)
	saved := reviewSaved(t, "Saved compass")
	held := reviewHeld(t, "Held compass")

	m, err := findMemory(cfg, saved)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(m.Path)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := readMCPWriteProposal(cfg, held)
	if err != nil {
		t.Fatal(err)
	}
	argumentRevision, err := mcpProposalArgumentsSHA256(p.Arguments)
	if err != nil {
		t.Fatal(err)
	}

	before := proposalInspectionSnapshot(t, cfg)
	for _, tc := range []struct {
		kind, id, title, typ, state, revision string
	}{
		{reviewKindSaved, saved, "Saved compass", "fact", "tentative", indexstore.ContentSHA256(raw)},
		{reviewKindHeld, held, "Held compass", "rule", "pending", argumentRevision},
	} {
		preview, err := loadDesktopReviewPreview(cfg, tc.kind, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if preview.Kind != tc.kind || preview.ID != tc.id || preview.Title != tc.title ||
			preview.Text != "Body of "+tc.title+"." || preview.Scope != "project:demo" ||
			preview.Type != tc.typ || preview.Source != "mcp" || preview.Provenance != memory.ProvenanceAuthored ||
			preview.State != tc.state || preview.CanonicalRevision != tc.revision || len(preview.DisplayDigest) != 64 ||
			!preview.Complete {
			t.Fatalf("preview = %+v", preview)
		}
		again, err := loadDesktopReviewPreview(cfg, tc.kind, tc.id)
		if err != nil || again.DisplayDigest != preview.DisplayDigest {
			t.Fatalf("repeat display digest = %q, %v; want %q", again.DisplayDigest, err, preview.DisplayDigest)
		}
	}
	if after := proposalInspectionSnapshot(t, cfg); !bytes.Equal(mustDesktopPreviewJSON(t, before), mustDesktopPreviewJSON(t, after)) {
		t.Fatal("preview read changed config, state, data, or vault")
	}

	firstSaved, err := loadDesktopReviewPreview(cfg, reviewKindSaved, saved)
	if err != nil {
		t.Fatal(err)
	}
	m.Scope = "project:next"
	edited, err := renderMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Path, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	preview, err := loadDesktopReviewPreview(cfg, reviewKindSaved, saved)
	if err != nil || preview.Scope != "project:next" ||
		preview.CanonicalRevision != indexstore.ContentSHA256(edited) || preview.DisplayDigest == firstSaved.DisplayDigest {
		t.Fatalf("changed saved note was not read from its current revision: %+v (%v)", preview, err)
	}

	first, err := loadDesktopReviewPreview(cfg, reviewKindHeld, held)
	if err != nil {
		t.Fatal(err)
	}
	p.Arguments["source"] = "updated fixture source"
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	second, err := loadDesktopReviewPreview(cfg, reviewKindHeld, held)
	if err != nil || second.Source != "updated fixture source" ||
		second.CanonicalRevision == first.CanonicalRevision || second.DisplayDigest == first.DisplayDigest {
		t.Fatalf("changed held source did not change the preview revision: %+v (%v)", second, err)
	}
}

func TestDesktopReviewPreviewMarksIncompleteContent(t *testing.T) {
	cfg := reviewSetup(t)
	saved := reviewSaved(t, "Saved synthetic")
	held := reviewHeld(t, "Held synthetic")
	p, _, err := readMCPWriteProposal(cfg, held)
	if err != nil {
		t.Fatal(err)
	}
	p.Arguments["text"] = "token=fixture-hidden-value\n" + strings.Repeat("雪", 20001)
	p.Arguments["nested"] = map[string]any{"credential": "fixture-hidden-nested"}
	p.Arguments["source"] = "https://user:fixture-hidden-credential@example.invalid/path"
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}

	preview, err := loadDesktopReviewPreview(cfg, reviewKindHeld, held)
	if err != nil || preview.Complete || !preview.Redacted || !preview.Truncated {
		t.Fatalf("held preview flags = %+v, %v", preview, err)
	}
	wire := string(mustDesktopPreviewJSON(t, preview))
	if !strings.Contains(preview.Text, "[REDACTED]") || strings.Contains(wire, "fixture-hidden-") ||
		!utf8.ValidString(preview.Text) || len(preview.Text) > 20000 {
		t.Fatal("preview exceeded the UTF-8 cap or exposed a redacted value")
	}

	first := preview
	p.Arguments["nested"] = map[string]any{"credential": "fixture-hidden-updated"}
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	preview, err = loadDesktopReviewPreview(cfg, reviewKindHeld, held)
	if err != nil || preview.DisplayDigest != first.DisplayDigest || preview.CanonicalRevision == first.CanonicalRevision {
		t.Fatalf("hidden edit did not preserve the display and change the canonical revision: %v", err)
	}

	m, err := findMemory(cfg, saved)
	if err != nil {
		t.Fatal(err)
	}
	m.Text = "Bearer fixture-hidden-saved\ncontrol\u202e line"
	m.Truncated = true
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	preview, err = loadDesktopReviewPreview(cfg, reviewKindSaved, saved)
	if err != nil || preview.Complete || !preview.Redacted || !preview.Truncated || !preview.Escaped ||
		strings.ContainsAny(preview.Text, "\u202e") || strings.Contains(preview.Text, "fixture-hidden-") {
		t.Fatalf("saved preview did not mark incomplete data: flags = %t/%t/%t/%t, %v", preview.Complete, preview.Redacted, preview.Truncated, preview.Escaped, err)
	}
}

func TestDesktopReviewPreviewRefusesUnavailableItems(t *testing.T) {
	cfg := reviewSetup(t)
	saved := reviewSaved(t, "No longer pending")
	held := reviewHeld(t, "Malformed proposal")
	decided := reviewHeld(t, "Already rejected")
	run(t, "confirm", saved)
	run(t, "mcp", "proposals", "reject", decided)
	_, path, err := readMCPWriteProposal(cfg, held)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ kind, id string }{
		{reviewKindSaved, saved},
		{reviewKindHeld, held},
		{reviewKindHeld, decided},
		{reviewKindHeld, "p_missing"},
		{reviewKindSaved, "mem_missing"},
		{"unrecognized", held},
	} {
		preview, err := loadDesktopReviewPreview(cfg, tc.kind, tc.id)
		if err == nil || preview.Complete || preview.Text != "" || preview.DisplayDigest != "" {
			t.Fatalf("unavailable %s item returned usable preview: %+v, %v", tc.kind, preview, err)
		}
	}
}

func TestDesktopReviewPreviewRequiresEveryHeldArgumentToBeShown(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewHeld(t, "Held compass")
	first, err := loadDesktopReviewPreview(cfg, reviewKindHeld, id)
	if err != nil || !first.Complete {
		t.Fatalf("known fields must produce a complete preview: %v", err)
	}
	p, _, err := readMCPWriteProposal(cfg, id)
	if err != nil {
		t.Fatal(err)
	}

	p.Arguments["unrecognized_field"] = "unknown-fixture-one"
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	one, err := loadDesktopReviewPreview(cfg, reviewKindHeld, id)
	if err != nil || one.Complete || !one.OmittedArguments || one.DisplayDigest == first.DisplayDigest {
		t.Fatalf("unrendered argument was not marked incomplete: %v", err)
	}
	if strings.Contains(string(mustDesktopPreviewJSON(t, one)), "unknown-fixture-one") {
		t.Fatal("preview disclosed the unrendered argument value")
	}

	p.Arguments["unrecognized_field"] = "unknown-fixture-two"
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	two, err := loadDesktopReviewPreview(cfg, reviewKindHeld, id)
	if err != nil || two.Complete || two.DisplayDigest != one.DisplayDigest || two.CanonicalRevision == one.CanonicalRevision {
		t.Fatalf("unrendered argument edit did not change only the canonical revision: %v", err)
	}

	delete(p.Arguments, "unrecognized_field")
	p.Arguments["source"] = true
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	invalid, err := loadDesktopReviewPreview(cfg, reviewKindHeld, id)
	if err != nil || invalid.Complete || !invalid.OmittedArguments {
		t.Fatalf("defaulted invalid argument was not marked incomplete: %v", err)
	}
}

func TestDesktopReviewPreviewShowsDecisionDetails(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewHeld(t, "Decision fixture")
	p, _, err := readMCPWriteProposal(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	p.Arguments["type"] = "decision"
	p.Arguments["as_of"] = "2026-11-12T20:00:00Z"
	p.Arguments["durability"] = "working"
	p.Arguments["flip_conditions"] = "the fixture changes;the trial ends"
	p.Arguments["review_by"] = "2026-12-12T20:00:00Z"
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	preview, err := loadDesktopReviewPreview(cfg, reviewKindHeld, id)
	if err != nil || !preview.Complete {
		t.Fatalf("fully rendered decision marked incomplete: %v", err)
	}
	got := map[string][]string{}
	for _, field := range preview.Details {
		got[field.Label] = append(got[field.Label], field.Value)
	}
	if strings.Join(got["decision flip condition"], ";") != p.Arguments["flip_conditions"] ||
		strings.Join(got["decision as of"], "") != p.Arguments["as_of"] ||
		strings.Join(got["decision durability"], "") != p.Arguments["durability"] ||
		strings.Join(got["decision review by"], "") != p.Arguments["review_by"] {
		t.Fatal("complete preview did not show the original decision details")
	}
}

func TestDesktopReviewPreviewBoundsFileReadsAndKeepsIDLookup(t *testing.T) {
	cfg := reviewSetup(t)
	saved := reviewSaved(t, "Saved compass")
	m, err := findMemory(cfg, saved)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(filepath.Dir(m.Path), "hand-named.md")
	if err := os.Rename(m.Path, moved); err != nil {
		t.Fatal(err)
	}
	preview, err := loadDesktopReviewPreview(cfg, reviewKindSaved, saved)
	if err != nil || !preview.Complete || preview.ID != saved {
		t.Fatalf("hand-named record did not resolve by canonical ID: %v", err)
	}
	if err := os.Truncate(moved, maxDesktopReviewMemoryBytes+1); err != nil {
		t.Fatal(err)
	}
	if preview, err := loadDesktopReviewPreview(cfg, reviewKindSaved, saved); err == nil || preview.DisplayDigest != "" {
		t.Fatal("oversized saved record returned a preview")
	}

	held := reviewHeld(t, "Held compass")
	_, path, err := readMCPWriteProposal(cfg, held)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxProposalInspectionBytes+1); err != nil {
		t.Fatal(err)
	}
	if preview, err := loadDesktopReviewPreview(cfg, reviewKindHeld, held); err == nil || preview.DisplayDigest != "" {
		t.Fatal("oversized held record returned a preview")
	}
}

func TestReviewShowPrintsReadOnlyDesktopPreview(t *testing.T) {
	cfg := reviewSetup(t)
	saved := reviewSaved(t, "Saved CLI preview")
	held := reviewHeld(t, "Held CLI preview")
	before := proposalInspectionSnapshot(t, cfg)

	for _, tc := range []struct {
		kind, id string
	}{
		{reviewKindSaved, saved},
		{reviewKindHeld, held},
	} {
		var receipt struct {
			Schema  string               `json:"schema"`
			Preview desktopReviewPreview `json:"preview"`
		}
		if err := json.Unmarshal([]byte(run(t, "review", "--show", tc.kind, tc.id, "--json")), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.Schema != "mora.review.preview" || receipt.Preview.Kind != tc.kind || receipt.Preview.ID != tc.id ||
			receipt.Preview.CanonicalRevision == "" || receipt.Preview.DisplayDigest == "" || !receipt.Preview.Complete {
			t.Fatalf("preview receipt = %+v", receipt)
		}
	}
	if after := proposalInspectionSnapshot(t, cfg); !bytes.Equal(mustDesktopPreviewJSON(t, before), mustDesktopPreviewJSON(t, after)) {
		t.Fatal("review --show changed config, state, data, vault, or proposal queue")
	}
}

func TestReviewShowRejectsInvalidCombinations(t *testing.T) {
	for _, args := range [][]string{
		{"review", "--show", reviewKindSaved},
		{"review", "--show", reviewKindSaved, "mem_missing"},
		{"review", "--show", "unknown", "mem_missing", "--json"},
		{"review", "--list", "--show", reviewKindSaved, "mem_missing", "--json"},
		{"review", "--show", reviewKindSaved, "mem_missing", "extra", "--json"},
	} {
		var out bytes.Buffer
		if err := Run(testCtx(t), args, &out, &out, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), reviewUsage) {
			t.Fatalf("Run(%q) = %v, %q; want usage error", args, err, out.String())
		}
	}
}

func TestReviewShowMarksIncompleteContent(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "Incomplete CLI preview")
	m, err := findMemory(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	m.Text = "Bearer fixture-hidden-value"
	m.Truncated = true
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Preview desktopReviewPreview `json:"preview"`
	}
	if err := json.Unmarshal([]byte(run(t, "review", "--show", reviewKindSaved, id, "--json")), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Preview.Complete || !receipt.Preview.Redacted || !receipt.Preview.Truncated || strings.Contains(receipt.Preview.Text, "fixture-hidden-value") {
		t.Fatalf("incomplete preview = %+v", receipt.Preview)
	}
}

func TestReviewShowFailsClosedForUnavailableItems(t *testing.T) {
	reviewSetup(t)
	saved := reviewSaved(t, "Confirmed preview")
	held := reviewHeld(t, "Rejected preview")
	run(t, "confirm", saved)
	run(t, "mcp", "proposals", "reject", held)
	for _, tc := range []struct{ kind, id string }{
		{reviewKindSaved, saved},
		{reviewKindHeld, held},
		{reviewKindSaved, "mem_missing"},
		{reviewKindHeld, "p_missing"},
	} {
		_, err := runErr(t, "review", "--show", tc.kind, tc.id, "--json")
		if err != errDesktopReviewUnavailable {
			t.Fatalf("unavailable %s/%s error = %v", tc.kind, tc.id, err)
		}
	}
}

func mustDesktopPreviewJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
