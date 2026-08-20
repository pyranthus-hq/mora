// Package registry owns the static connector catalog and deterministic connector-instance identity.
package registry

import (
	"sort"
	"strings"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
)

type Info struct {
	Type        string
	DisplayName string
	NeedsAuth   bool
	Ingesting   bool
	Rank        int
	Label       string
	// Provider is the memory-side Provider this connector's mapper mints in
	// frontmatter when it differs from Type (applecalendar mints "applecal").
	// Empty means Provider == Type. The alias is applied at LOOKUP boundaries
	// only (providerToType / sourceInstanceKey in connectors.go) — on-disk
	// frontmatter is never rewritten to "fix" a mismatch.
	// TestConnectorProviderKeysReconcile enforces the round-trip for every
	// ingesting entry.
	Provider string
	// Upcoming marks connectors whose items are future-dated events: cold-start
	// courtesy windows look FORWARD (next 7d) instead of back. Capability DATA
	// here, never a provider-string heuristic in digest code (the old
	// HasPrefix(key, "calendar") silently missed applecalendar).
	Upcoming bool
}

var catalog = []Info{
	{Type: "gmail", DisplayName: "Gmail", NeedsAuth: true, Ingesting: true, Rank: 2, Label: "Emails"},
	{Type: "calendar", DisplayName: "Google Calendar", NeedsAuth: true, Ingesting: true, Rank: 0, Label: "Calendar", Upcoming: true},
	{Type: "filesystem", DisplayName: "Filesystem", NeedsAuth: false, Ingesting: true, Rank: 3, Label: "Files"},
	// iMessage: default-disabled, no OAuth — the real gate is macOS Full Disk
	// Access (surfaced by `mora doctor`), not a login (D-11, Surface 1).
	{Type: "imessage", DisplayName: "iMessage", NeedsAuth: false, Ingesting: true, Rank: 1, Label: "Texts"},
	// Apple Calendar: same gate story as iMessage (local store + Full Disk
	// Access, no login). Rank ties with Google Calendar break on the key, so
	// both calendar sections lead the digest together. Its mapper mints
	// Provider "applecal" (internal/applecal), so the entry carries the alias —
	// without it, applecal memories never reconcile with this instance and
	// silently vanish from the delta brief.
	{Type: "applecalendar", DisplayName: "Apple Calendar", NeedsAuth: false, Ingesting: true, Rank: 0, Label: "Calendar (Apple)", Provider: "applecal", Upcoming: true},
	{Type: "github", DisplayName: "GitHub Issues", NeedsAuth: false, Ingesting: true, Rank: 4, Label: "GitHub Issues"},
}

const UnknownRank = 100

// Entries returns a defensive copy of the ordered static catalog.
func Entries() []Info { return append([]Info(nil), catalog...) }

func Lookup(ctype string) (Info, bool) {
	for _, c := range catalog {
		if c.Type == ctype {
			return c, true
		}
	}
	return Info{}, false
}

func MacOSOnly(ctype string) bool {
	switch ctype {
	case "imessage", "applecalendar", "addressbook":
		return true
	default:
		return false
	}
}

func CatalogForGOOS(goos string) []Info {
	if goos != "windows" {
		return Entries()
	}
	out := make([]Info, 0, len(catalog))
	for _, c := range catalog {
		if !MacOSOnly(c.Type) {
			out = append(out, c)
		}
	}
	return out
}

func SourceInstanceKey(m memory.Memory) (string, bool) {
	if m.Provider == "" {
		return "", false
	}
	// Normalize the memory-side Provider onto its catalog Type (applecal →
	// applecalendar) so this key reconciles with InstanceKeyForSource by
	// construction. Alias at the lookup boundary ONLY — frontmatter on disk
	// keeps the provider the connector minted.
	provider := ProviderToType(m.Provider)
	// Multi-account (the seam this comment block promised): a labeled account
	// composes "provider:account" so each mailbox gets its own watermark bucket,
	// digest section, and three-state — never collapsed into the default's.
	if m.Account != "" {
		return provider + ":" + m.Account, true
	}
	return provider, true
}

func ProviderToType(provider string) string {
	for _, c := range catalog {
		if c.Provider != "" && c.Provider == provider {
			return c.Type
		}
	}
	return provider
}

func InstanceKeyForSource(s memory.Source) string {
	if s.Account != "" {
		return s.Type + ":" + s.Account
	}
	return s.Type
}

func IngestingConnectors(cfg config.Config, loadSources func(config.Config) ([]memory.Source, error)) ([]string, error) {
	sources, err := loadSources(cfg)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		if !s.IsEnabled() {
			continue
		}
		ci, ok := Lookup(s.Type)
		if !ok || !ci.Ingesting {
			continue
		}
		key := InstanceKeyForSource(s)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out) // byte-stable determinism invariant
	return out, nil
}

func Display(instanceKey string) (rank int, label string) {
	// 1. exact catalog match.
	if ci, ok := Lookup(instanceKey); ok {
		return ci.Rank, ci.Label
	}
	// 2. composite "provider:account" → resolve by provider prefix, with the
	//    account label appended so two mailboxes never render as one "Emails".
	if i := strings.IndexByte(instanceKey, ':'); i > 0 {
		if ci, ok := Lookup(instanceKey[:i]); ok {
			return ci.Rank, ci.Label + " (" + instanceKey[i+1:] + ")"
		}
	}
	// 3. unknown: shared stable rank + clean derived label.
	return UnknownRank, CleanLabel(instanceKey)
}

func Upcoming(instanceKey string) bool {
	if ci, ok := Lookup(instanceKey); ok {
		return ci.Upcoming
	}
	if i := strings.IndexByte(instanceKey, ':'); i > 0 {
		if ci, ok := Lookup(instanceKey[:i]); ok {
			return ci.Upcoming
		}
	}
	return false
}

func CleanLabel(key string) string {
	if key == "" {
		return "Other"
	}
	r := []rune(key)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}
