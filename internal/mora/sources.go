package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"io"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/registry"
)

// IsEnabled centralizes the nil-sentinel handling for Source.Enabled so no
// caller dereferences the raw pointer. nil (legacy/unset) is normalized to true
// by loadSources before callers ever see it; an explicit *false stays disabled (D-12).

// lookupCatalog returns the catalog entry for ctype. The bool is false for any
// type not in the static catalog — callers MUST reject unknown types with an
// error (D-03 / ASVS V5), never silently no-op.
func lookupCatalog(t string) (connectorInfo, bool)        { return registry.Lookup(t) }
func macOSOnlyConnector(t string) bool                    { return registry.MacOSOnly(t) }
func connectorCatalogForGOOS(goos string) []connectorInfo { return registry.CatalogForGOOS(goos) }
func cmdSources(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora sources add|list")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		sources, err := loadSources(cfg)
		if err != nil {
			return err
		}
		return emit(stdout, sources, true)
	case "add":
		return addSource(cfg, args[1:], stdout)
	default:
		return errors.New("usage: mora sources add|list")
	}
}

// setSourceEnabled loads sources, flips Enabled for every source whose Type
// matches ctype (D-02 per-type), and persists atomically via saveSources (0600,
// D-10 — no new storage file). If no source row exists yet for ctype, one is
// created so an auth-less type (e.g. filesystem) can be enabled before it has a
// configured source row. Mirrors the addSource load → rebuild → save idiom but
// matches by Type and flips the bit instead of replacing.
// setSourceSinceDays persists the gmail backfill window (in days) onto the
// matching source row so `connect --since-days N` carries over to later syncs.
// No-op if the source row does not exist yet.
// setSourceEnabledByName flips one source row by NAME (the multi-account
// connect path: "gmail-work" must enable exactly that mailbox's row, while the
// type-matching setSourceEnabled would flip BOTH accounts' rows — or mint a
// bogus row with the suffixed name as its Type). Errors on a missing name: the
// connect flow runs ensureGoogleSources first, so absence is a real bug.
func setSourceEnabledByName(cfg Config, name string, enabled bool) error {
	return mutateSources(cfg, func(sources []Source) ([]Source, error) {
		for i := range sources {
			if sources[i].Name == name {
				sources[i].Enabled = genericutil.Ptr(enabled)
				return sources, nil
			}
		}
		return nil, fmt.Errorf("no source named %q", name)
	})
}

// setSourceSinceDaysByName mirrors setSourceEnabledByName for the window
// override — account-scoped, never the whole type family.
func setSourceSinceDaysByName(cfg Config, name string, days int) error {
	return mutateSources(cfg, func(sources []Source) ([]Source, error) {
		for i := range sources {
			if sources[i].Name == name {
				sources[i].SinceDays = days
				return sources, nil
			}
		}
		return nil, fmt.Errorf("no source named %q", name)
	})
}
func setSourceSinceDays(cfg Config, ctype string, days int) error {
	return mutateSources(cfg, func(sources []Source) ([]Source, error) {
		for i := range sources {
			if sources[i].Type == ctype {
				sources[i].SinceDays = days
			}
		}
		return sources, nil
	})
}
func setSourceEnabled(cfg Config, ctype string, enabled bool) error {
	return mutateSources(cfg, func(sources []Source) ([]Source, error) {
		found := false
		for i := range sources {
			if sources[i].Type != ctype {
				continue
			}
			if ctype == "filesystem" && sources[i].Path == "" && enabled {
				// Never (re)activate a pathless filesystem row: it cannot ingest
				// anything and only fails the walk (a legacy phantom minted by
				// older binaries). Disabling one is still allowed.
				continue
			}
			sources[i].Enabled = genericutil.Ptr(enabled)
			found = true
		}
		if !found && enabled && ctype != "filesystem" {
			// No source row yet (e.g. imessage/applecalendar before any explicit
			// add — their implicit local store makes a bare row meaningful). Create
			// a minimal row carrying the consent bit; an explicit Enabled avoids
			// the load-time grandfather flipping a nil to true (D-11). NOT minted
			// on disable (absence already means disabled — a minted disabled row
			// would later be "found" and resurrected by enable) and NOT for
			// filesystem (a row without a path can never ingest; enableConnector
			// guides the user to configure a folder instead).
			sources = append(sources, Source{
				Name:      ctype,
				Type:      ctype,
				Scope:     "personal",
				Enabled:   genericutil.Ptr(enabled),
				CreatedAt: time.Now().Format(time.RFC3339),
			})
		}
		return sources, nil
	})
}

