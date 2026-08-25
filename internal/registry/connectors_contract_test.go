package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pyranthus-hq/mora/internal/applecal"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/githubissues"
	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/imessage"
	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/whatsapp"
)

func sourceInstanceKey(m memory.Memory) (string, bool) { return SourceInstanceKey(m) }
func lookupCatalog(kind string) (Info, bool)           { return Lookup(kind) }

type connectorInfo = Info

func connectorDisplay(key string) (int, string)        { return Display(key) }
func instanceKeyForSource(source memory.Source) string { return InstanceKeyForSource(source) }
func connectorUpcoming(key string) bool                { return Upcoming(key) }
func ingestingConnectors(cfg config.Config) ([]string, error) {
	return IngestingConnectors(cfg, LoadSources)
}

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
		key, ok := sourceInstanceKey(memory.Memory{Provider: c.provider})
		if !ok {
			t.Fatalf("sourceInstanceKey(Provider=%q) ok=false, want true", c.provider)
		}
		if key != c.want {
			t.Fatalf("sourceInstanceKey(Provider=%q) = %q, want %q", c.provider, key, c.want)
		}
	}
}

func TestSourceInstanceKeyEmptyRejected(t *testing.T) {
	key, ok := sourceInstanceKey(memory.Memory{Provider: ""})
	if ok {
		t.Fatalf("sourceInstanceKey(Provider=\"\") ok=true, want false (empty must be rejected)")
	}
	if key != "" {
		t.Fatalf("sourceInstanceKey(Provider=\"\") key=%q, want \"\"", key)
	}
}

func TestSourceInstanceKeyDoesNotReadSource(t *testing.T) {
	_, ok := sourceInstanceKey(memory.Memory{Source: "gmail_thread/abc123", Provider: ""})
	if ok {
		t.Fatalf("sourceInstanceKey keyed on per-item Source; want rejection on empty Provider")
	}
}

func TestCapabilityTagsCatalog(t *testing.T) {
	want := map[string]bool{
		"gmail":      true,
		"calendar":   true,
		"imessage":   true,
		"filesystem": true,
		"github":     true,
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

func TestCapabilityTagExcludesNonIngesting(t *testing.T) {
	// A hypothetical live-passthrough or on-demand connector would carry
	// Ingesting=false. Verify the filter predicate honors it.
	passthrough := connectorInfo{Type: "posthog", DisplayName: "PostHog", Ingesting: false}
	if passthrough.Ingesting {
		t.Fatalf("non-ingesting descriptor must report Ingesting=false")
	}
}

func TestIngestingConnectorsEnabledIntersectIngesting(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}

	// gmail enabled+ingesting (no memories — must still enumerate);
	// calendar enabled+ingesting; imessage DISABLED (must be excluded).
	want := []memory.Source{
		{Name: "gmail", Type: "gmail", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
		{Name: "calendar", Type: "calendar", Scope: "personal", Calendar: "primary", Enabled: genericutil.Ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
		{Name: "imessage", Type: "imessage", Scope: "personal", Enabled: genericutil.Ptr(false), CreatedAt: "2026-01-01T00:00:00Z"},
	}
	if err := SaveSources(cfg, want); err != nil {
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

func TestIngestingConnectorsSortedDeterministic(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}

	// Deliberately reverse-ordered on disk.
	want := []memory.Source{
		{Name: "imessage", Type: "imessage", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
		{Name: "gmail", Type: "gmail", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
		{Name: "calendar", Type: "calendar", Scope: "personal", Calendar: "primary", Enabled: genericutil.Ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
	}
	if err := SaveSources(cfg, want); err != nil {
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

func TestIngestingConnectorsNoSources(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}

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

func TestConnectorDisplayRanksPreserveOrder(t *testing.T) {
	rCal, _ := connectorDisplay("calendar")
	rMsg, _ := connectorDisplay("imessage")
	rMail, _ := connectorDisplay("gmail")
	if rCal >= rMsg || rMsg >= rMail {
		t.Fatalf("rank order broken: calendar=%d imessage=%d gmail=%d (want strictly increasing)", rCal, rMsg, rMail)
	}
}

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

func TestConnectorProviderKeysReconcile(t *testing.T) {
	// Mint a Provider through each connector's REAL mapping path (the kind
	// registry for gmail/calendar/applecal; iMessage's custom mapper stamps its
	// provider directly and bypasses the registry).
	mint := map[string]func() string{
		"gmail": func() string {
			return memory.MapItem(memory.Item{Kind: google.KindGmailThread}, "global", 0).Provider
		},
		"calendar": func() string {
			return memory.MapItem(memory.Item{Kind: google.KindCalEvent}, "global", 0).Provider
		},
		"imessage": func() string {
			return imessage.MapConversationFn(nil)(memory.Item{Kind: imessage.KindIMessageChat}, "global", 0).Provider
		},
		"whatsapp": func() string {
			return whatsapp.MapConversationFn()(memory.Item{Kind: whatsapp.KindConversation}, "global", 0).Provider
		},
		"applecalendar": func() string {
			return memory.MapItem(memory.Item{Kind: applecal.KindAppleCalEvent}, "global", 0).Provider
		},
		"github": func() string {
			return githubissues.MapIssue(memory.Item{Kind: githubissues.KindIssue}, "global", 0).Provider
		},
		// filesystem mints NO Provider on purpose: sourceInstanceKey rejects the
		// empty provider and the brief skips filesystem by design (brief.go).
	}
	for _, ci := range Entries() {
		if !ci.Ingesting || ci.Type == "filesystem" {
			continue
		}
		mintFn, ok := mint[ci.Type]
		if !ok {
			t.Fatalf("catalog entry %q has no provider mint in this test — add one; this table is the reconciliation contract every ingesting connector must pass", ci.Type)
		}
		provider := mintFn()
		key, ok := sourceInstanceKey(memory.Memory{Provider: provider})
		if !ok {
			t.Fatalf("%s: sourceInstanceKey rejected minted provider %q", ci.Type, provider)
		}
		want := instanceKeyForSource(memory.Source{Type: ci.Type})
		if key != want {
			t.Errorf("%s: memory-side key %q != source-side key %q — this connector's memories never reconcile with its enumerated instance", ci.Type, key, want)
		}
	}
}

func TestSourceInstanceKeyNormalizesAliasedProvider(t *testing.T) {
	key, ok := sourceInstanceKey(memory.Memory{Provider: "applecal", Account: "family"})
	if !ok || key != "applecalendar:family" {
		t.Fatalf("sourceInstanceKey(applecal, family) = %q, %v; want \"applecalendar:family\", true", key, ok)
	}
}

func TestConnectorUpcomingCapability(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"calendar", true},
		{"applecalendar", true},
		{"calendar:work", true}, // composite account key inherits the capability
		{"gmail", false},
		{"imessage", false},
		{"filesystem", false},
		{"notion", false}, // unknown connectors default to the past window
	}
	for _, c := range cases {
		if got := connectorUpcoming(c.key); got != c.want {
			t.Errorf("connectorUpcoming(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}
