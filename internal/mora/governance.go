package mora

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// The governance ledger is Mora's single durable record of "who decided what
// about which memory, and when" — the local-first governance/provenance layer
// that #52 (forget) and #53 (prune) both need. It lives in the vault as
// `.mora-governance.json` (sibling of the `.mora-vault.json` identity marker),
// so it rides `mora sync git` to a user's other devices and — being a dotfile,
// not a `.md` — is never parsed as a memory by the index rebuild.
//
// THE #52 TRAP (why the key shape matters): a forget must survive the hourly,
// agent-less sync that re-fetches everything in its window and re-creates any
// file missing at its stable path. The only durable key is the SOURCE-NATIVE
// atom — `{provider, stable_id | handle | address}` — NEVER the post-merge
// `person:` graph id, which MOVES as identities merge/split. Enforcement lives
// at the single connector write chokepoint (`writeMappedMemory`), so re-ingest
// cannot defeat it.

const governanceSchema = 1

// governanceFile is the ledger's on-disk name inside the vault. A dotfile so the
// index rebuild (which parses `*.md`) ignores it; NOT matched by the vault
// `.gitignore` (index.db/tokens/identity*/share/) so it survives `mora sync git`.
const governanceFile = ".mora-governance.json"

// Atom kinds — the source-native identity the ledger keys on. "host" is reserved
// for a future connector field (no source populates it yet), so it is accepted
// in the schema but derived from no Meta.
const (
	atomStableID = "stable_id" // one whole memory (a chat/thread/event)
	atomHandle   = "handle"    // an iMessage participant handle (+1…, or an email)
	atomAddress  = "address"   // a Gmail/Calendar email address
	atomHost     = "host"      // reserved; no source field yet
)

// Entry kinds — the governance operations that ride the one ledger. Only the
// suppression kinds (forget/prune/source_scope) gate the write chokepoint today;
// the rest are durable records later phases consume (redact = P16 graph-compile
// participant filter; merge_confirm = P13 confirm-queue; archive reserved).
const (
	govKindForget       = "forget"
	govKindPrune        = "prune"
	govKindSourceScope  = "source_scope"
	govKindRedact       = "redact"
	govKindMergeConfirm = "merge_confirm"
	govKindArchive      = "archive"
)

// Actions — what an entry DOES at the write chokepoint. "suppress" skips the
// write entirely (never persist); "record" is an inert durable note (corrections)
// with no write-time effect.
const (
	govActionSuppress = "suppress"
	govActionRecord   = "record"
)

// govAtom is a stable-atom key. Provider "" is a cross-provider wildcard (e.g.
// forget an email address across gmail AND calendar); a concrete provider scopes
// the match to that connector (e.g. an iMessage handle).
type govAtom struct {
	Provider string `json:"provider,omitempty"`
	Kind     string `json:"kind"`
	Value    string `json:"value"`
}

// govEntry is one durable governance decision.
type govEntry struct {
	ID     string  `json:"id"`
	Kind   string  `json:"kind"`
	Atom   govAtom `json:"atom"`
	Action string  `json:"action"`
	Reason string  `json:"reason,omitempty"`
	// Atom2/Decision carry a two-atom correction (merge_confirm): "these two
	// source-native identities are (confirm) / are not (reject) the same person".
	// Keyed by atoms, never a person: id — the #52 trap.
	Atom2     *govAtom `json:"atom2,omitempty"`
	Decision  string   `json:"decision,omitempty"`
	CreatedAt string   `json:"created_at"`
	CreatedBy string   `json:"created_by"`
	RevokedAt string   `json:"revoked_at,omitempty"` // set by unforget/undo; a revoked entry is inert
}

func (e govEntry) revoked() bool { return e.RevokedAt != "" }

// governance is the whole ledger.
type governance struct {
	Schema  int        `json:"schema"`
	Entries []govEntry `json:"entries"`
}

// activeSuppress returns the live entries that gate the write chokepoint (the
// suppression kinds, action=suppress, not revoked).
func (g governance) activeSuppress() []govEntry {
	var out []govEntry
	for _, e := range g.Entries {
		if e.revoked() || e.Action != govActionSuppress {
			continue
		}
		switch e.Kind {
		case govKindForget, govKindPrune, govKindSourceScope:
			out = append(out, e)
		}
	}
	return out
}

func governancePath(cfg Config) string { return filepath.Join(cfg.VaultDir, governanceFile) }

// loadGovernance reads the ledger. An ABSENT ledger is the common case and reads
// as an empty ledger (no error). A CORRUPT ledger FAILS LOUD: treating it as
// empty would silently resurrect forgotten content — a privacy violation. The
// error is surfaced to the write path, which counts it as a failed write
// (honest-snapshot) rather than laundering a partial/wrong sync into success.
func loadGovernance(cfg Config) (governance, error) {
	b, err := os.ReadFile(governancePath(cfg))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return governance{Schema: governanceSchema}, nil
		}
		return governance{}, err
	}
	var g governance
	if err := json.Unmarshal(b, &g); err != nil {
		return governance{}, fmt.Errorf("governance ledger %s is unreadable (corrupt JSON) — restore it from your vault backup (e.g. `mora sync git`) rather than deleting it: %w", governancePath(cfg), err)
	}
	return g, nil
}

// saveGovernance persists the ledger via atomicWrite (temp+rename). Mode 0600 —
// it names identities the user chose to forget, mildly sensitive; and the vault
// may leave the machine via `mora sync git`.
func saveGovernance(cfg Config, g governance) error {
	if g.Schema == 0 {
		g.Schema = governanceSchema
	}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(governancePath(cfg), append(b, '\n'), 0o600)
}