// hasConfiguredFilesystemSource reports whether any filesystem source row is
// actually configured — i.e. carries a non-empty Path. A pathless row (a legacy
// phantom minted by older binaries on `connectors enable filesystem`) does not
// count: it can never ingest, so enable must guide the user to configure a
// folder rather than flip it.

// containsType reports whether types contains t.

// withoutTypes returns types with each of drop removed (preserving order).

// parseCSVList splits a comma-separated input into trimmed, non-empty entries.

// setIMessageDenyList persists the deny-list onto the imessage source row in
// sources.json (creating the row if needed), so every future `mora sync imessage`
// honors it (D-07; no new config file).
func setIMessageDenyList(cfg Config, contacts, conversations []string) error {
	return mutateSources(cfg, func(sources []Source) ([]Source, error) {
		found := false
		for i := range sources {
			if sources[i].Type == "imessage" {
				sources[i].DenyContacts = contacts
				sources[i].DenyConversations = conversations
				found = true
			}
		}
		if !found {
			sources = append(sources, Source{
				Name: "imessage", Type: "imessage", Scope: "personal",
				Enabled: genericutil.Ptr(true), CreatedAt: time.Now().Format(time.RFC3339),
				DenyContacts: contacts, DenyConversations: conversations,
			})
		}
		return sources, nil
	})
}

// ensureGoogleSources registers the gmail/calendar source pair for one Google
// account. The unlabeled default keeps the legacy "gmail"/"calendar" names; a
// labeled account (multi-mailbox: personal vs business) registers
// "gmail-<label>"/"calendar-<label>" rows carrying Account=<label>, so each
// mailbox gets its own enable bit, sync status, and digest section. Existence
// is keyed by NAME (not type) so a second account is not mistaken for the
// first. New rows stay disabled (D-11); connect flips them.
func ensureGoogleSources(cfg Config, account string) error {
	return mutateSources(cfg, func(sources []Source) ([]Source, error) {
		have := map[string]bool{}
		for _, s := range sources {
			have[s.Name] = true
		}
		gmailName, calName := googleSourceNames(account)
		now := time.Now().Format(time.RFC3339)
		if !have[gmailName] {
			sources = append(sources, Source{Name: gmailName, Type: "gmail", Scope: "personal", Account: account, Enabled: genericutil.Ptr(false), CreatedAt: now})
		}
		if !have[calName] {
			sources = append(sources, Source{Name: calName, Type: "calendar", Scope: "personal", Calendar: "primary", Account: account, Enabled: genericutil.Ptr(false), CreatedAt: now})
		}
		return sources, nil
	})
}

// loadSourcesOrEmpty is loadSources with the error collapsed to "no sources" —
// for guard paths where a missing/corrupt sources file should mean "no
// conflict", never an abort.

// googleAccountForEmail reports which existing account label a Google address
// is already connected under. The re-auth guard: connecting the SAME mailbox
// under a SECOND label would double-ingest it (every thread twice, distinct
// @account StableIDs), so connect exits gracefully instead.
func hasConfiguredFilesystemSource(s []Source) bool { return registry.HasConfiguredFilesystemSource(s) }
func containsType(types []string, t string) bool    { return registry.ContainsType(types, t) }
func withoutTypes(types []string, drop ...string) []string {
	return registry.WithoutTypes(types, drop...)
}
func parseCSVList(s string) []string                    { return registry.ParseCSVList(s) }
func isValidAccountLabel(s string) bool                 { return registry.ValidAccountLabel(s) }
func googleSourceNames(account string) (string, string) { return registry.GoogleSourceNames(account) }
func googleAccountForEmail(s []Source, email string) (string, bool) {
	return registry.GoogleAccountForEmail(s, email)
}

func loadSources(cfg Config) ([]Source, error)       { return registry.LoadSources(cfg) }
func saveSources(cfg Config, sources []Source) error { return registry.SaveSources(cfg, sources) }
func loadSourcesOrEmpty(cfg Config) []Source         { return registry.LoadSourcesOrEmpty(cfg) }

