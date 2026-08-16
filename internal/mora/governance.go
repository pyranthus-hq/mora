package mora

import (
	"github.com/pyranthus-hq/mora/internal/atomicio"
	governancepkg "github.com/pyranthus-hq/mora/internal/governance"
	"os"
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

const governanceSchema = governancepkg.SchemaVersion

// governanceFile is the ledger's on-disk name inside the vault. A dotfile so the
// index rebuild (which parses `*.md`) ignores it; NOT matched by the vault
// `.gitignore` (index.db/tokens/identity*/share/) so it survives `mora sync git`.
const (
	governanceParentStableIDKey = governancepkg.ParentStableIDKey
	governanceParentProviderKey = governancepkg.ParentProviderKey
	governanceParentAtomsKey    = governancepkg.ParentAtomsKey
)

// Atom kinds — the source-native identity the ledger keys on. "host" is reserved
// for a future connector field (no source populates it yet), so it is accepted
// in the schema but derived from no Meta.
const (
	atomStableID = governancepkg.AtomStableID // one whole memory (a chat/thread/event)
	atomHandle   = governancepkg.AtomHandle   // an iMessage participant handle (+1…, or an email)
	atomAddress  = governancepkg.AtomAddress  // a Gmail/Calendar email address
	atomHost     = governancepkg.AtomHost     // reserved; no source field yet
)

// Entry kinds — the governance operations that ride the one ledger. Only the
// suppression kinds (forget/prune/source_scope) gate the write chokepoint today;
// the rest are durable records later phases consume (redact = P16 graph-compile
// participant filter; merge_confirm = P13 confirm-queue; archive reserved).
const (
	govKindForget          = governancepkg.KindForget
	govKindPrune           = governancepkg.KindPrune
	govKindSourceScope     = governancepkg.KindSourceScope
	govKindRedact          = governancepkg.KindRedact
	govKindMergeConfirm    = governancepkg.KindMergeConfirm
	govKindArchive         = governancepkg.KindArchive
	govKindTeachCommitment = governancepkg.KindTeachCommitment
	govKindTeachMemory     = governancepkg.KindTeachMemory
	govKindEvalConsent     = governancepkg.KindEvalConsent
)

// Actions — what an entry DOES at the write chokepoint. "suppress" skips the
// write entirely (never persist); "record" is an inert durable note (corrections)
// with no write-time effect.
const (
	govActionSuppress = governancepkg.ActionSuppress
	govActionRecord   = governancepkg.ActionRecord
)

// merge_confirm decisions — the two verdicts the P13 one-tap confirm-queue records
// about a source-atom pair. "confirm" unifies the two identities in the graph build;
// "reject" pins them apart (never re-proposed, never merged). A later entry on the
// same pair supersedes an earlier one (last-writer-wins).
const (
	mergeDecisionConfirm = governancepkg.DecisionConfirm
	mergeDecisionReject  = governancepkg.DecisionReject
)

// govAtom is a stable-atom key. Provider "" is a cross-provider wildcard (e.g.
// forget an email address across gmail AND calendar); a concrete provider scopes
// the match to that connector (e.g. an iMessage handle).
type govAtom = governancepkg.Atom
type govEntry = governancepkg.Entry

func govEntryRevoked(e govEntry) bool { return e.RevokedAt != "" }

// governance is the whole ledger.
type governance struct {
	Schema  int        `json:"schema"`
	Entries []govEntry `json:"entries"`
}

// activeSuppress returns the live entries that gate the write chokepoint (the
// suppression kinds, action=suppress, not revoked).
func (g governance) activeSuppress() []govEntry {
	return governancepkg.ActiveSuppress(toGovernanceLedger(g))
}

func toGovernanceEntry(e govEntry) governancepkg.Entry   { return e }
func fromGovernanceEntry(e governancepkg.Entry) govEntry { return e }
func toGovernanceLedger(g governance) governancepkg.Ledger {
	return governancepkg.Ledger{Schema: g.Schema, Entries: g.Entries}
}
func fromGovernanceLedger(g governancepkg.Ledger) governance {
	return governance{Schema: g.Schema, Entries: g.Entries}
}
func governanceStore(cfg Config) governancepkg.Store {
	return governancepkg.Store{VaultDir: cfg.VaultDir, NewID: newID, Now: time.Now, CreatedBy: func() string { return "mora " + BuildVersion }, PostLoad: func() {
		if testHookGovAppendPostLoad != nil {
			testHookGovAppendPostLoad()
		}
	}}
}

func governancePath(cfg Config) string { return governanceStore(cfg).Path() }

// loadGovernance reads the ledger. An ABSENT ledger is the common case and reads
// as an empty ledger (no error). A CORRUPT ledger FAILS LOUD: treating it as
// empty would silently resurrect forgotten content — a privacy violation. The
// error is surfaced to the write path, which counts it as a failed write
// (honest-snapshot) rather than laundering a partial/wrong sync into success.
func loadGovernance(cfg Config) (governance, error) {
	g, err := governanceStore(cfg).Load()
	return fromGovernanceLedger(g), err
}

// saveGovernance persists the ledger via atomicWrite (temp+rename). Mode 0600 —
// it names identities the user chose to forget, mildly sensitive; and the vault
// may leave the machine via `mora sync git`.
// governanceLockPath is the ledger's cross-process lease file. Both it and the
// persistent OS guard selected by leaseGuardPath end in `*.lock`, so vault Git
// excludes them from `mora sync git`.
func governanceLockPath(cfg Config) string { return governanceStore(cfg).LockPath() }

// governanceAcquireTimeout is the WALL-CLOCK budget for the governance lease's
// contention spin. It matches sourcesAcquireTimeout's envelope because the hold
// is the same shape — a ledger reload plus one atomicWrite, or the connector
// write chokepoint's check-and-write (governanceWriteLease) — but it is a
// SEPARATE constant on purpose: the ledger lease is the one contended by every
// item of an hourly sync, so its budget must be tunable without moving the
// sources registry's. Exhausting it returns the fail-fast "retry in a moment"
// error a caller is expected to retry (issue #115), never a dropped suppression.
const governanceAcquireTimeout = 2 * time.Second

// acquireGovernanceLock serializes a read-modify-write of the governance ledger
// across processes, exactly as acquireSourcesLock does for sources.json: an
// unlocked load→append→save races (the last save clobbers the other writer's
// entry, and a dropped forget suppression silently resurrects forgotten content).
// It reuses the crash-safe lease primitives (publishLockFile / reapStaleLockTTL /
// loopLockReleaser), the sources lease's TTL + jittered backoff, and its own
// governanceAcquireTimeout wall-clock acquire budget — the same
// single-host, single-user model (a manual `mora forget` racing another forget,
// or the forward-declared scheduled prune #53). It returns a real error, never a
// silent no-op, if the lease cannot be taken; the returned release is idempotent.
func acquireGovernanceLock(cfg Config, now time.Time) (func(), error) {
	return governanceStore(cfg).Acquire(now)
}

// appendGovernanceEntry mints id/created_at/created_by, appends, and persists.
// Returns the stored entry (with its minted id, for a later unforget/undo). The
// whole read-modify-write is serialized across processes (acquireGovernanceLock)
// and RELOADS inside the lease, so two concurrent writers can never clobber each
// other's entry — a dropped forget suppression would silently resurrect content.
func appendGovernanceEntry(cfg Config, e govEntry) (govEntry, error) {
	stored, err := governanceStore(cfg).Append(toGovernanceEntry(e))
	return fromGovernanceEntry(stored), err
}

// appendGovernanceEntryLocked appends to a ledger already loaded while the caller
// holds the governance lease. cmdForget uses it so its removal scan and suppression
// append are one critical section; calling appendGovernanceEntry there would try
// to reacquire the same cross-process lease.
func appendGovernanceEntryLocked(cfg Config, g governance, e govEntry) (govEntry, error) {
	stored, err := governanceStore(cfg).AppendLocked(toGovernanceLedger(g), toGovernanceEntry(e))
	return fromGovernanceEntry(stored), err
}

// revokeGovernanceEntry marks an entry inert (unforget/undo). Returns whether an
// active entry with that id was found. Serialized + reloaded under the same lease
// as appendGovernanceEntry so a concurrent append is never clobbered by a revoke.
func revokeGovernanceEntry(cfg Config, id string) (bool, error) {
	return governanceStore(cfg).Revoke(id)
}

// itemAtom is a memory's whole-item stable-atom key (exact StableID — never with
// the `@account` suffix stripped, which would over-match across accounts).
func itemAtom(provider, stableID string) govAtom {
	return governancepkg.ItemAtom(provider, stableID)
}

// counterpartyAtoms is the memory's set of EXTERNAL identity atoms, coerced from
// Meta with the same helpers the entity graph uses (so the ledger's identity view
// matches the graph's). Sorted + deduped for determinism. iMessage yields handle
// atoms (participants exclude self); Gmail/Calendar yield address atoms across
// from/to/cc/attendees/organizer (these INCLUDE self, which is why identity
// suppression is gated on a sole counterparty — see decideSuppress).
func counterpartyAtoms(provider string, meta map[string]any) []govAtom {
	return governancepkg.CounterpartyAtoms(provider, meta)
}

// normalizeIdentity applies the MINIMAL normalization a stable-atom key needs.
// Addresses and email-shaped handles are lowercased+trimmed; phone handles are
// only trimmed. Deliberately NOT phone/email canonicalization — that lives in
// canonicalizePersons and coupling to it would risk a false merge (precision-first:
// under-match rather than over-match).
func normalizeIdentity(kind, raw string) string { return governancepkg.NormalizeIdentity(kind, raw) }

// providerMatches reports whether an entry's atom provider matches a memory's
// provider. "" on the entry is a cross-provider wildcard.

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
	return governancepkg.DecideSuppress(toGovernanceLedger(g), provider, stableID, meta)
}

