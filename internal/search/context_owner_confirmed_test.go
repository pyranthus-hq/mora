package search

import (
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestContextSaysTheOwnerConfirmedTheExactText(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	m := memory.Memory{ID: "mem_1", Scope: "project:demo", Type: "decision", Title: "Ship it", Text: "we ship the rerank lane", Source: "mcp"}
	m.Tags = []string{memory.OwnerConfirmedTagPrefix + memory.OwnerConfirmedDigest(m)}
	got := BuildContext(cfg, []memory.Memory{m}, 4000, true)
	for _, want := range []string{
		`Provenance: agent-written (source "mcp"); the owner then confirmed this exact text in their own terminal.`,
		"Adopted by user: the owner confirmed this exact text in their own terminal",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	// Edited after the keep: the tag no longer counts, so neither line claims it.
	m.Text = "we ship the rerank lane next year"
	got = BuildContext(cfg, []memory.Memory{m}, 4000, true)
	if strings.Contains(got, "owner then confirmed") || strings.Contains(got, "the owner confirmed") || !strings.Contains(got, "Adopted by user: no record either way") {
		t.Fatalf("a stale tag still claimed confirmation:\n%s", got)
	}
}

func TestContextShowsAnOwnerDecisionWithoutValidityAsCurrent(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	bare := &memory.DecisionValidity{AsOf: "2026-11-10T00:00:00Z", Durability: "provisional"}
	m := memory.Memory{ID: "mem_1", Scope: "project:demo", Type: "decision", Title: "Ship it", Text: "we ship", Source: "manual", Decision: bare, DecisionStatus: "current"}
	m.Tags = []string{memory.OwnerConfirmedTagPrefix + memory.OwnerConfirmedDigest(m)}
	got := BuildContext(cfg, []memory.Memory{m}, 4000, true)
	if !strings.Contains(got, "Decision status: current (owner-confirmed; no review date set)\nAs of: 2026-11-10T00:00:00Z\n") ||
		strings.Contains(got, "Durability:") || strings.Contains(got, "Flip conditions:") {
		t.Fatalf("bare owner decision:\n%s", got)
	}

	// A review date, a flip condition, or no confirmation: the full block.
	for name, mod := range map[string]func(*memory.Memory){
		"review date": func(x *memory.Memory) {
			x.Decision = &memory.DecisionValidity{AsOf: bare.AsOf, Durability: "provisional", ReviewBy: "2020-01-01T00:00:00Z"}
		},
		"flip": func(x *memory.Memory) {
			x.Decision = &memory.DecisionValidity{AsOf: bare.AsOf, Durability: "working", FlipConditions: []string{"x"}}
		},
		"not confirmed": func(x *memory.Memory) { x.Tags = nil },
	} {
		c := m
		mod(&c)
		if c.Tags != nil {
			c.Tags = []string{memory.OwnerConfirmedTagPrefix + memory.OwnerConfirmedDigest(c)}
		}
		c.DecisionStatus = "needs_review"
		if out := BuildContext(cfg, []memory.Memory{c}, 4000, true); !strings.Contains(out, "Decision status: needs_review") || !strings.Contains(out, "Durability:") {
			t.Fatalf("%s:\n%s", name, out)
		}
	}
}