// setSourceEmailByAccount stamps the signed-in address onto an account's
// gmail/calendar rows (the guard's lookup data).
func setSourceEmailByAccount(cfg Config, account, email string) error {
	return mutateSources(cfg, func(sources []Source) ([]Source, error) {
		for i := range sources {
			if (sources[i].Type == "gmail" || sources[i].Type == "calendar") && sources[i].Account == account {
				sources[i].Email = email
			}
		}
		return sources, nil
	})
}

// sourceFreshlySynced reports whether a source completed a CLEAN sync within
// the window — the connect-path skip guard, so a re-auth minutes after a full
// backfill doesn't re-pull the whole window again. Reads LastSuccessAt (the
// field that survives an aborted attempt without advancing).
func sourceFreshlySynced(cfg Config, s Source, within time.Duration, now time.Time) bool {
	st, err := memory.LoadStatus(syncStatusPathFor(cfg, s))
	if err != nil || st == nil || st.LastSuccessAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, st.LastSuccessAt)
	if err != nil {
		return false
	}
	return now.Sub(t) < within
}

// isValidAccountLabel gates `--account`: lowercase letters, digits, hyphens —
// the label lands in filenames (tokens/google-<label>.json, source names,
// sync-status paths), so it must be path-safe by construction.

// googleSourceNames maps an account label to its gmail/calendar source names.
func addSource(cfg Config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora sources add <filesystem|gmail|calendar|gdrive> [flags]")
	}
	stype := args[0]
	fs := flag.NewFlagSet("sources add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", stype, "source name")
	scope := fs.String("scope", "personal", "scope")
	path := fs.String("path", "", "path")
	label := fs.String("label", "", "gmail label")
	cal := fs.String("calendar", "", "calendar")
	folder := fs.String("folder", "", "drive folder id")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// New sources are consent-gated per TYPE: a row added under a type the user
	// never enabled defaults disabled (D-11), while a row added under an
	// already-enabled type inherits that consent and starts enabled — consent is
	// granted at type granularity (D-02: `connectors enable` flips every row of
	// the type), so re-running enable would flip the new row anyway, and landing
	// it disabled only produced the "run `mora connectors enable <type>` first"
	// dead end for a command the user had ALREADY run. Enabled is always set
	// explicitly (never nil) so the grandfather migration in loadSources (which
	// normalizes nil => true for pre-Enabled legacy sources) cannot silently
	// auto-enable a freshly added source on the next load.
	s := Source{Name: *name, Type: stype, Scope: *scope, Path: genericutil.ExpandHome(*path), Label: *label, Calendar: *cal, FolderID: *folder, CreatedAt: time.Now().Format(time.RFC3339)}
	if s.Type == "filesystem" && s.Path == "" {
		return errors.New("filesystem source requires --path")
	}
	// Serialize the read-modify-write (P3): mutateSources reloads inside the lease
	// so a concurrent writer's change is not clobbered. Unlike the old swallowed
	// load error, a corrupt registry now fails loudly instead of being replaced by
	// only this new source (matching connectFilesystem's long-standing guard).
	// The consent-inheritance check reads the SAME freshly loaded snapshot, so it
	// cannot race a concurrent enable/disable.
	if err := mutateSources(cfg, func(sources []Source) ([]Source, error) {
		typeEnabled := false
		var next []Source
		for _, existing := range sources {
			if existing.Type == s.Type && existing.IsEnabled() {
				typeEnabled = true
			}
			if existing.Name != s.Name {
				next = append(next, existing)
			}
		}
		s.Enabled = genericutil.Ptr(typeEnabled)
		next = append(next, s)
		return next, nil
	}); err != nil {
		return err
	}
	return emit(stdout, s, true)
}

// saveSources persists the source registry. atomicWrite stages through a unique
// temp per writer, so this is safe against the temp-collision race (two writers
// clobbering a shared `.tmp`). It is NOT the serialization boundary for the
// higher-level read-modify-write: two callers each doing load → mutate → save
// could otherwise still lose an update. That serialization lives in
// mutateSources / acquireSourcesLock (sources_lock.go); every load → mutate →
// save on sources.json MUST go through one of them. Call saveSources directly
// only while already holding the sources lease (mutateSources does).
