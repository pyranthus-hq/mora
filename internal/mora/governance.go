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
	"time"

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

// merge_confirm decisions — the two verdicts the P13 one-tap confirm-queue records
// about a source-atom pair. "confirm" unifies the two identities in the graph build;
// "reject" pins them apart (never re-proposed, never merged). A later entry on the
// same pair supersedes an earlier one (last-writer-wins).
const (
	mergeDecisionConfirm = "confirm"
	mergeDecisionReject  = "reject"
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

// governanceLockPath is the ledger's cross-process lease file, co-located with
// the ledger so breakLock's os.Rename stays atomic (same filesystem). It is a
// `*.lock` file, excluded by the vault .gitignore, so a leftover lease never
// rides `mora sync git`.
func governanceLockPath(cfg Config) string { return governancePath(cfg) + ".lock" }

// acquireGovernanceLock serializes a read-modify-write of the governance ledger
// across processes, exactly as acquireSourcesLock does for sources.json: an
// unlocked load→append→save races (the last save clobbers the other writer's
// entry, and a dropped forget suppression silently resurrects forgotten content).
// It reuses the crash-safe lease primitives (publishLockFile / reapStaleLockTTL /
// loopLockReleaser) and the sources lease's TTL + jittered backoff — the same
// single-host, single-user model (a manual `mora forget` racing another forget,
// or the forward-declared scheduled prune #53). It returns a real error, never a
// silent no-op, if the lease cannot be taken; the returned release is idempotent.
func acquireGovernanceLock(cfg Config, now time.Time) (release func(), err error) {
	if err := os.MkdirAll(cfg.VaultDir, 0o700); err != nil {
		return nil, err
	}
	lockPath := governanceLockPath(cfg)
	body, _ := json.Marshal(loopLockBody{PID: os.Getpid(), AcquiredAt: now.UTC().Format(time.RFC3339)})
	for attempt := 0; attempt < maxSourcesAcquireAttempts; attempt++ {
		published, perr := publishLockFile(lockPath, body)
		switch {
		case perr == nil && published:
			return loopLockReleaser(lockPath), nil
		case perr != nil && !sharingViolationRetryable(perr):
			return nil, perr // a real, non-contention fs error: never interleave a partial write.
		case perr == nil:
			reaped, rerr := reapStaleLockTTL(lockPath, now, sourcesLockTTL)
			if rerr != nil && !sharingViolationRetryable(rerr) {
				return nil, rerr
			}
			if rerr == nil && reaped {
				continue // cleared an abandoned lease; retry publish immediately.
			}
		}
		if attempt < maxSourcesAcquireAttempts-1 {
			time.Sleep(sourcesAcquireBackoff(attempt))
		}
	}
	return nil, fmt.Errorf("governance ledger is locked by another mora process (%s); retry in a moment", lockPath)
}

// appendGovernanceEntry mints id/created_at/created_by, appends, and persists.
// Returns the stored entry (with its minted id, for a later unforget/undo). The
// whole read-modify-write is serialized across processes (acquireGovernanceLock)
// and RELOADS inside the lease, so two concurrent writers can never clobber each
// other's entry — a dropped forget suppression would silently resurrect content.
func appendGovernanceEntry(cfg Config, e govEntry) (govEntry, error) {
	release, err := acquireGovernanceLock(cfg, time.Now())
	if err != nil {
		return govEntry{}, err
	}
	defer release()
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
// active entry with that id was found. Serialized + reloaded under the same lease
// as appendGovernanceEntry so a concurrent append is never clobbered by a revoke.
func revokeGovernanceEntry(cfg Config, id string) (bool, error) {
	release, err := acquireGovernanceLock(cfg, time.Now())
	if err != nil {
		return false, err
	}
	defer release()
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

// atomPersonID maps a handle/address stable-atom to the PRE-MERGE graph person id it
// denotes (the same personID the entity graph mints). Returns "" for atoms that are
// not a person identity (stable_id / host). This is the one bridge between the
// source-native ledger key and the graph's identity space — and it maps to the
// pre-merge id, never a post-merge canonical (the #52 trap): the canonical moves as
// identities cluster, the source atom does not.
func atomPersonID(a govAtom) string {
	switch a.Kind {
	case atomHandle, atomAddress:
		if a.Value == "" {
			return ""
		}
		return personID(a.Value)
	}
	return ""
}

// mergePairKey is the order-independent key of a source-atom person pair, used to
// dedup decisions and to filter already-decided candidates out of the confirm-queue.
func mergePairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b
}

// briefLineDecisionKey is the stable key for one meeting-brief attribution decision:
// "this cited source memory (stable atom) is / is not linked to this attendee atom".
// Last writer wins by key.
func briefLineDecisionKey(stableAtom, attendeeAtom govAtom) string {
	return stableAtom.Provider + "\x00" +
		stableAtom.Value + "\x00" +
		attendeeAtom.Provider + "\x00" +
		attendeeAtom.Kind + "\x00" +
		attendeeAtom.Value
}

// briefLineDecisions resolves P16 click-to-correct entries from the governance
// ledger: redact entries keyed by (stable_id atom, attendee atom) with decision
// confirm/reject. Returned map uses briefLineDecisionKey and applies last-writer-
// wins semantics.
func (g governance) briefLineDecisions() map[string]string {
	decisions := map[string]string{}
	for _, e := range g.Entries {
		if e.revoked() || e.Kind != govKindRedact || e.Action != govActionRecord || e.Atom2 == nil {
			continue
		}
		if e.Atom.Kind != atomStableID || strings.TrimSpace(e.Atom.Value) == "" {
			continue
		}
		attendee := *e.Atom2
		if attendee.Kind != atomHandle && attendee.Kind != atomAddress {
			continue
		}
		attendee.Value = normalizeIdentity(attendee.Kind, attendee.Value)
		if attendee.Value == "" {
			continue
		}
		if e.Decision != mergeDecisionConfirm && e.Decision != mergeDecisionReject {
			continue
		}
		decisions[briefLineDecisionKey(e.Atom, attendee)] = e.Decision
	}
	return decisions
}

// activeMergeConfirms returns the non-revoked, two-atom merge_confirm entries.
func (g governance) activeMergeConfirms() []govEntry {
	var out []govEntry
	for _, e := range g.Entries {
		if e.revoked() || e.Kind != govKindMergeConfirm || e.Atom2 == nil {
			continue
		}
		out = append(out, e)
	}
	return out
}

// mergeDecisions resolves the ledger's merge_confirm entries into what P13 consumes:
//   - confirmed: same-person pairs (as pre-merge person ids) the graph build unifies;
//   - decided: the set of pairs (confirm OR reject) the confirm-queue must not
//     re-propose. Keyed by mergePairKey over source-atom person ids (never a
//     post-merge canonical id).
//
// Last-writer-wins per pair (entries are chronological), so a reject after a confirm
// (or vice-versa) takes effect. Deterministic: confirmed is sorted.
func (g governance) mergeDecisions() (confirmed []confirmedMerge, decided map[string]bool) {
	decided = map[string]bool{}
	verdict := map[string]string{} // pairKey -> latest decision
	ids := map[string][2]string{}  // pairKey -> (personA, personB)
	govOf := map[string]string{}   // pairKey -> authorizing ledger id
	for _, e := range g.activeMergeConfirms() {
		a, b := atomPersonID(e.Atom), atomPersonID(*e.Atom2)
		if a == "" || b == "" || a == b {
			continue
		}
		key := mergePairKey(a, b)
		verdict[key] = e.Decision
		ids[key] = [2]string{a, b}
		govOf[key] = e.ID
		decided[key] = true
	}
	for key, d := range verdict {
		if d == mergeDecisionConfirm {
			p := ids[key]
			confirmed = append(confirmed, confirmedMerge{A: p[0], B: p[1], GovID: govOf[key]})
		}
	}
	sort.Slice(confirmed, func(i, j int) bool {
		if confirmed[i].A != confirmed[j].A {
			return confirmed[i].A < confirmed[j].A
		}
		return confirmed[i].B < confirmed[j].B
	})
	return confirmed, decided
}
