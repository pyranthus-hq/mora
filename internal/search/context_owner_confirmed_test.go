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