// appendGovernanceEntry mints id/created_at/created_by, appends, and persists.
// Returns the stored entry (with its minted id, for a later unforget/undo).
func appendGovernanceEntry(cfg Config, e govEntry) (govEntry, error) {
	g, err := loadGovernance(cfg)
	if err != nil {
		return govEntry{}, err
	}
	if e.ID == "" {
		e.ID = "gov_" + strings.TrimPrefix(newID(), "mem_")
	}
	if e.CreatedAt == "" {
		e.CreatedAt = nowRFC3339()
	}
	if e.CreatedBy == "" {
		e.CreatedBy = "mora " + BuildVersion
	}
	g.Entries = append(g.Entries, e)
	if err := saveGovernance(cfg, g); err != nil {
		return govEntry{}, err
	}
	return e, nil
}

// revokeGovernanceEntry marks an entry inert (unforget/undo). Returns whether an
// active entry with that id was found.
func revokeGovernanceEntry(cfg Config, id string) (bool, error) {
	g, err := loadGovernance(cfg)
	if err != nil {
		return false, err
	}
	found := false
	for i := range g.Entries {
		if g.Entries[i].ID == id && !g.Entries[i].revoked() {
			g.Entries[i].RevokedAt = nowRFC3339()
			found = true
		}
	}
	if !found {
		return false, nil
	}
	return true, saveGovernance(cfg, g)
}

// itemAtom is a memory's whole-item stable-atom key (exact StableID — never with
// the `@account` suffix stripped, which would over-match across accounts).
func itemAtom(provider, stableID string) govAtom {
	return govAtom{Provider: provider, Kind: atomStableID, Value: stableID}
}

// counterpartyAtoms is the memory's set of EXTERNAL identity atoms, coerced from
// Meta with the same helpers the entity graph uses (so the ledger's identity view
// matches the graph's). Sorted + deduped for determinism. iMessage yields handle
// atoms (participants exclude self); Gmail/Calendar yield address atoms across
// from/to/cc/attendees/organizer (these INCLUDE self, which is why identity
// suppression is gated on a sole counterparty — see decideSuppress).
func counterpartyAtoms(provider string, meta map[string]any) []govAtom {
	if meta == nil {
		return nil
	}
	seen := map[string]govAtom{}
	add := func(kind, raw string) {
		v := normalizeIdentity(kind, raw)
		if v == "" {
			return
		}
		seen[kind+"\x00"+v] = govAtom{Provider: provider, Kind: kind, Value: v}
	}
	switch provider {
	case "imessage":
		for _, p := range metaPairs(meta["participants"]) {
			add(atomHandle, p.handle)
		}
	default: // gmail / calendar (email-addressed connectors)
		for _, key := range []string{"from", "to", "cc", "attendees"} {
			for _, a := range metaStrings(meta[key]) {
				add(atomAddress, a)
			}
		}
		if org, ok := meta["organizer"].(string); ok {
			add(atomAddress, org)
		}
	}
	out := make([]govAtom, 0, len(seen))
	for _, a := range seen {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// normalizeIdentity applies the MINIMAL normalization a stable-atom key needs.
// Addresses and email-shaped handles are lowercased+trimmed; phone handles are
// only trimmed. Deliberately NOT phone/email canonicalization — that lives in
// canonicalizePersons and coupling to it would risk a false merge (precision-first:
// under-match rather than over-match).
func normalizeIdentity(kind, raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	if kind == atomAddress || strings.Contains(v, "@") {
		return strings.ToLower(v)
	}
	return v
}

// providerMatches reports whether an entry's atom provider matches a memory's
// provider. "" on the entry is a cross-provider wildcard.
func providerMatches(entryProvider, memProvider string) bool {
	return entryProvider == "" || entryProvider == memProvider
}

// decideSuppress is the write-chokepoint decision, pure over (ledger, memory
// fields). Returns the id of the first matching active suppression entry, or "".
//
//   - Item (stable_id) match ALWAYS suppresses the whole memory (forget a chat).
//   - Identity (handle/address) match suppresses ONLY a SOLE-COUNTERPARTY (1:1)
//     memory — the forgotten identity is the memory's only external counterparty.
//     A group thread where the identity is one of many is KEPT (the data-loss /
//     "layoff email" guard); its per-participant redaction is deferred to the
//     P16 graph-compile-time filter, recorded via a `redact` entry.
func (g governance) decideSuppress(provider, stableID string, meta map[string]any) (bool, string) {
	item := itemAtom(provider, stableID)
	cps := counterpartyAtoms(provider, meta)
	sole := len(cps) == 1
	for _, e := range g.activeSuppress() {
		a := e.Atom
		switch a.Kind {
		case atomStableID:
			if providerMatches(a.Provider, provider) && a.Value == item.Value {
				return true, e.ID
			}
		case atomHandle, atomAddress:
			if sole && cps[0].Kind == a.Kind && providerMatches(a.Provider, provider) && cps[0].Value == a.Value {
				return true, e.ID
			}
		}
	}
	return false, ""
}

// suppresses is the MappedMemory-typed decision (the connector write path).
func (g governance) suppresses(mm memory.MappedMemory) (bool, string) {
	return g.decideSuppress(mm.Provider, mm.StableID, mm.Meta)
}

// shouldSuppressWrite is the guard entry point: load the ledger (fail-closed on
// corruption) and decide. Called at the single connector write chokepoint
// (writeMappedMemory) and its one derived sibling (writeAttachmentMemories).
func shouldSuppressWrite(cfg Config, mm memory.MappedMemory) (bool, string, error) {
	g, err := loadGovernance(cfg)
	if err != nil {
		return false, "", err
	}
	sup, id := g.suppresses(mm)
	return sup, id, nil
}
