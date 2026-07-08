package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// IsEnabled centralizes the nil-sentinel handling for Source.Enabled so no
// caller dereferences the raw pointer. nil (legacy/unset) is normalized to true
// by loadSources before callers ever see it; an explicit *false stays disabled (D-12).
func (s Source) IsEnabled() bool { return s.Enabled != nil && *s.Enabled }

// lookupCatalog returns the catalog entry for ctype. The bool is false for any
// type not in the static catalog — callers MUST reject unknown types with an
// error (D-03 / ASVS V5), never silently no-op.
func lookupCatalog(ctype string) (connectorInfo, bool) {
	for _, c := range connectorCatalog {
		if c.Type == ctype {
			return c, true
		}
	}
	return connectorInfo{}, false
}
func macOSOnlyConnector(ctype string) bool {
	switch ctype {
	case "imessage", "applecalendar", "addressbook":
		return true
	default:
		return false
	}
}
func connectorCatalogForGOOS(goos string) []connectorInfo {
	if goos != "windows" {
		return connectorCatalog
	}
	out := make([]connectorInfo, 0, len(connectorCatalog))
	for _, c := range connectorCatalog {
		if !macOSOnlyConnector(c.Type) {
			out = append(out, c)
		}
	}
	return out
}
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
				sources[i].Enabled = ptr(enabled)
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
			if sources[i].Type == ctype {
				sources[i].Enabled = ptr(enabled)
				found = true
			}
		}
		if !found {
			// No source row yet (e.g. filesystem before any `sources add`). Create a
			// minimal row carrying the consent bit; an explicit Enabled avoids the
			// load-time grandfather flipping a nil to true (D-11).
			sources = append(sources, Source{
				Name:      ctype,
				Type:      ctype,
				Scope:     "personal",
				Enabled:   ptr(enabled),
				CreatedAt: time.Now().Format(time.RFC3339),
			})
		}
		return sources, nil
	})
}

// containsType reports whether types contains t.
func containsType(types []string, t string) bool {
	for _, x := range types {
		if x == t {
			return true
		}
	}
	return false
}

// withoutTypes returns types with each of drop removed (preserving order).
func withoutTypes(types []string, drop ...string) []string {
	out := types[:0:0]
	for _, x := range types {
		if !containsType(drop, x) {
			out = append(out, x)
		}
	}
	return out
}

// parseCSVList splits a comma-separated input into trimmed, non-empty entries.
func parseCSVList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

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
				Enabled: ptr(true), CreatedAt: time.Now().Format(time.RFC3339),
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
			sources = append(sources, Source{Name: gmailName, Type: "gmail", Scope: "personal", Account: account, Enabled: ptr(false), CreatedAt: now})
		}
		if !have[calName] {
			sources = append(sources, Source{Name: calName, Type: "calendar", Scope: "personal", Calendar: "primary", Account: account, Enabled: ptr(false), CreatedAt: now})
		}
		return sources, nil
	})
}

// loadSourcesOrEmpty is loadSources with the error collapsed to "no sources" —
// for guard paths where a missing/corrupt sources file should mean "no
// conflict", never an abort.
func loadSourcesOrEmpty(cfg Config) []Source {
	sources, err := loadSources(cfg)
	if err != nil {
		return nil
	}
	return sources
}

// googleAccountForEmail reports which existing account label a Google address
// is already connected under. The re-auth guard: connecting the SAME mailbox
// under a SECOND label would double-ingest it (every thread twice, distinct
// @account StableIDs), so connect exits gracefully instead.
func googleAccountForEmail(sources []Source, email string) (label string, found bool) {
	if email == "" {
		return "", false
	}
	for _, s := range sources {
		if (s.Type == "gmail" || s.Type == "calendar") && s.Email != "" && strings.EqualFold(s.Email, email) {
			return s.Account, true
		}
	}
	return "", false
}

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
func isValidAccountLabel(label string) bool {
	for _, r := range label {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return label != ""
}

// googleSourceNames maps an account label to its gmail/calendar source names.
func googleSourceNames(account string) (gmail, calendar string) {
	if account == "" {
		return "gmail", "calendar"
	}
	return "gmail-" + account, "calendar-" + account
}
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
	// New sources are consent-gated: default-disabled (D-11). Enabled is set
	// explicitly to false so the grandfather migration in loadSources (which
	// normalizes nil => true for pre-Enabled legacy sources) cannot silently
	// auto-enable a freshly added source on the next load.
	s := Source{Name: *name, Type: stype, Scope: *scope, Path: expandHome(*path), Label: *label, Calendar: *cal, FolderID: *folder, Enabled: ptr(false), CreatedAt: time.Now().Format(time.RFC3339)}
	if s.Type == "filesystem" && s.Path == "" {
		return errors.New("filesystem source requires --path")
	}
	// Serialize the read-modify-write (P3): mutateSources reloads inside the lease
	// so a concurrent writer's change is not clobbered. Unlike the old swallowed
	// load error, a corrupt registry now fails loudly instead of being replaced by
	// only this new source (matching connectFilesystem's long-standing guard).
	if err := mutateSources(cfg, func(sources []Source) ([]Source, error) {
		var next []Source
		for _, existing := range sources {
			if existing.Name != s.Name {
				next = append(next, existing)
			}
		}
		next = append(next, s)
		return next, nil
	}); err != nil {
		return err
	}
	return emit(stdout, s, true)
}
func loadSources(cfg Config) ([]Source, error) {
	path := filepath.Join(cfg.ConfigDir, "sources.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var sources []Source
	if err := json.Unmarshal(b, &sources); err != nil {
		return nil, err
	}
	// Grandfather migration (D-12): a missing `enabled` key means a pre-Enabled
	// binary wrote this source, i.e. the user had already explicitly added it —
	// treat absence as prior consent and normalize nil => true. An explicit
	// `false` is preserved as disabled (it is non-nil, so the loop skips it).
	for i := range sources {
		if sources[i].Enabled == nil {
			sources[i].Enabled = ptr(true)
		}
	}
	return sources, nil
}

// saveSources persists the source registry. atomicWrite stages through a unique
// temp per writer, so this is safe against the temp-collision race (two writers
// clobbering a shared `.tmp`). It is NOT the serialization boundary for the
// higher-level read-modify-write: two callers each doing load → mutate → save
// could otherwise still lose an update. That serialization lives in
// mutateSources / acquireSourcesLock (sources_lock.go); every load → mutate →
// save on sources.json MUST go through one of them. Call saveSources directly
// only while already holding the sources lease (mutateSources does).
func saveSources(cfg Config, sources []Source) error {
	b, err := json.MarshalIndent(sources, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(cfg.ConfigDir, "sources.json"), append(b, '\n'), 0o600)
}
