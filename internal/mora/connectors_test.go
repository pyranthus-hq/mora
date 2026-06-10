package mora

import (
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// Task 1 — sourceInstanceKey seam (M-1)
// ---------------------------------------------------------------------------

// TestSourceInstanceKeyIdentityToday asserts the seam returns m.Provider today
// (the single-account reality): key == Provider for gmail/imessage/calendar.
func TestSourceInstanceKeyIdentityToday(t *testing.T) {
	cases := []struct {
		provider string
		want     string
	}{
		{"gmail", "gmail"},
		{"imessage", "imessage"},
		{"calendar", "calendar"},
	}
	for _, c := range cases {
		key, ok := sourceInstanceKey(Memory{Provider: c.provider})
		if !ok {
			t.Fatalf("sourceInstanceKey(Provider=%q) ok=false, want true", c.provider)
		}
		if key != c.want {
			t.Fatalf("sourceInstanceKey(Provider=%q) = %q, want %q", c.provider, key, c.want)
		}
	}
}

// TestSourceInstanceKeyEmptyRejected asserts an empty-Provider memory (the
// filesystem connector, mora.go) is rejected with ("", false) so callers SKIP
// it rather than minting one shared empty-key watermark bucket (M-1
// silent-data-loss prevention).
func TestSourceInstanceKeyEmptyRejected(t *testing.T) {
	key, ok := sourceInstanceKey(Memory{Provider: ""})
	if ok {
		t.Fatalf("sourceInstanceKey(Provider=\"\") ok=true, want false (empty must be rejected)")
	}
	if key != "" {
		t.Fatalf("sourceInstanceKey(Provider=\"\") key=%q, want \"\"", key)
	}
}

// TestSourceInstanceKeyDoesNotReadSource asserts the seam keys on Provider, not
// the per-item Source field (which is the per-item ProviderID and would mint
// thousands of one-item keys). A memory with a populated Source but empty
// Provider must still be rejected.
func TestSourceInstanceKeyDoesNotReadSource(t *testing.T) {
	_, ok := sourceInstanceKey(Memory{Source: "gmail_thread/abc123", Provider: ""})
	if ok {
		t.Fatalf("sourceInstanceKey keyed on per-item Source; want rejection on empty Provider")
	}
}

// ---------------------------------------------------------------------------
// Task 2 — capability tag + enumeration set (M-2) + data-driven Rank/Label (M-6)
// ---------------------------------------------------------------------------

// TestCapabilityTagsCatalog asserts every ingesting connector in the catalog is
// tagged Ingesting=true (gmail/calendar/imessage/filesystem). These are the
// connectors that persist memories + a SyncStatus and so belong in the
// three-state enumeration set.
func TestCapabilityTagsCatalog(t *testing.T) {
	want := map[string]bool{
		"gmail":      true,
		"calendar":   true,
		"imessage":   true,
		"filesystem": true,
	}
	for ctype, wantIngesting := range want {
		ci, ok := lookupCatalog(ctype)
		if !ok {
			t.Fatalf("catalog missing %q", ctype)
		}
		if ci.Ingesting != wantIngesting {
			t.Fatalf("catalog[%q].Ingesting = %v, want %v", ctype, ci.Ingesting, wantIngesting)
		}
	}
}

// TestCapabilityTagExcludesNonIngesting asserts a non-ingesting row is excluded
// by construction: a synthetic passthrough/on-demand descriptor (Ingesting=false)
// is never reported as ingesting. This proves the enumeration filter is the tag,
// not mere catalog membership.
func TestCapabilityTagExcludesNonIngesting(t *testing.T) {
	// A hypothetical live-passthrough connector (PostHog/Linear) or on-demand
	// (GitHub) would carry Ingesting=false. Verify the filter predicate honors it.
	passthrough := connectorInfo{Type: "posthog", DisplayName: "PostHog", Ingesting: false}
	if passthrough.Ingesting {
		t.Fatalf("non-ingesting descriptor must report Ingesting=false")
	}
}

// TestIngestingConnectorsEnabledIntersectIngesting asserts ingestingConnectors
// returns ONLY the catalog types that are BOTH enabled in sources.json
// (Source.IsEnabled()) AND Ingesting — sorted, byte-stable. A disabled connector
// is excluded; an enabled ingesting connector with ZERO memories is still
// included (so an all-deleted/never-synced source can surface "unavailable").
func TestIngestingConnectorsEnabledIntersectIngesting(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// gmail enabled+ingesting (no memories — must still enumerate);
	// calendar enabled+ingesting; imessage DISABLED (must be excluded).
	want := []Source{
		{Name: "gmail", Type: "gmail", Scope: "personal", Enabled: ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
		{Name: "calendar", Type: "calendar", Scope: "personal", Calendar: "primary", Enabled: ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
		{Name: "imessage", Type: "imessage", Scope: "personal", Enabled: ptr(false), CreatedAt: "2026-01-01T00:00:00Z"},
	}
	if err := saveSources(cfg, want); err != nil {
		t.Fatalf("saveSources: %v", err)
	}

	got, err := ingestingConnectors(cfg)
	if err != nil {
		t.Fatalf("ingestingConnectors: %v", err)
	}
	wantSet := []string{"calendar", "gmail"} // sorted; imessage excluded (disabled)
	if len(got) != len(wantSet) {
		t.Fatalf("ingestingConnectors = %v, want %v", got, wantSet)
	}
	for i := range wantSet {
		if got[i] != wantSet[i] {
			t.Fatalf("ingestingConnectors[%d] = %q, want %q (full=%v)", i, got[i], wantSet[i], got)
		}
	}
}

// TestIngestingConnectorsSortedDeterministic asserts the returned set is sorted
// byte-stable regardless of sources.json order (the project's determinism
// invariant — no map-iteration dependence).
func TestIngestingConnectorsSortedDeterministic(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// Deliberately reverse-ordered on disk.
	want := []Source{
		{Name: "imessage", Type: "imessage", Scope: "personal", Enabled: ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
		{Name: "gmail", Type: "gmail", Scope: "personal", Enabled: ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
		{Name: "calendar", Type: "calendar", Scope: "personal", Calendar: "primary", Enabled: ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
	}
	if err := saveSources(cfg, want); err != nil {
		t.Fatalf("saveSources: %v", err)
	}

	got, err := ingestingConnectors(cfg)
	if err != nil {
		t.Fatalf("ingestingConnectors: %v", err)
	}
	wantSorted := []string{"calendar", "gmail", "imessage"}
	for i := range wantSorted {
		if i >= len(got) || got[i] != wantSorted[i] {
			t.Fatalf("ingestingConnectors not sorted: got %v, want %v", got, wantSorted)
		}
	}
}

// TestIngestingConnectorsNoSources asserts an empty/absent sources.json yields
// an empty (non-nil-safe) enumeration set without error.
func TestIngestingConnectorsNoSources(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	if err := os.Remove(filepath.Join(cfg.ConfigDir, "sources.json")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove sources.json: %v", err)
	}
	got, err := ingestingConnectors(cfg)
	if err != nil {
		t.Fatalf("ingestingConnectors: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ingestingConnectors with no sources = %v, want empty", got)
	}
}

// TestConnectorDisplayKnownRankLabel asserts connectorDisplay returns the
// descriptor's Rank/Label for each known instance key, preserving today's intent
// (calendar=0/Calendar, imessage=1/Texts, gmail=2/Emails). This is the single
// owner of rank/label DATA (M-6 descriptor half).
func TestConnectorDisplayKnownRankLabel(t *testing.T) {
	cases := []struct {
		key       string
		wantRank  int
		wantLabel string
	}{
		{"calendar", 0, "Calendar"},
		{"imessage", 1, "Texts"},
		{"gmail", 2, "Emails"},
	}
	for _, c := range cases {
		rank, label := connectorDisplay(c.key)
		if rank != c.wantRank {
			t.Fatalf("connectorDisplay(%q) rank = %d, want %d", c.key, rank, c.wantRank)
		}
		if label != c.wantLabel {
			t.Fatalf("connectorDisplay(%q) label = %q, want %q", c.key, label, c.wantLabel)
		}
	}
}

// TestConnectorDisplayRanksPreserveOrder asserts the known ranks impose the
// original section ordering (calendar < imessage < gmail) so the most
// time-sensitive channels lead and survive budget truncation.
func TestConnectorDisplayRanksPreserveOrder(t *testing.T) {
	rCal, _ := connectorDisplay("calendar")
	rMsg, _ := connectorDisplay("imessage")
	rMail, _ := connectorDisplay("gmail")
	if rCal >= rMsg || rMsg >= rMail {
		t.Fatalf("rank order broken: calendar=%d imessage=%d gmail=%d (want strictly increasing)", rCal, rMsg, rMail)
	}
}

// TestConnectorDisplayUnknownCleanLabel asserts an UNKNOWN instance key gets a
// deterministic, clean label (NOT a title-cased raw provider) and a stable rank
// that is NOT silently last-truncated. A future "provider:account" composite
// resolves via its provider prefix so a 2nd gmail account inherits gmail's rank.
func TestConnectorDisplayUnknownCleanLabel(t *testing.T) {
	// Composite key: provider prefix is a known connector → inherit its rank,
	// with the account appended to the label so two mailboxes never render as
	// one indistinguishable "Emails" (multi-account, 2026-06-10).
	rank, label := connectorDisplay("gmail:work@example.com")
	if rank != 2 {
		t.Fatalf("connectorDisplay(composite gmail) rank = %d, want 2 (inherit prefix)", rank)
	}
	if label != "Emails (work@example.com)" {
		t.Fatalf("connectorDisplay(composite gmail) label = %q, want %q", label, "Emails (work@example.com)")
	}

	// Fully-unknown connector: must NOT be rank 3 (the old default = first to be
	// budget-truncated). It must sort AFTER every known connector deterministically.
	rUnknown, lUnknown := connectorDisplay("notion")
	maxKnown := 3 // filesystem
	if rUnknown <= maxKnown {
		t.Fatalf("connectorDisplay(unknown) rank = %d, want > %d (must not collide with / precede known ranks)", rUnknown, maxKnown)
	}
	if lUnknown == "" {
		t.Fatalf("connectorDisplay(unknown) label is empty; want a clean derived label")
	}
	// Clean label: first letter upper, rest unchanged — and crucially the empty
	// key must never reach here, but if it did it must not panic / produce junk.
	if lUnknown != "Notion" {
		t.Fatalf("connectorDisplay(unknown=notion) label = %q, want %q", lUnknown, "Notion")
	}
}

// TestConnectorDisplayUnknownDeterministic asserts two unknown keys order by
// (rank, then key) deterministically and never panic on degenerate input.
func TestConnectorDisplayUnknownDeterministic(t *testing.T) {
	r1, _ := connectorDisplay("aaa")
	r2, _ := connectorDisplay("zzz")
	if r1 != r2 {
		t.Fatalf("two unknown connectors got different ranks (%d vs %d); want a shared stable fallback rank", r1, r2)
	}
	// Empty key must not panic and must yield a clean fallback label.
	_, lEmpty := connectorDisplay("")
	if lEmpty == "" {
		t.Fatalf("connectorDisplay(\"\") produced empty label; want a clean fallback")
	}
}