// governanceParentContext decodes the source-native parent identity stamped on a
// derived attachment. These keys are intentionally separate from graph Meta so an
// attachment does not double-count the parent's participants, but the governance
// write/removal chokepoints can still cascade a parent forget.

// suppresses is the MappedMemory-typed decision (the connector write path).
func (g governance) suppresses(mm memory.MappedMemory) (bool, string) {
	return governancepkg.Suppresses(toGovernanceLedger(g), mm)
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
func atomPersonID(a govAtom) string { return governancepkg.AtomPersonID(a) }

// mergePairKey is the order-independent key of a source-atom person pair, used to
// dedup decisions and to filter already-decided candidates out of the confirm-queue.
func mergePairKey(a, b string) string { return governancepkg.MergePairKey(a, b) }

// briefLineDecisionKey is the stable key for one meeting-brief attribution decision:
// "this cited source memory (stable atom) is / is not linked to this attendee atom".
// Last writer wins by key.
func briefLineDecisionKey(a, b govAtom) string { return governancepkg.BriefLineDecisionKey(a, b) }

// briefLineDecisions resolves P16 click-to-correct entries from the governance
// ledger: redact entries keyed by (stable_id atom, attendee atom) with decision
// confirm/reject. Returned map uses briefLineDecisionKey and applies last-writer-
// wins semantics.
func (g governance) briefLineDecisions() map[string]string {
	return governancepkg.BriefLineDecisions(toGovernanceLedger(g))
}

// activeMergeConfirms returns the non-revoked, two-atom merge_confirm entries.

// mergeDecisions resolves the ledger's merge_confirm entries into what P13 consumes:
//   - confirmed: same-person pairs (as pre-merge person ids) the graph build unifies;
//   - decided: the set of pairs (confirm OR reject) the confirm-queue must not
//     re-propose. Keyed by mergePairKey over source-atom person ids (never a
//     post-merge canonical id).
//
// Last-writer-wins per pair (entries are chronological), so a reject after a confirm
// (or vice-versa) takes effect. Deterministic: confirmed is sorted.
func (g governance) mergeDecisions() (confirmed []confirmedMerge, decided map[string]bool) {
	items, decided := governancepkg.MergeDecisions(toGovernanceLedger(g))
	for _, m := range items {
		confirmed = append(confirmed, confirmedMerge{A: m.A, B: m.B, GovID: m.GovID})
	}
	return confirmed, decided
}

// governanceWriteLease is the shared primitive behind EVERY no-resurrection write
// path: it acquires the governance lease (acquireGovernanceLock — the same lease
// `mora forget` takes to append its suppression) and loads the ledger, returning
// both the ledger and a release func the caller MUST defer. Holding the lease
// across the suppression CHECK and the subsequent atomicWrite is what makes those
// two steps atomic w.r.t. a concurrent forget's append: a forget can commit its
// suppression EITHER entirely before the check (⇒ the write is skipped) or
// entirely after the write releases the lease (⇒ its removal pass reaps the
// just-written file) — never in between. A once-per-caller SNAPSHOT (release then
// write) reopened that between window and silently resurrected the atom (#113).
// On a load error the lease is released before returning so the caller need not.
func governanceWriteLease(cfg Config) (governance, func(), error) {
	release, err := acquireGovernanceLock(cfg, time.Now())
	if err != nil {
		return governance{}, nil, err
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		release()
		return governance{}, nil, err
	}
	return g, release, nil
}

// writeUnlessForgotten atomically writes body to dest UNLESS the governance
// ledger suppresses the (provider, id) atom — the resurrection guard for a write
// path that renders directly (ingestFilesystem). The whole load→decide→write runs
// under a single governance lease (governanceWriteLease); see that helper for why
// the lease must SPAN the check and the write. A once-per-walk ledger SNAPSHOT
// could not give this — a forget committing after the snapshot but before the
// write silently resurrected the atom (#113) — and releasing the lease before the
// atomicWrite would reopen the same window. A write that lands just AHEAD of the
// commit is a normal re-index that forget's lease-held removal scan observes and
// reaps. Reports whether it wrote (false ⇒ suppressed). Reloading + relocking per
// file costs an O_EXCL create/remove per write; that is the deliberate price of
// the no-resurrection invariant on a local, single-user tool.
func writeUnlessForgotten(cfg Config, provider, id, dest string, body []byte, mode os.FileMode) (bool, error) {
	g, release, err := governanceWriteLease(cfg)
	if err != nil {
		return false, err
	}
	defer release()
	if sup, _ := g.decideSuppress(provider, id, nil); sup {
		return false, nil
	}
	if testHookInWriteCritical != nil {
		testHookInWriteCritical() // test seam: assert the lease is held ACROSS the write (#113).
	}
	if err := atomicio.Write(dest, body, mode); err != nil {
		return false, err
	}
	return true, nil
}

// testHookInWriteCritical, when non-nil (tests only), fires inside
// writeUnlessForgotten AFTER the fresh suppression check and BEFORE atomicWrite —
// i.e. while the governance lease is held. It is the seam that proves the lease
// SPANS the write (a mere per-file reload without the lease would leave this
// window unlocked and #113 open). Nil in production.
var testHookInWriteCritical func()

// testHookGovAppendPostLoad, when non-nil (tests only), fires inside
// appendGovernanceEntry AFTER loadGovernance and BEFORE the append+save — the
// read-modify-write window. Tests use it to widen that window and prove the lease
// serializes concurrent appends (without it, an unlocked RMW drops updates). Nil
// in production.
var testHookGovAppendPostLoad func()
