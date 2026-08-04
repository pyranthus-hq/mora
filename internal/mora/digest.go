package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// The daily digest (the headline use case). As of Phase 12 it is DELTA-aware: each
// run records a per-instance watermark (a content-hash set), and the next run
// surfaces only memories that are new-or-changed since it — "what changed since
// you last looked" — not a fixed-window re-dump. An explicit --since-hours window
// (opts.SinceHours>0) still renders the plain window for ad-hoc use and never
// touches the watermark. It is a deterministic, model-free floor — no LLM call —
// so it is cheap, reproducible, and safe to run on a schedule. An agent
// (Codex/Claude) can layer synthesis on top of the cited items.
const (
	digestSnippetLen   = 200
	digestDefaultHours = 24
	digestDefaultCap   = 8
	// commitmentDailyWindow is the DAILY obligation contract: the trailing seven
	// 24-hour periods ending at the surface clock, inclusive at both endpoints.
	commitmentDailyWindow = 7 * 24 * time.Hour
	// digestColdStartDays is the courtesy display window on an instance's FIRST
	// run (no watermark yet): we baseline ALL current hashes (so archived backfill
	// becomes the starting line, not a flood) but DISPLAY only the last 7 days
	// (calendar = the upcoming 7 days, its natural framing). Run 2 onward is a true
	// delta. (D-04)
	digestColdStartDays = 7
	// digestStaleHours is the staleness threshold (reused from `sync status`): a
	// source whose last clean sync is older than this reads "stale". (D-03)
	digestStaleHours = 48
)

// Three-state per-instance labels (D-03). Exactly one is reported per enumerated
// connector: a delta (new/updated items, no sentinel), or one of these sentinels.
const (
	stateDelta       = "delta"
	stateNoChanges   = "no changes since last brief"
	stateStale       = "stale"
	stateUnavailable = "unavailable"
	stateColdStart   = "baseline" // cold start: first run, 7d courtesy window displayed
)

// DigestItem is one cited artifact — title + snippet + the memory id so the agent
// (or the user) can pull the full memory with read_memory. Identified obligations
// stay nested beneath that artifact so ranking, caps, budgets, and watermarks remain
// artifact-grain. Change is the typed Delta seam (M-5): "new" | "updated" in DELTA
// mode, "" in the plain-window path. renderDigest AND the MCP projection (Plan 05)
// both read this one struct.
type DigestItem struct {
	ID          string             `json:"id"`
	Title       string             `json:"title"`
	Source      string             `json:"source"`
	CreatedAt   string             `json:"created_at"`
	Snippet     string             `json:"snippet"`
	Change      string             `json:"change,omitempty"` // new | updated (M-5)
	Obligations []DigestObligation `json:"obligations,omitempty"`
	// The scalar lane is retained for legacy, ID-less commitment generations.
	// Identified generations use Obligations so one artifact can expose every
	// independently materialized commitment without guessing which one represents it.
	Owner     govAtom   `json:"owner,omitzero"`
	Direction Direction `json:"direction,omitempty"`
	// CounterpartyLabel is name-grain display attribution only. It does not imply
	// a provider identity or participate in entity merging.
	CounterpartyLabel string `json:"counterparty_label,omitempty"`
	DueAt             string `json:"due_at,omitempty"`
	Lifecycle         string `json:"lifecycle,omitempty"`
	ClosureRef        string `json:"closure_ref,omitempty"`
	// LowSignal flags a SERVICE-ONLY item — every participant is an automated/service
	// identity (receipts, newsletters, no-reply notices), per memoryIsServiceOnly. It
	// drives the noise-collapse in section assembly so a window of pure-service items
	// doesn't crowd out signal. It is internal (json:"-") so it never changes the MCP
	// payload shape or budget.
	LowSignal bool `json:"-"`
}

// DigestObligation is one identified, evidence-cited commitment nested beneath a
// DigestItem. The row copies only materialized product state. It never derives an ID
// or citation from the artifact title/snippet.
type DigestObligation struct {
	CommitmentID      string               `json:"commitment_id"`
	Summary           string               `json:"summary"`
	Owner             govAtom              `json:"owner"`
	Direction         Direction            `json:"direction"`
	CounterpartyLabel string               `json:"counterparty_label,omitempty"`
	DueAt             string               `json:"due_at"`
	Lifecycle         string               `json:"lifecycle"`
	ClosureRef        string               `json:"closure_ref"`
	Citations         []CommitmentCitation `json:"citations"`
}

// DigestSection groups one instance's surfaced items, recency-ordered. State is
// the three-state label (D-03); MoreCount/Truncated carry the silent-data-loss
// guard's "+N more since last brief" when the per-instance delta exceeds the cap.
// These are the typed Delta seam (M-5) read by renderDigest now and the MCP
// projection in Plan 05 — one source of truth, not a render string plus a map.
type DigestSection struct {
	Source         string       `json:"source"`
	State          string       `json:"state"`
	Items          []DigestItem `json:"items"`
	MoreCount      int          `json:"more_count,omitempty"`
	Truncated      bool         `json:"truncated,omitempty"`
	ElidedByBudget int          `json:"elided_by_budget,omitempty"`
}

// digestEmptyEvidence records bounded, deterministic facts from the exact vault
// input pass used to build a digest. VaultRows counts parsed, visible,
// non-tombstoned rows. FilteredRows counts those rows that match every active
// entity/scope/since-days filter plus the source-instance/family filter, before a
// window or delta classifier can suppress them.
//
// This is current-build evidence only: it is unexported, omitted from JSON, not
// persisted, and never participates in ContentHash or the brief watermark. It
// therefore does not change the hash schema governed by briefHashSchemaVersion.
type digestEmptyEvidence struct {
	vaultRows    int
	filteredRows int
}

// Digest is the assembled brief. Generated is injected by the caller (time.Now)
// for deterministic tests; it (and the cold-start cutoff) is canonicalized to UTC
// for byte-stability without disturbing the DST-safe window math.
type Digest struct {
	Generated  string `json:"generated"`
	SinceHours int    `json:"since_hours"`
	// Urgent is the item-level shelf (issue #62 defect 2): deadline-bearing items from
	// known humans, lifted ABOVE the sections and budget-protected so they always
	// render. UrgentMore counts any that overflow the shelf cap (they re-surface next
	// run rather than being marked seen).
	Urgent           []DigestItem      `json:"urgent,omitempty"`
	UrgentMore       int               `json:"urgent_more,omitempty"`
	Sections         []DigestSection   `json:"sections"`
	Freshness        map[string]string `json:"freshness,omitempty"`
	StaleTasks       []string          `json:"stale_tasks,omitempty"`
	EmptyExplanation string            `json:"empty_explanation,omitempty"`
	// SourceHealth is the per-connector freshness snapshot (HEALTH-01/-02),
	// computed ONCE at build time (sourceHealthAll) and carried on the struct so
	// every render/projection path (Markdown banner, MCP digest/brief payload)
	// reads the SAME snapshot instead of re-deriving it against a possibly-later
	// clock. The red banner (healthBannerFromSources) is a pure function of this.
	SourceHealth []sourceHealth `json:"source_health,omitempty"`
	// idxHealth is the index arm snapshot (Gate 2), captured at build time next to
	// SourceHealth so the aggregate banner is a pure function of both. UNEXPORTED so
	// it never enters the MCP digest payload (that compact envelope is Packet C) and
	// the byte-determinism envelope tests stay untouched.
	idxHealth indexHealth
	// producerHealth is the producer-liveness arm snapshot (Gate 2 / HEALTH-11),
	// pinned at build time so the aggregate banner surfaces a dead automation on the
	// flagship brief surface too — a healthy source and clean index must not let the
	// brief render green while nothing has produced it. UNEXPORTED, like idxHealth.
	producerHealth []producerHealth
	// emptyEvidence preserves pre-window/pre-delta filter-match facts that cannot
	// be reconstructed from an empty set of surfaced items.
	emptyEvidence digestEmptyEvidence
}

// briefOpts is the buildDigest options seam. advance gates the watermark commit
// (default-off on every surface — D-02; only the scheduled --advance caller sets
// it, wired in Plan 05). sinceHours>0 selects the plain-window path (SC#2) which
// never advances. perSourceCap caps each section (0 => default).
type briefOpts struct {
	advance bool
	// forceRegen bypasses the persisted dated-brief cache in resolveBrief so the
	// brief is regenerated from the live vault on demand (the `mora brief --fresh`
	// path). Read-only like the rest of the read side — it never advances the
	// watermark; it only skips the verbatim-file shortcut.
	forceRegen   bool
	sinceHours   int
	perSourceCap int
	// source filters the digest to one connector instance or provider family
	// (digestSourceMatches). Empty = all sources. Preview-only ergonomics: the
	// scheduled --advance job never sets it (a filtered advance would mark
	// unseen sources' items as read).
	source string
	// entityIDSet, scope, sinceDays are the three preview-only filters applied
	// per-memory inside filterByInstance. entityIDSet is the resolved alias-id SET
	// for one person (canonical id ∪ every address/handle alias id — P1-A); empty =
	// no entity filter. scope matches Memory.Scope exactly; empty = all. sinceDays
	// is an additional created-at lower bound in days; <=0 = none (a negative is
	// inert, never a future cutoff — P1-D). None of these may combine with advance.
	entityIDSet map[string]bool
	scope       string
	sinceDays   int
}

// filtered reports whether any preview-only filter is active. The persisted
// (unfiltered) brief cache is used ONLY when this is false, and a filtered digest
// can never --advance the watermark. Note: sinceHours is NOT included — it is a
// pulse-only window, never a brief input (P1-E); and a negative sinceDays does not
// count (it is clamped/inert — P1-D).
func (o briefOpts) filtered() bool {
	return o.source != "" || len(o.entityIDSet) > 0 || o.scope != "" || o.sinceDays > 0
}

// sourceDigestRank / digestSourceLabel are now thin shims over connectorDisplay
// (M-6) — the catalog descriptor in connectors.go OWNS the rank/label data, so an
// Nth connector gets a real rank/label instead of default-rank-3 / a title-cased
// raw provider. Kept as named helpers so the sort/label call sites read clearly.
func sourceDigestRank(src string) int     { r, _ := connectorDisplay(src); return r }
func digestSourceLabel(src string) string { _, l := connectorDisplay(src); return l }

// buildDigest assembles the brief.
//
//   - PLAIN-WINDOW mode (opts.sinceHours>0, SC#2): every non-deleted memory created
//     within the last sinceHours, grouped by sourceInstanceKey, recency-ordered,
//     capped — the legacy behavior, ad-hoc. NEVER advances the watermark.
//   - DELTA mode (opts.sinceHours==0, the scheduled default, SC#1): per instance,
//     surface only memories new-or-changed since its watermark (content-hash diff
//     via classify). On an instance's first run apply a 7-day courtesy display
//     window while baselining ALL hashes (D-04). Reports a three-state label per
//     ENABLED+INGESTING connector (D-03). When opts.advance is set, commits the
//     watermark under a lock, advancing ONLY over items actually shown (the
//     silent-data-loss guard) and dropping deleted ids (M-4). Preview writes
//     nothing (SC#4).
//
// now is injected for deterministic windowing.
func buildDigest(cfg Config, now time.Time, opts briefOpts) (Digest, error) {
	perSourceCap, byInstance, memSal, emptyEvidence, err := digestInputs(cfg, now, opts)
	if err != nil {
		return Digest{}, err
	}
	commitmentsByMemory, gated, err := digestCommitmentInventory(cfg, now)
	if err != nil {
		return Digest{}, err
	}
	if gated {
		commitmentsByMemory = dailyCommitmentInventory(commitmentsByMemory, now)
		byInstance = filterDigestInstancesByCommitments(byInstance, commitmentsByMemory)
	}
	var d Digest
	if opts.sinceHours > 0 {
		d, err = buildWindowDigest(cfg, now, opts.sinceHours, perSourceCap, byInstance, memSal, opts.source)
	} else {
		// buildDigest NEVER commits the watermark — it is the pure build (used by every
		// preview/read surface). The scheduled --advance transaction lives in advanceBrief,
		// which reruns the delta build to capture the per-instance commit plans and
		// commits ONLY over what survives the Markdown budget (issue #62 defect 1).
		d, _, err = buildDeltaDigest(cfg, now, opts, perSourceCap, byInstance, memSal)
	}
	if err != nil {
		return Digest{}, err
	}
	attachDigestCommitments(&d, commitmentsByMemory)
	d.emptyEvidence = emptyEvidence
	d.EmptyExplanation = deriveEmptyExplanation(d, opts, false)
	return d, nil
}

// digestCommitmentInventory loads the generation-stamped, whole-vault
// materialization shared with meeting briefs. A missing DataDir is the explicit seam
// used by pure digest assembly tests; a real loaded Config always gates on the typed
// inventory.
func digestCommitmentInventory(cfg Config, at time.Time) (map[string][]Commitment, bool, error) {
	// Pure resolver tests may intentionally construct only VaultDir/StateDir and
	// have no index location. A real loaded Config always has DataDir; do not turn
	// an otherwise valid empty/filtered brief into an attempt to create "index.db"
	// in the process working directory.
	if strings.TrimSpace(cfg.DataDir) == "" {
		return nil, false, nil
	}
	inventory, err := readCommitmentInventory(context.Background(), cfg, at)
	if err != nil {
		return nil, false, err
	}
	return inventory, true, nil
}

func commitmentSurfaceEligible(commitment Commitment) bool {
	return commitment.State == commitOpen && commitment.DuplicateOf == ""
}

func commitmentDailyEligible(commitment Commitment, at time.Time) bool {
	if !commitmentSurfaceEligible(commitment) {
		return false
	}
	openedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(commitment.OpenedBy.OccurredAt))
	if err != nil {
		return false
	}
	at = at.UTC()
	cutoff := at.Add(-commitmentDailyWindow)
	return !openedAt.Before(cutoff) && !openedAt.After(at)
}

func dailyCommitmentInventory(inventory map[string][]Commitment, at time.Time) map[string][]Commitment {
	out := make(map[string][]Commitment, len(inventory))
	for memoryID, commitments := range inventory {
		for _, commitment := range commitments {
			if commitmentDailyEligible(commitment, at) {
				out[memoryID] = append(out[memoryID], commitment)
			}
		}
	}
	return out
}

// filterDigestInstancesByCommitments applies the typed inventory as the eligibility
// gate before urgency, recurrence collapse, ranking, and caps. Filtering after
// assembly would let non-obligation artifacts consume a scarce slot and suppress a
// real commitment.
func filterDigestInstancesByCommitments(byInstance map[string][]Memory, inventory map[string][]Commitment) map[string][]Memory {
	out := make(map[string][]Memory, len(byInstance))
	for key, memories := range byInstance {
		for _, m := range memories {
			eligible := false
			for _, commitment := range inventory[m.ID] {
				if commitmentSurfaceEligible(commitment) {
					eligible = true
					break
				}
			}
			if eligible {
				out[key] = append(out[key], m)
			}
		}
	}
	return out
}

// digestCommitmentFor chooses the typed lane represented by an artifact-grain item.
// Prefer the independently anchored commitment whose evidence is actually visible
// in the title/snippet projection; if clipping hides every opening, use the stable
// materialization order. This removes the old exactly-one-per-artifact hole without
// inventing a content-derived commitment or duplicating an artifact citation.
func digestCommitmentFor(item DigestItem, candidates []Commitment) (Commitment, bool) {
	var eligible []Commitment
	for _, commitment := range candidates {
		if commitmentSurfaceEligible(commitment) {
			eligible = append(eligible, commitment)
		}
	}
	if len(eligible) == 0 {
		return Commitment{}, false
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		return commitmentEvidenceLess(eligible[i], eligible[j])
	})
	visible := strings.ToLower(oneLine(item.Title + " " + item.Snippet))
	best := -1
	bestPos := -1
	for i, commitment := range eligible {
		quote := strings.ToLower(oneLine(commitment.OpenedBy.Quote))
		if quote == "" {
			quote = strings.ToLower(oneLine(commitment.Summary))
		}
		if pos := strings.LastIndex(visible, quote); pos > bestPos {
			best, bestPos = i, pos
		}
	}
	if best >= 0 {
		return eligible[best], true
	}
	return eligible[0], true
}

func identifiedDigestObligations(candidates []Commitment) (out []DigestObligation, identified bool) {
	var eligible []Commitment
	for _, commitment := range candidates {
		if !commitmentSurfaceEligible(commitment) || commitment.ID == "" {
			continue
		}
		identified = true
		eligible = append(eligible, commitment)
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		return commitmentEvidenceLess(eligible[i], eligible[j])
	})
	for _, commitment := range eligible {
		var citations []CommitmentCitation
		hasOpener := false
		for _, citation := range commitment.Citations {
			if citation.CommitmentID != commitment.ID || citation.Citation.MemoryID() == "" {
				continue
			}
			switch citation.Role {
			case commitCitationOpener:
				if citation.Citation.MemoryID() != commitment.OpenedBy.MemoryID {
					continue
				}
				hasOpener = true
			case commitCitationClosure, commitCitationSupporting:
			default:
				continue
			}
			citations = append(citations, citation)
		}
		// An identified row without its own opening evidence would be an uncited
		// claim. Drop it instead of borrowing the parent artifact citation.
		if !hasOpener {
			continue
		}
		summary := oneLine(commitment.Summary)
		if summary == "" {
			summary = oneLine(commitment.OpenedBy.Quote)
		}
		if summary == "" {
			continue
		}
		out = append(out, DigestObligation{
			CommitmentID:      commitment.ID,
			Summary:           summary,
			Owner:             commitment.Owner,
			Direction:         commitment.Direction,
			CounterpartyLabel: commitment.CounterpartyLabel,
			DueAt:             commitDueValue(commitment.Due),
			Lifecycle:         commitment.State,
			ClosureRef:        commitment.ClosureRef,
			Citations:         citations,
		})
	}
	return out, identified
}

// attachDigestCommitments copies typed obligation rows from the same snapshot that
// gated assembly. Identified generations emit every eligible commitment as a nested
// subrow. Legacy generations keep their existing scalar, single-selection lane.
func attachDigestCommitments(d *Digest, byMemory map[string][]Commitment) {
	attach := func(item *DigestItem) {
		obligations, identified := identifiedDigestObligations(byMemory[item.ID])
		if identified {
			item.Obligations = obligations
			return
		}
		commitment, ok := digestCommitmentFor(*item, byMemory[item.ID])
		if !ok {
			return
		}
		item.Owner = commitment.Owner
		item.Direction = commitment.Direction
		item.CounterpartyLabel = commitment.CounterpartyLabel
		item.DueAt = commitDueValue(commitment.Due)
		item.Lifecycle = commitment.State
		item.ClosureRef = commitment.ClosureRef
	}
	for i := range d.Urgent {
		attach(&d.Urgent[i])
	}
	for i := range d.Sections {
		for j := range d.Sections[i].Items {
			attach(&d.Sections[i].Items[j])
		}
	}
}

// digestInputs factors the shared preprocessing behind both the window and delta
// paths: parse every non-tombstoned memory, group by INSTANCE key (M-1, never the
// per-item ProviderID), compute the whole-vault person-salience map ONCE (SC#3), then
// apply the preview-only entity/scope/since-days filters. It also returns the
// pre-classification evidence needed to distinguish a real filter miss from a
// matching row suppressed by a window or watermark. It opens no index DB.
func digestInputs(cfg Config, now time.Time, opts briefOpts) (perSourceCap int, byInstance map[string][]Memory, memSal map[string]int64, emptyEvidence digestEmptyEvidence, err error) {
	perSourceCap = opts.perSourceCap
	if perSourceCap <= 0 {
		perSourceCap = digestDefaultCap
	}
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return 0, nil, nil, digestEmptyEvidence{}, err
	}
	// Skip tombstones up front (M-4) so a cancelled calendar event (new content_hash)
	// is never live [updated] nor in the cold-start 7d window.
	byInstance = map[string][]Memory{}
	governance, err := loadGovernance(cfg)
	if err != nil {
		return 0, nil, nil, digestEmptyEvidence{}, err
	}
	for _, path := range files {
		m, perr := parseMemory(path)
		if perr != nil {
			continue
		}
		if m.DeletedAt != "" {
			continue // M-4: tombstone — mirror graph.go's skip.
		}
		if !governance.memoryVisible(m.ID) {
			continue
		}
		m = decorateDecision(m, now)
		emptyEvidence.vaultRows++
		if m.DecisionStatus == decisionNeedsReview {
			m.Title = "[NEEDS REVIEW] " + m.Title
			m.Text = "Decision validity is incomplete or expired. Review before relying on it.\n\n" + m.Text
		}
		key, groupable := sourceInstanceKey(m)
		if !groupable && opts.sinceHours > 0 && (m.Source == "manual" || m.Source == "mcp") {
			// Explicit-window DAILY is not a watermark run. Include locally
			// authored memories in a deterministic manual bucket so an open
			// commitment in a note can satisfy the obligations-v2 uniform DAILY
			// rule. Delta mode still rejects empty-Provider memories: they have no
			// connector instance and must never mint a watermark key (M-1).
			key = "manual"
			groupable = true
		}
		countsAsFilterMatch := memoryMatchesPreviewFilters(m, opts, now)
		if opts.source != "" {
			countsAsFilterMatch = countsAsFilterMatch && groupable && digestSourceMatches(key, opts.source)
		}
		if countsAsFilterMatch {
			emptyEvidence.filteredRows++
		}
		if !groupable {
			continue
		}
		byInstance[key] = append(byInstance[key], m)
	}
	// Whole-vault salience so a person's volume spans the whole vault (matching
	// buildGraph), computed BEFORE the preview-only narrowing (P1-C).
	memSal = digestMemorySalience(flattenInstances(byInstance))
	byInstance = filterByInstance(byInstance, opts, now)
	return perSourceCap, byInstance, memSal, emptyEvidence, nil
}

// digestSourceMatches reports whether an instance key passes the digest's
// source filter. Empty filter passes everything. A filter matches its exact
// instance key AND its provider family — "gmail" selects both "gmail" and
// "gmail:work" (so a family rundown spans accounts), while "gmail:work"
// selects only that mailbox. This is what lets an agent ask for "just my
// iMessages this week" without calendar sections (which RANK earlier) eating
// the entire byte budget.
func digestSourceMatches(key, source string) bool {
	if source == "" {
		return true
	}
	// Normalize the user/agent-supplied filter through the same provider→type
	// alias as the keying seam: "applecal" is what the user SEES on disk
	// (frontmatter provider, sources/applecal/ directory) and silently
	// matching nothing would return an empty digest with no error.
	source = providerToType(source)
	return key == source || strings.HasPrefix(key, source+":")
}

// flattenInstances collects every memory across all instances into one slice, in
// deterministic (sorted-key) order so the salience aggregation has no
// map-iteration-order dependence. (The kernel's per-person sums/max are
// order-independent regardless, but we keep the input order stable on principle.)
func flattenInstances(byInstance map[string][]Memory) []Memory {
	var all []Memory
	for _, key := range sortedInstanceKeys(byInstance) {
		all = append(all, byInstance[key]...)
	}
	return all
}

// digestMemorySalience maps each non-tombstoned memory's ID to its salience: the
// MAX salience of its participant people, computed via the SAME 14-01 kernel
// (aggregatePersonSalience) the entity graph consumes — one source of truth, so the
// digest and `mora graph` rank on identical math. A memory with no participants (or
// only service participants, which score 0) maps to 0. Max-fold (not sum) mirrors
// the graph's canon remap: a thread's salience is its most-salient human, never a
// sum that would reward many low-value participants. Deterministic: the kernel is
// pure and max is order-independent.
func digestMemorySalience(mems []Memory) map[string]int64 {
	personSal := aggregatePersonSalience(mems)
	out := make(map[string]int64, len(mems))
	for _, m := range mems {
		if m.DeletedAt != "" {
			continue // tombstone — mirror aggregatePersonSalience's skip.
		}
		parts, _, _, _ := personRefs(m)
		var best int64
		for _, p := range parts {
			if s := personSal[p.id]; s > best {
				best = s
			}
		}
		out[m.ID] = best
	}
	return out
}

// memoryIsServiceOnly reports whether a memory's SENDER(s) are all service /
// automated identities (no-reply senders, receipts, newsletters, job/parking alerts)
// — the precise "collapse-eligible noise" signal for the digest. It gates on SENDERS
// (the email `from` / calendar organizer), NOT all participants, on purpose: the
// user's own address is a recipient (`to`) on every received email and classifies as
// a person, so an all-participants check would never fire (self always looks human).
// The sender is the OTHER party, so a no-reply sender is detected even though the user
// is on the thread. It returns false for a memory with no senders (a note, an
// unparticipated item) so non-connector content is never collapsed, and false the
// moment one HUMAN sender appears (an order receipt your dad forwarded stays — a human
// sent it). Reuses the SAME classifyIdentity floor the salience kernel uses.
func memoryIsServiceOnly(m Memory) bool {
	_, senders, _, _ := personRefs(m)
	if len(senders) == 0 {
		return false
	}
	for _, id := range senders {
		if classifyIdentity(strings.TrimPrefix(id, "person:"), "") == "person" {
			return false
		}
	}
	return true
}

// isLowSignalItem reports whether a surfaced item should be collapsed into the
// "+N more" tail rather than shown as a full line — the noise-collapse signal
// (issue #62 defect 3, extending the service-only collapse):
//
//   - memoryIsServiceOnly — every sender is a service identity. This ALREADY catches a
//     subscription/feed calendar whose organizer is a bulk-send address (noreply@,
//     sports/holiday feeds), so those flood-sources collapse with no new rule.
//   - a PAST calendar event resurfacing ONLY as [updated] — a re-sync bumped a
//     months-old event's hash; the meeting already happened, so it is stale noise (a
//     FUTURE [updated] event is a genuine reschedule and stays full-signal).
//
// A "no organizer at all → low-signal" heuristic was deliberately NOT added: a real
// personal Apple Calendar event can legitimately carry no organizer, so collapsing on
// its absence would hide genuine events. Preventing calendar noise from STARVING the
// Emails section is instead handled structurally by the fair per-source budget floor
// (budgetSourceFloor), which needs no fragile subscription classifier.
func isLowSignalItem(m Memory, change string, now time.Time) bool {
	if memoryIsServiceOnly(m) {
		return true
	}
	if m.Type == "event" && change == "updated" && itemOccurredAt(m).Before(now) {
		return true // stale past event bumped by a re-sync.
	}
	return false
}

// buildWindowDigest is the plain ad-hoc window path (SC#2): created_at within the
// last sinceHours, no delta, no watermark, no State sentinels. It mirrors the
// legacy behavior but groups by instance key so the human labels fire.
func buildWindowDigest(cfg Config, now time.Time, sinceHours, perSourceCap int, byInstance map[string][]Memory, memSal map[string]int64, sourceFilter string) (Digest, error) {
	window := time.Duration(sinceHours) * time.Hour
	cutoff := now.Add(-window)
	forward := now.Add(window)
	var sections []DigestSection
	var urgentAll []urgentEntry
	for _, key := range sortedInstanceKeys(byInstance) {
		if !digestSourceMatches(key, sourceFilter) {
			continue
		}
		upcoming := connectorUpcoming(key)
		var tis []tsItem
		for _, m := range byInstance[key] {
			ts, err := time.Parse(time.RFC3339, m.CreatedAt)
			if err != nil {
				continue
			}
			// Forward-bounded window (Bug: an N-hour brief used to surface
			// months-out calendar events). Upcoming sources look FORWARD into the
			// next window [now, now+N]; past-oriented sources look BACK [now-N, now]
			// and reject future-dated outliers (clock-skew / scheduled-send).
			if upcoming {
				// Grace window (P1-F): an event that started within meetingPrepGrace is
				// still "current" — the meeting you just walked into — so it isn't dropped
				// from the calendar section.
				if ts.Before(now.Add(-meetingPrepGrace)) || ts.After(forward) {
					continue
				}
			} else if ts.Before(cutoff) || ts.After(now) {
				continue
			}
			// Urgent items lead the brief on the cross-source shelf here too (issue #62),
			// so the empty-delta window fallback never buries a recent deadline email.
			if ok, phrase := isUrgent(m, now); ok {
				urgentAll = append(urgentAll, urgentEntry{
					item:       urgentItemFor(cfg, m, key, "", phrase),
					occurredAt: itemOccurredAt(m),
					sal:        memSal[m.ID],
					score:      urgencyScore(m, phrase),
				})
				continue
			}
			it := digestItemFor(cfg, m, key, "")
			it.LowSignal = isLowSignalItem(m, "", now)
			tis = append(tis, tsItem{item: it, ts: ts, sal: memSal[m.ID], series: recurringSeriesID(m)})
		}
		if len(tis) == 0 {
			continue
		}
		tis = collapseRecurringSeries(tis, now)
		items, more := capRecency(tis, perSourceCap, upcoming)
		items, collapsed := collapseLowSignal(items)
		if items == nil {
			items = []DigestItem{}
		}
		sections = append(sections, DigestSection{Source: key, Items: items, MoreCount: more + collapsed, Truncated: more > 0})
	}
	sortSections(sections)
	shelf, shelfMore := assembleUrgentShelf(urgentAll)
	stale, _ := staleTasks(cfg, 3)
	hSnap := healthOf(cfg, now)
	return Digest{
		Generated:      now.UTC().Format(time.RFC3339),
		SinceHours:     sinceHours,
		Urgent:         shelf,
		UrgentMore:     shelfMore,
		Sections:       sections,
		Freshness:      sourceFreshness(cfg),
		StaleTasks:     stale,
		SourceHealth:   hSnap.Sources,
		idxHealth:      hSnap.Index,
		producerHealth: hSnap.Producers,
	}, nil
}

// briefCommitPlan carries everything needed to commit ONE instance's watermark AFTER
// the Markdown budget has decided what actually rendered (issue #62 defect 1). It is
// produced by the PURE delta build and consumed by advanceBrief once the budgeted
// survivor set is known — the build itself never writes the watermark.
type briefCommitPlan struct {
	key   string
	snap  briefSnapshot
	delta briefDelta
	// lineMembers maps each displayed line's DigestItem.ID to the stable memory IDs it
	// represents (a collapsed recurring series line stands for all its instances). The
	// watermark advances over a line's members iff that line survived the budget.
	lineMembers map[string][]string
	// countOnlyIDs are stable IDs acknowledged WITHOUT a rendered line (the low-signal
	// receipt/newsletter tail folded into "+N more"). They commit iff the section
	// rendered ≥1 line — preserving the pre-#62 count-only acknowledgement.
	countOnlyIDs []string
}

// buildDeltaDigest is the delta + three-state path (SC#1, SC#3, SC#4). It is the
// behavioral heart of Phase 12. It is PURE: it builds the Digest and returns a
// per-instance commit plan for each enumerated connector, but NEVER locks, persists,
// or advances the watermark (that is advanceBrief's job — issue #62 defect 1).
func buildDeltaDigest(cfg Config, now time.Time, opts briefOpts, perSourceCap int, byInstance map[string][]Memory, memSal map[string]int64) (Digest, []briefCommitPlan, error) {
	// The enumeration set for the three-state labels is the ENABLED+INGESTING
	// connectors (M-2) — not providers-found-in-memories (which would hide a
	// broken/all-deleted source) and not the sync/ dir. A connector enumerated
	// here but absent from byInstance still emits a section with its State, so a
	// zero-memory/all-deleted source surfaces "unavailable" rather than vanishing
	// (the SC#3 gap).
	enumerated, err := ingestingConnectors(cfg)
	if err != nil {
		return Digest{}, nil, err
	}

	// The set of instance keys to BUILD sections for IS exactly the enumerated
	// (enabled+ingesting) connectors (M-2). `enumerated` is already sorted; copy so
	// the iteration order is deterministic.
	keys := append([]string(nil), enumerated...)
	sort.Strings(keys)
	if opts.source != "" {
		// A filtered advance is rejected up front by advanceBrief; here we only narrow
		// the section keys to the (preview-only) source filter.
		filtered := keys[:0]
		for _, k := range keys {
			if digestSourceMatches(k, opts.source) {
				filtered = append(filtered, k)
			}
		}
		keys = filtered
	}

	var sections []DigestSection
	var plans []briefCommitPlan
	var urgentAll []urgentEntry
	for _, key := range keys {
		mems := byInstance[key] // may be nil for a zero-memory enumerated connector
		snap := loadBriefSnapshot(cfg, key)
		delta := classify(snap, mems, now)

		items, urgent, lineMembers, countOnly, moreCount := deltaSectionItems(cfg, delta, mems, now, key, perSourceCap, memSal)
		urgentAll = append(urgentAll, urgent...)

		// hasDelta counts the shelf too: an instance whose only new item was promoted to
		// the Urgent shelf still had a delta (state must not read "no changes").
		state := classifyState(cfg, key, now, delta, len(items) > 0 || len(urgent) > 0)
		if items == nil {
			items = []DigestItem{}
		}
		sec := DigestSection{Source: key, State: state, Items: items}
		if moreCount > 0 {
			sec.MoreCount = moreCount
			sec.Truncated = true
		}
		sections = append(sections, sec)
		plans = append(plans, briefCommitPlan{key: key, snap: snap, delta: delta, lineMembers: lineMembers, countOnlyIDs: countOnly})
	}
	sortSections(sections)
	shelf, shelfMore := assembleUrgentShelf(urgentAll)

	// StaleTasks come from vault/live-tasks.md and are sync-independent — they are
	// NOT gated by the watermark (D-03 note).
	stale, _ := staleTasks(cfg, 3)
	hSnap := healthOf(cfg, now)
	return Digest{
		Generated:      now.UTC().Format(time.RFC3339),
		SinceHours:     0,
		Urgent:         shelf,
		UrgentMore:     shelfMore,
		Sections:       sections,
		Freshness:      sourceFreshness(cfg),
		StaleTasks:     stale,
		SourceHealth:   hSnap.Sources,
		idxHealth:      hSnap.Index,
		producerHealth: hSnap.Producers,
	}, plans, nil
}

// assembleUrgentShelf orders the cross-source urgent entries (most-recent arrival
// first, salience only as a tie-break — issue #62 keeps salience a tie-breaker, never
// the urgency key) and caps the shelf at urgentShelfCap. Overflow entries are NOT
// rendered and NOT committed, so they re-surface next run rather than being marked
// seen — the same safe pattern as a budget-clipped item.
func assembleUrgentShelf(entries []urgentEntry) (items []DigestItem, more int) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			return entries[i].score > entries[j].score // starred/important/unread + deadline boost.
		}
		if !entries[i].occurredAt.Equal(entries[j].occurredAt) {
			return entries[i].occurredAt.After(entries[j].occurredAt)
		}
		if entries[i].sal != entries[j].sal {
			return entries[i].sal > entries[j].sal
		}
		return entries[i].item.ID < entries[j].item.ID
	})
	for i, e := range entries {
		if i >= urgentShelfCap {
			more = len(entries) - urgentShelfCap
			break
		}
		items = append(items, e.item)
	}
	return items, more
}

// advanceBrief runs the scheduled --advance transaction (issue #62 defect 1). Under
// the brief lock it: (1) builds the delta digest + per-instance commit plans, (2)
// STRUCTURALLY budgets the brief to budgetChars of rendered Markdown, (3) persists
// the dated artifact when briefFile is set, and (4) commits each instance's watermark
// over ONLY the items that survived into the budgeted brief. A budget-clipped item is
// never marked seen, so it re-surfaces next run instead of being silently lost; and
// when persisting, a write failure ABORTS the commit (safer than the pre-#62
// non-fatal persist) so the watermark never advances past a brief the user never got.
//
// It returns the BUDGETED digest (so the caller renders/persists the same bytes the
// commit reflects) and the artifact path ("" when briefFile is false). A filtered or
// windowed request is preview-only and rejected — it can never advance the watermark.
func advanceBrief(cfg Config, now time.Time, opts briefOpts, budgetChars int, briefFile bool) (Digest, string, error) {
	if opts.filtered() {
		return Digest{}, "", fmt.Errorf("a filtered digest (--source/--entity/--scope/--since-days) is preview-only and cannot --advance the watermark")
	}
	if opts.sinceHours > 0 {
		return Digest{}, "", fmt.Errorf("an explicit --since-hours window is watermark-independent and cannot --advance")
	}
	if budgetChars <= 0 {
		budgetChars = cfg.contextDefaultTokens() * charsPerToken
	}

	// Hold the brief lock across the WHOLE transaction (build → budget → persist →
	// commit) so a hand-run --advance racing the cron never interleaves and never
	// commits around a half-written artifact (T-12-07 + issue #62 defect 1).
	release, lerr := acquireBriefLock(cfg)
	if lerr != nil {
		return Digest{}, "", fmt.Errorf("brief commit in progress (another --advance run holds the lock): %w", lerr)
	}
	defer release()

	perSourceCap, byInstance, memSal, _, err := digestInputs(cfg, now, opts)
	if err != nil {
		return Digest{}, "", err
	}
	commitmentsByMemory, gated, err := digestCommitmentInventory(cfg, now)
	if err != nil {
		return Digest{}, "", err
	}
	if gated {
		commitmentsByMemory = dailyCommitmentInventory(commitmentsByMemory, now)
		byInstance = filterDigestInstancesByCommitments(byInstance, commitmentsByMemory)
	}
	d, plans, err := buildDeltaDigest(cfg, now, opts, perSourceCap, byInstance, memSal)
	if err != nil {
		return Digest{}, "", err
	}
	attachDigestCommitments(&d, commitmentsByMemory)

	budgeted, survived := budgetDigestForMarkdown(d, budgetChars)

	// Persist FIRST when requested; abort the commit on failure.
	path := ""
	if briefFile {
		p, werr := writeBriefArtifactAt(cfg, budgeted, now, budgetChars)
		if werr != nil {
			return Digest{}, "", fmt.Errorf("persist brief artifact: %w", werr)
		}
		path = p
	}

	for _, plan := range plans {
		committed, intended := map[string]bool{}, map[string]bool{}
		sectionRendered := false
		for lineID, members := range plan.lineMembers {
			for _, m := range members {
				intended[m] = true
			}
			if survived[lineID] {
				sectionRendered = true
				for _, m := range members {
					committed[m] = true
				}
			}
		}
		for _, id := range plan.countOnlyIDs {
			intended[id] = true
		}
		if sectionRendered {
			// Preserve the pre-#62 count-only acknowledgement: a section that rendered
			// still acknowledges its folded low-signal tail (receipts stay collapsed,
			// they do not re-surface every day).
			for _, id := range plan.countOnlyIDs {
				committed[id] = true
			}
		}
		next := commitSnapshot(plan.snap, plan.delta, committed, intended, plan.key)
		if serr := saveBriefSnapshot(cfg, next, now); serr != nil {
			return Digest{}, "", fmt.Errorf("commit watermark for %q: %w", plan.key, serr)
		}
	}
	return budgeted, path, nil
}

// deltaSectionItems turns a classify result into the section's rendered items.
//
//   - cold start: surface the COURTESY window (last 7d by created_at; calendar =
//     upcoming 7d) from the instance's memories (D-04). The baseline-all behavior
//     lives in classify; here we only choose what to DISPLAY.
//   - steady state: surface delta.Items (new/updated), recency-ordered, capped.
//
// It returns the items to render, the URGENT entries lifted onto the cross-source
// shelf (issue #62 defect 2 — rescued from the per-source cap so a low-volume deadline
// email is never buried), a map from each displayed/shelf line's id to the stable
// memory ids it represents (lineMembers — a collapsed series line stands for all its
// instances), the stable ids of the low-signal tail folded into "+N more"
// (countOnlyIDs — acknowledged but not rendered), and the count truncated past the
// cap. advanceBrief advances the watermark over a line's members iff that line
// survives the Markdown budget (issue #62 defect 1); a shelf line is budget-protected
// and therefore always survives.
func deltaSectionItems(cfg Config, delta briefDelta, mems []Memory, now time.Time, key string, cap int, memSal map[string]int64) (items []DigestItem, urgent []urgentEntry, lineMembers map[string][]string, countOnlyIDs []string, moreCount int) {
	isCalendar := connectorUpcoming(key)

	// candidateMem yields (memory, change, include) for each item this instance would
	// surface — the cold-start courtesy window or the steady-state delta set.
	type cand struct {
		m      Memory
		change string
	}
	var cands []cand
	if delta.ColdStart {
		for _, m := range mems {
			ts, err := time.Parse(time.RFC3339, m.CreatedAt)
			if err != nil || !inColdStartWindow(ts, now, isCalendar) {
				continue // out-of-window archive: baselined by commitSnapshot (flood suppression).
			}
			cands = append(cands, cand{m: m, change: "new"}) // cold start: everything is "new".
		}
	} else {
		byID := map[string]Memory{}
		for _, m := range mems {
			byID[m.ID] = m
		}
		for _, di := range delta.Items {
			if m, ok := byID[di.ID]; ok {
				cands = append(cands, cand{m: m, change: di.Change})
			}
		}
	}

	// Partition urgent items OUT of the normal section flow (they bypass the cap and
	// lead the brief on the shelf); the rest go through series-collapse + cap +
	// low-signal collapse as before.
	var tis []tsItem
	for _, c := range cands {
		if ok, phrase := isUrgent(c.m, now); ok {
			urgent = append(urgent, urgentEntry{
				item:       urgentItemFor(cfg, c.m, key, c.change, phrase),
				occurredAt: itemOccurredAt(c.m),
				sal:        memSal[c.m.ID],
				score:      urgencyScore(c.m, phrase),
			})
			continue
		}
		ts, err := time.Parse(time.RFC3339, c.m.CreatedAt)
		if err != nil {
			ts = time.Time{} // unparsable created_at sorts last; still shown.
		}
		it := digestItemFor(cfg, c.m, key, c.change)
		it.LowSignal = isLowSignalItem(c.m, c.change, now)
		tis = append(tis, tsItem{item: it, ts: ts, sal: memSal[c.m.ID], series: recurringSeriesID(c.m)})
	}

	tis = collapseRecurringSeries(tis, now)
	memberOf := lineMemberMap(tis)
	shown, more := capRecency(tis, cap, isCalendar)
	// Collapse the zero-salience tail so the brief leads with signal; the folded ids
	// are count-only acknowledged (never re-surfaced) while the surviving lines drive
	// the commit. On cold start the cap-`more` overflow stays part of the baselined
	// archive (starting line, not a truncated delta).
	displayed, lm, countOnly := splitDisplayLowSignal(shown, memberOf)
	for _, u := range urgent {
		lm[u.item.ID] = []string{u.item.ID} // a shelf line commits its own id when it renders.
	}
	return displayed, urgent, lm, countOnly, more + (len(shown) - len(displayed))
}

// urgentEntry is one shelf candidate carried up from a section for cross-source
// ordering: the rendered item, its label/deadline urgency score (primary sort), and
// its arrival instant + salience (tie-breaks).
type urgentEntry struct {
	item       DigestItem
	occurredAt time.Time
	sal        int64
	score      int
}

// urgentItemFor builds a shelf DigestItem: like digestItemFor but with a
// deadline-anchored snippet (defect 4) instead of the blind tail clip, so the visible
// snippet carries the ask, not a sign-off.
func urgentItemFor(cfg Config, m Memory, key, change, phrase string) DigestItem {
	return DigestItem{
		ID:        m.ID,
		Title:     m.Title,
		Source:    key,
		CreatedAt: m.CreatedAt,
		Snippet:   urgentSnippet(m.Text, cfg.digestSnippetChars(), phrase),
		Change:    change,
	}
}

// lineMemberMap maps each tsItem line's id to the stable memory ids it represents (a
// collapsed recurring series stands for every folded instance; a plain line stands
// for itself), so the watermark can advance over the WHOLE set a rendered line covers.
func lineMemberMap(tis []tsItem) map[string][]string {
	out := make(map[string][]string, len(tis))
	for _, ti := range tis {
		if len(ti.members) > 0 {
			out[ti.item.ID] = ti.members
		} else {
			out[ti.item.ID] = []string{ti.item.ID}
		}
	}
	return out
}

// splitDisplayLowSignal partitions a capped, capRecency-ordered line set into the
// rendered lines (every salient line plus at most digestLowSignalFloor low-signal
// lines) with their line→members map, and the stable ids of the low-signal tail
// folded into "+N more" (count-only acknowledged). Input MUST be capRecency order so
// the low-signal items are already a recency-sorted tail.
func splitDisplayLowSignal(shown []DigestItem, memberOf map[string][]string) (displayed []DigestItem, lineMembers map[string][]string, countOnly []string) {
	lineMembers = map[string][]string{}
	kept := 0
	for _, it := range shown {
		members := memberOf[it.ID]
		if len(members) == 0 {
			members = []string{it.ID}
		}
		if it.LowSignal {
			if kept >= digestLowSignalFloor {
				countOnly = append(countOnly, members...)
				continue
			}
			kept++
		}
		displayed = append(displayed, it)
		lineMembers[it.ID] = members
	}
	return displayed, lineMembers, countOnly
}

// classifyState derives the three-state label for an enumerated instance (D-03).
// hasDelta reports whether the section surfaced any new/updated/cold-start items.
//
// Order (first match wins):
//  1. unavailable — a recorded sync error (LastError/ErrorCount, now correctly
//     reset on recovery by M-3) OR never synced (no clean LastSuccessAt).
//  2. stale       — last clean sync older than 48h.
//  3. delta       — surfaced new/updated (or a cold-start courtesy window).
//  4. no changes  — synced recently, no error, nothing new.
func classifyState(cfg Config, key string, now time.Time, delta briefDelta, hasDelta bool) string {
	st := loadConnectorSyncStatus(cfg, key)
	if st == nil || st.LastError != "" || st.ErrorCount > 0 || st.LastSuccessAt == "" {
		return stateUnavailable
	}
	// Staleness is measured against the INJECTED now (not time.Since, which reads
	// the real wall clock) so tests are deterministic and the 48h clock is honest
	// under an injected now. (D-03, determinism invariant.)
	if t, err := time.Parse(time.RFC3339, st.LastSuccessAt); err == nil {
		if now.Sub(t) > digestStaleHours*time.Hour {
			return stateStale
		}
	}
	if delta.ColdStart {
		// First run for a healthy source: it is a baseline, not "no changes" (which
		// would mislead on an initial sync — the 7d courtesy window IS shown).
		return stateColdStart
	}
	if hasDelta {
		return stateDelta
	}
	return stateNoChanges
}

// commitSnapshot computes the watermark to persist for one instance AFTER the
// Markdown budget has decided what actually rendered (issue #62 defect 1). committed
// is the set of stable ids that survived into rendered lines (plus the count-only
// acknowledged low-signal ids of a section that rendered). intendedDisplay is every
// stable id the section CHOSE to show this run (rendered OR budget-dropped) — used
// only on cold start to carve the display window out of the blanket baseline.
//
//   - cold start: baseline the ARCHIVE/starting-line (every current hash NOT chosen
//     for display) so a large backfill doesn't flood later runs (D-04), then advance
//     over exactly the display items that actually rendered. A chosen-but-clipped
//     display item is intentionally OMITTED, so it re-surfaces next run as a normal
//     "new" delta instead of being silently swallowed on the first run (defect 1b).
//   - steady state: keep the PREVIOUS snapshot value EXACTLY for every unshown id
//     still present (so an unshown updated item keeps its OLD hash and re-surfaces —
//     never silently marked-read), UNION the current hashes of items actually
//     committed (rendered), MINUS any ids no longer present (deleted/tombstoned —
//     M-4 drop, so a later same-id recreation re-surfaces as new).
func commitSnapshot(prev briefSnapshot, delta briefDelta, committed, intendedDisplay map[string]bool, key string) briefSnapshot {
	items := map[string]string{}
	if delta.ColdStart {
		for id, h := range delta.Baseline {
			if !intendedDisplay[id] {
				items[id] = h // archive / starting line (flood suppression, D-04).
			}
		}
		for id := range committed {
			if h, ok := delta.Baseline[id]; ok {
				items[id] = h // a display item that actually rendered.
			}
		}
		return briefSnapshot{Key: key, Items: items}
	}
	// Keep prior entries ONLY for ids still present this run (delta.Baseline holds
	// every present, non-empty-key id's current hash). An id in prev but absent
	// from Baseline was deleted/tombstoned → DROP it (M-4).
	for id, prevHash := range prev.Items {
		if _, present := delta.Baseline[id]; present {
			items[id] = prevHash // unshown id: keep OLD hash so an update re-surfaces.
		}
	}
	// Advance the hash ONLY for items actually committed (rendered) this run.
	for id := range committed {
		if h, ok := delta.Baseline[id]; ok {
			items[id] = h
		}
	}
	return briefSnapshot{Key: key, Items: items}
}

// tsItem carries the parsed instant alongside the item so sorting/capping is
// timezone-correct (raw RFC3339 string order misranks mixed offsets). sal is the
// item's salience_micros (SC#3): the max-participant salience from the shared
// 14-01 kernel, the PRIMARY sort key in capRecency so the most-salient item leads
// each section and survives the cap.
type tsItem struct {
	item DigestItem
	ts   time.Time
	sal  int64
	// series is the recurring_event_id (calendar) this item belongs to, "" if not
	// recurring. members are the memory IDs folded into this item by
	// collapseRecurringSeries (its own id plus every sibling instance it now
	// represents) — the watermark must advance over ALL of them so a collapsed
	// series is never re-surfaced instance-by-instance on the next run.
	series  string
	members []string
}

// recurringSeriesID returns a memory's recurring-series id (Google Calendar's
// meta.recurring_event_id), or "" when the memory is not a recurring instance.
func recurringSeriesID(m Memory) string {
	if m.Meta == nil {
		return ""
	}
	id, _ := m.Meta["recurring_event_id"].(string)
	return id
}

// collapseRecurringSeries folds tsItems that share a recurring-series id into ONE
// representative line so a single daily/weekly series can't fill a calendar
// section (and, via the byte budget, starve other sources). For each series the
// representative is the NEAREST-future instance (the soonest occurrence at/after
// now; the latest past instance if all are past), and its title is annotated with
// "(×N through <last date>)" so the agent still sees the cadence and span. The
// representative carries members = every folded instance's memory id, so the delta
// watermark advances over the whole series (no per-instance re-flood next run).
// Non-recurring items pass through untouched. Output order is the representative's
// best position; capRecency re-sorts afterward, so order here only needs to be
// deterministic.
func collapseRecurringSeries(tis []tsItem, now time.Time) []tsItem {
	// Bucket COLLAPSIBLE indices by series; preserve first-seen order for
	// determinism. An "updated" instance is NEVER folded: a single rescheduled
	// occurrence is a real, instance-specific change the reader must see (and, in
	// delta mode, folding it would mark its update acknowledged without ever
	// surfacing it). Only new/window instances — the actual flood — collapse.
	collapsible := func(ti tsItem) bool { return ti.series != "" && ti.item.Change != "updated" }
	groups := map[string][]int{}
	var order []string
	for i, ti := range tis {
		if !collapsible(ti) {
			continue
		}
		if _, seen := groups[ti.series]; !seen {
			order = append(order, ti.series)
		}
		groups[ti.series] = append(groups[ti.series], i)
	}
	if len(groups) == 0 {
		return tis // nothing collapsible — fast path, byte-identical.
	}

	out := make([]tsItem, 0, len(tis))
	// Pass-through every item that is NOT part of a multi-instance collapsible
	// group: non-recurring items, individually-changed (updated) instances, and a
	// lone instance whose series has just one member this run.
	for _, ti := range tis {
		if !collapsible(ti) || len(groups[ti.series]) == 1 {
			out = append(out, ti)
		}
	}
	// One representative per multi-instance series.
	for _, sid := range order {
		idxs := groups[sid]
		if len(idxs) < 2 {
			continue
		}
		rep := idxs[0]
		var last time.Time
		var bestSal int64
		members := make([]string, 0, len(idxs))
		for _, k := range idxs {
			members = append(members, tis[k].item.ID)
			if tis[k].ts.After(last) {
				last = tis[k].ts
			}
			if tis[k].sal > bestSal {
				bestSal = tis[k].sal
			}
			if betterSeriesRep(tis[k].ts, tis[rep].ts, now) {
				rep = k
			}
		}
		rti := tis[rep]
		rti.sal = bestSal // a series leads on its most-salient instance.
		rti.members = members
		rti.item.Title = fmt.Sprintf("%s (×%d through %s)", rti.item.Title, len(idxs), last.UTC().Format("Jan 2"))
		out = append(out, rti)
	}
	return out
}

// betterSeriesRep reports whether instant a is a better series representative than
// b relative to now: the nearest future occurrence wins; if both are future the
// EARLIER wins; if neither is future the LATER (most recent past) wins.
func betterSeriesRep(a, b, now time.Time) bool {
	aFut, bFut := !a.Before(now), !b.Before(now)
	if aFut != bFut {
		return aFut // a future instance beats a past one.
	}
	if aFut { // both future: soonest.
		return a.Before(b)
	}
	return a.After(b) // both past: most recent.
}

// capRecency orders items SALIENCE-FIRST then most-recent (the existing instant +
// id tie-break) and keeps the first cap, returning the kept items and the count
// truncated past the cap (SC#3). The order-BEFORE-truncate split is the seam this
// re-sort plugs into without re-entering buildDigest: because the sort runs BEFORE
// truncation, the most-salient item both LEADS its section and SURVIVES the cap —
// a high-salience item is never dropped in favor of a noisier recent one. Among
// equal-salience items (e.g. all 0-salience service/no-participant notifications)
// the existing recency-then-id order is preserved, so humans lead and services
// sink to the bottom. The comparator is total and deterministic (salience int64 →
// instant → id), so two passes over the same input are byte-identical.
//
// upcoming flips the recency tie-break for future-dated sources (calendar): there
// the NEAREST event should lead and survive the cap, so equal-salience items sort
// EARLIEST-instant-first instead of most-recent-first. Past-oriented sources keep
// most-recent-first. (Bug: a 48h brief used to rank the farthest-future event of a
// calendar section first.)
func capRecency(tis []tsItem, cap int, upcoming bool) (items []DigestItem, more int) {
	sort.SliceStable(tis, func(i, j int) bool {
		if tis[i].sal != tis[j].sal {
			return tis[i].sal > tis[j].sal // salience DESC is the primary key (SC#3).
		}
		if !tis[i].ts.Equal(tis[j].ts) {
			if upcoming {
				return tis[i].ts.Before(tis[j].ts) // nearest-future-first for events.
			}
			return tis[i].ts.After(tis[j].ts) // then most-recent-first.
		}
		return tis[i].item.ID < tis[j].item.ID // deterministic tie-break on exact-instant ties.
	})
	if len(tis) > cap {
		more = len(tis) - cap
		tis = tis[:cap]
	}
	out := make([]DigestItem, len(tis))
	for i, ti := range tis {
		out[i] = ti.item
	}
	return out, more
}

// digestLowSignalFloor is how many of a section's MOST-RECENT zero-salience items
// stay visible before the rest collapse into the "+N more" count. Keeping a small
// floor (not zero) means a genuinely recent automated notice — a security alert, an
// OAuth-added notice — still surfaces, while a window of 8 receipts/newsletters
// shrinks to 2 + a count instead of drowning the section.
const digestLowSignalFloor = 2

// collapseLowSignal trims a section's zero-salience tail: it keeps every salient
// (LowSignal==false) item and at most digestLowSignalFloor of the most-recent
// low-signal items, folding the rest into collapsed. Input MUST be capRecency order
// (salience-desc, then recency), so the low-signal items are already a recency-sorted
// tail. It is DISPLAY-only: the caller still marks the full capped set as
// acknowledged for the Phase-12 watermark, so collapsed items are counted (never
// re-surfaced), not silently dropped.
func collapseLowSignal(items []DigestItem) (displayed []DigestItem, collapsed int) {
	kept := 0
	for _, it := range items {
		if it.LowSignal {
			if kept >= digestLowSignalFloor {
				collapsed++
				continue
			}
			kept++
		}
		displayed = append(displayed, it)
	}
	return displayed, collapsed
}

// digestItemFor builds a DigestItem from a memory, stamping the instance key as
// the section Source and the typed Change. The snippet is TAIL-biased:
// conversation memories (iMessage chats, gmail threads) append chronologically,
// so the user's own replies live at the end of the body — a head-clip
// systematically shows the other party's messages and drops the reply, and the
// consuming agent then reports a replied-to thread as "unanswered". The digest
// is "what's new"; the newest content is the tail.
func digestItemFor(cfg Config, m Memory, key, change string) DigestItem {
	return DigestItem{
		ID:        m.ID,
		Title:     m.Title,
		Source:    key,
		CreatedAt: m.CreatedAt,
		Snippet:   snippetTail(m.Text, cfg.digestSnippetChars()),
		Change:    change,
	}
}

// snippetTail is snippet's end-anchored twin: it keeps the LAST n content runes
// and marks the elision at the start. Short bodies pass through whole.
func snippetTail(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return "…" + strings.TrimSpace(string(r[len(r)-n:]))
}

// inColdStartWindow reports whether a memory's instant falls in the cold-start
// courtesy window: the last 7 days for gmail/imessage (by created_at), or the
// UPCOMING 7 days for calendar (its natural framing — events are future-dated).
// Window math stays now.Add(±N*time.Hour) + parsed-instant compare (DST-safe;
// not converted to calendar arithmetic). (D-04)
func inColdStartWindow(ts, now time.Time, isCalendar bool) bool {
	if isCalendar {
		end := now.Add(time.Duration(digestColdStartDays) * 24 * time.Hour)
		return !ts.Before(now) && !ts.After(end)
	}
	start := now.Add(-time.Duration(digestColdStartDays) * 24 * time.Hour)
	return !ts.Before(start)
}

// loadConnectorSyncStatus loads the on-disk SyncStatus for an enumerated
// connector instance. The sync file family is provider-derived (gmail/calendar →
// sync/google-<name>.json; imessage → sync/imessage-<name>.json), and today
// Source.Name == connector type for the ingesting connectors, so we resolve the
// status across the enabled sources of that type. Returns nil if no status file
// exists (never-synced) so classifyState reads "unavailable".
func loadConnectorSyncStatus(cfg Config, key string) *memory.SyncStatus {
	sources, err := loadSources(cfg)
	if err != nil {
		return nil
	}
	for _, s := range sources {
		// Exact instance-key match (the source-side twin of sourceInstanceKey):
		// "gmail:work" resolves to THAT account's status file, never the default
		// mailbox's — a healthy personal sync must not mask a broken work sync.
		if instanceKeyForSource(s) != key || !s.IsEnabled() {
			continue
		}
		path := syncStatusPathFor(cfg, s)
		if path == "" {
			continue
		}
		st, lerr := memory.LoadStatus(path)
		if lerr != nil || st == nil {
			continue
		}
		// A present status file with a LastAttemptAt/LastSynced is the real signal;
		// a fully-zero status (file absent → LoadStatus zero value) reads as
		// never-synced below. Return the first enabled source's status of this type.
		return st
	}
	return nil
}

// syncStatusPathFor maps a source to its on-disk SyncStatus path, mirroring the
// connector filename families used by the ingest paths.
func syncStatusPathFor(cfg Config, s Source) string {
	switch s.Type {
	case "gmail", "calendar":
		return filepath.Join(cfg.StateDir, "sync", "google-"+s.Name+".json")
	case "imessage":
		return filepath.Join(cfg.StateDir, "sync", "imessage-"+s.Name+".json")
	case "applecalendar":
		return filepath.Join(cfg.StateDir, "sync", "applecal-"+s.Name+".json")
	case "filesystem":
		return filepath.Join(cfg.StateDir, "sync", "filesystem-"+s.Name+".json")
	case "github":
		return filepath.Join(cfg.StateDir, "sync", "github-"+s.Name+".json")
	default:
		return ""
	}
}

// sortedInstanceKeys returns a map's keys in deterministic (sorted) order so
// section assembly never depends on Go map iteration order (byte-stability
// invariant).
func sortedInstanceKeys(m map[string][]Memory) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortSections orders sections by the connector descriptor Rank (M-6) — so the
// most time-sensitive channel leads and survives budget truncation — then by key
// for a stable tie-break. An Nth connector gets a real rank, not the old
// default-rank-3 that was first-to-truncate.
func sortSections(sections []DigestSection) {
	sort.SliceStable(sections, func(i, j int) bool {
		ri, rj := sourceDigestRank(sections[i].Source), sourceDigestRank(sections[j].Source)
		if ri != rj {
			return ri < rj
		}
		return sections[i].Source < sections[j].Source
	})
}

// --- Markdown render fragments (issue #62 defect 1) ----------------------------
//
// renderDigest and the structural budgeter (budgetDigestForMarkdown) share these
// fragment helpers so their byte accounting can never drift: the budgeter costs the
// EXACT bytes the renderer will emit for each item/section, guaranteeing that an
// item the budgeter reports as a survivor is fully present in the rendered brief.

// renderDigestHeader renders the brief's first line.
func renderDigestHeader(d Digest) string {
	if d.SinceHours > 0 {
		return fmt.Sprintf("# Mora digest — %s (last %dh)\n", d.Generated, d.SinceHours)
	}
	return fmt.Sprintf("# Mora digest — %s (since last brief)\n", d.Generated)
}

// renderDigestHealthBanner renders the red health-alarm line (HEALTH-02), or ""
// when every enabled source is fresh. Pure over d.SourceHealth — no cfg/now
// needed at render time; sourceHealthAll already pinned the snapshot at build
// time (D-03: no time.Now() in a render path). This is the FIRST content line
// after the header, before "Fresh as of:" — "stale" must never again be a
// heading string with no reader.
func renderDigestHealthBanner(d Digest) string {
	// Gate 2: the banner is now the AGGREGATE worst arm across sources AND the index
	// (producers arrive with PR 4). Both are pure snapshots pinned at build time, so
	// the render stays clock-free. Still exactly ONE line.
	banner := healthBannerFrom(Health{Sources: d.SourceHealth, Index: d.idxHealth, Producers: d.producerHealth})
	if banner == "" {
		return ""
	}
	return banner + "\n"
}

// renderDigestFreshness renders the "Fresh as of:" line, or "" when absent.
func renderDigestFreshness(d Digest) string {
	if len(d.Freshness) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d.Freshness))
	for k := range d.Freshness {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" "+d.Freshness[k])
	}
	return fmt.Sprintf("Fresh as of: %s\n", strings.Join(parts, " · "))
}

// renderDigestSectionHeading renders the "\n## <heading>\n" fragment.
func renderDigestSectionHeading(s DigestSection) string {
	return "\n## " + sectionHeading(s) + "\n"
}

func renderDigestArtifactLine(it DigestItem) string {
	return fmt.Sprintf("- %s%s — %s (id: %s)\n", changePrefix(it.Change), it.Title, it.Snippet, it.ID)
}

func renderDigestObligationRow(obligation DigestObligation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  obligation: commitment_id=%s · owner=%s:%s · direction=%s%s · due=%s · lifecycle=%s · closure=%s · summary=%s\n",
		obligation.CommitmentID, obligation.Owner.Kind, obligation.Owner.Value, obligation.Direction,
		renderCounterpartyLabel(obligation.CounterpartyLabel),
		obligation.DueAt, obligation.Lifecycle, obligation.ClosureRef, oneLine(obligation.Summary))
	for _, citation := range obligation.Citations {
		fmt.Fprintf(&b, "    citation: role=%s · memory_id=%s · commitment_id=%s\n",
			citation.Role, citation.Citation.MemoryID(), citation.CommitmentID)
	}
	return b.String()
}

// renderDigestItemLine renders one artifact and all of its nested commitment rows.
// The budgeter treats the complete block as one indivisible artifact-grain item.
func renderDigestItemLine(it DigestItem) string {
	line := renderDigestArtifactLine(it)
	if len(it.Obligations) > 0 {
		var b strings.Builder
		b.WriteString(line)
		for _, obligation := range it.Obligations {
			b.WriteString(renderDigestObligationRow(obligation))
		}
		return b.String()
	}
	if it.Direction == "" {
		return line
	}
	return line + fmt.Sprintf("  obligation: owner=%s:%s · direction=%s%s · due=%s · lifecycle=%s · closure=%s\n",
		it.Owner.Kind, it.Owner.Value, it.Direction, renderCounterpartyLabel(it.CounterpartyLabel),
		it.DueAt, it.Lifecycle, it.ClosureRef)
}

func renderCounterpartyLabel(label string) string {
	if label = strings.TrimSpace(label); label != "" {
		return " · counterparty=" + label
	}
	return ""
}

// renderDigestMoreLine renders the "+N more since last brief" guard line.
func renderDigestMoreLine(n int) string {
	return fmt.Sprintf("- +%d more since last brief\n", n)
}

// renderDigestStaleTasks renders the open-tasks block, or "" when there are none.
func renderDigestStaleTasks(d Digest) string {
	if len(d.StaleTasks) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Open tasks (%d stale)\n", len(d.StaleTasks))
	for _, task := range d.StaleTasks {
		fmt.Fprintf(&b, "- %s\n", task)
	}
	return b.String()
}

// renderDigestUrgentShelf renders the "⚠ Urgent" shelf ABOVE the sections, or "" when
// empty. The shelf is budget-protected (reserved by budgetDigestForMarkdown), so its
// deadline items always render regardless of how tight the byte budget is (issue #62
// defect 2).
func renderDigestUrgentShelf(d Digest) string {
	if len(d.Urgent) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(renderDigestUrgentHeading(len(d.Urgent)))
	for _, it := range d.Urgent {
		b.WriteString(renderDigestItemLine(it))
	}
	if d.UrgentMore > 0 {
		fmt.Fprintf(&b, "- +%d more urgent\n", d.UrgentMore)
	}
	return b.String()
}

// renderDigestUrgentHeading renders the shelf heading; factored so the budgeter can
// cost it exactly when fitting shelf items to the budget.
func renderDigestUrgentHeading(n int) string {
	return fmt.Sprintf("\n## ⚠ Urgent (%d)\n", n)
}

// renderDigestBody renders a digest to Markdown with NO budget clip — the digest is
// assumed already budgeted (budgetDigestForMarkdown). It composes the fragment
// helpers so the output is byte-identical to the legacy inline renderer (plus the
// Urgent shelf when present).
func renderDigestBody(d Digest) string {
	var b strings.Builder
	b.WriteString(renderDigestHeader(d))
	b.WriteString(renderDigestHealthBanner(d))
	b.WriteString(renderDigestFreshness(d))
	b.WriteString(renderDigestUrgentShelf(d))
	for _, s := range d.Sections {
		b.WriteString(renderDigestSectionHeading(s))
		for _, it := range s.Items {
			b.WriteString(renderDigestItemLine(it))
		}
		if s.MoreCount > 0 {
			b.WriteString(renderDigestMoreLine(s.MoreCount))
		}
	}
	b.WriteString(renderDigestStaleTasks(d))
	return b.String()
}

// renderDigest renders the brief as Markdown, STRUCTURALLY budgeted to budgetChars:
// the time-sensitive sections lead, and truncation drops whole tail ITEMS/SECTIONS
// (never a mid-line rune clip) so the persisted brief and the Phase-12 watermark
// commit agree on exactly which items were shown (issue #62 defect 1). The final
// truncateRunes is a safety net for frame/shell overshoot only; by construction it
// never bites a kept item line.
func renderDigest(d Digest, budgetChars int) string {
	if budgetChars <= 0 {
		budgetChars = defaultContextTokens * charsPerToken
	}
	bd, _ := budgetDigestForMarkdown(d, budgetChars)
	return truncateRunes(renderDigestBody(bd), budgetChars)
}

// budgetDigestForMarkdown structurally budgets a digest to budgetChars of RENDERED
// Markdown, cutting at item/section boundaries, and returns the budgeted digest plus
// the set of DigestItem IDs that survived as rendered lines — the ONLY ids the
// Phase-12 watermark may advance over on the scheduled --advance path (issue #62
// defect 1: the commit follows what actually rendered, not the pre-truncation cap).
//
// The reserved frame (header + freshness + the bounded open-tasks block) is always
// kept; sections then fill the remaining budget highest-rank-first, item by item.
// A section that partially fits keeps its fitting items and folds the rest into
// MoreCount/Truncated; once the budget is spent, every remaining section becomes a
// truncated shell (State + MoreCount, no bodies) so the reader still sees the source
// was suppressed, not absent. Deterministic + byte-stable: section order is
// preserved and each cut is a pure prefix of a section's items.
func budgetDigestForMarkdown(d Digest, budget int) (Digest, map[string]bool) {
	survived := map[string]bool{}
	if budget <= 0 {
		budget = defaultContextTokens * charsPerToken
	}
	out := d

	// The Urgent shelf leads and is highest-priority, but still budget-bounded so the
	// "survived ⟹ rendered" invariant holds even when the shelf ALONE overflows the
	// budget: fit as many shelf items as the header/freshness/open-tasks frame leaves
	// room for, mark ONLY those as survivors, and fold the rest into UrgentMore
	// (unrendered => NOT committed => re-surfaces next run). At the normal budget the
	// whole shelf (≤ urgentShelfCap items) fits and nothing is trimmed.
	// The health banner (▸CX budget accounting) is reserved in the SAME frame as
	// header/freshness/tasks: it renders unconditionally as the first content
	// line, outside any section, so its bytes must be accounted for here — a
	// banner rendered outside this frame would let final truncation cut an item
	// already marked as a budget survivor (the watermark/render invariant).
	frame := len(renderDigestHeader(d)) + len(renderDigestHealthBanner(d)) + len(renderDigestFreshness(d)) + len(renderDigestStaleTasks(d))
	if len(d.Urgent) > 0 {
		// Reserve the shelf heading (costed exactly at the full count, an over-estimate
		// of the possibly-fewer fitted count). No "+N more urgent" reserve: if trimming
		// makes that line render, the only possible overshoot lands in the tail
		// section-shells (no survived ids), never the front-rendered shelf.
		used := frame + len(renderDigestUrgentHeading(len(d.Urgent)))
		fit := 0
		for _, it := range d.Urgent {
			c := len(renderDigestItemLine(it))
			if used+c > budget {
				break
			}
			used += c
			survived[it.ID] = true
			fit++
		}
		if fit < len(d.Urgent) {
			out.Urgent = append([]DigestItem(nil), d.Urgent[:fit]...)
			out.UrgentMore = d.UrgentMore + (len(d.Urgent) - fit)
		}
	}

	// Sections budget against what remains after the frame + the (now bounded) shelf +
	// each section's CHROME (heading + a possible "+N more" line, charged at the full
	// count so the reservation is never an under-estimate). This keeps the budget a
	// trade-off of item BODIES only, so a survivor's line is never clipped by overshoot.
	reserve := frame + len(renderDigestUrgentShelf(out))
	for _, s := range d.Sections {
		reserve += len(renderDigestSectionHeading(s)) + len(renderDigestMoreLine(len(s.Items)+s.MoreCount))
	}
	remaining := budget - reserve
	if remaining < 0 {
		remaining = 0
	}

	n := len(d.Sections)
	kept := make([]int, n) // items kept per section — always a pure prefix.
	used := 0
	add := func(i, j int) bool {
		it := d.Sections[i].Items[j]
		cost := len(renderDigestItemLine(it))
		if used+cost > remaining {
			return false
		}
		used += cost
		kept[i]++
		survived[it.ID] = true
		return true
	}

	// Pass 1 (fair floor, issue #62 defect 3): give each section up to budgetSourceFloor
	// items in rank order, so a noisy high-rank source (calendar subscriptions) can't
	// starve a lower-rank one (Emails) below its floor.
	for i := range d.Sections {
		for j := 0; j < len(d.Sections[i].Items) && j < budgetSourceFloor; j++ {
			if !add(i, j) {
				break
			}
		}
	}
	// Pass 2 (greedy): fill the remaining items highest-rank-first until spent.
	for i := range d.Sections {
		for j := kept[i]; j < len(d.Sections[i].Items); j++ {
			if !add(i, j) {
				break
			}
		}
	}

	preBudgetCount := briefSurfacedItemCount(d)
	out.Sections = make([]DigestSection, 0, n)
	for i, s := range d.Sections {
		switch {
		case len(s.Items) == 0:
			ns := s
			if ns.Items == nil {
				ns.Items = []DigestItem{}
			}
			out.Sections = append(out.Sections, ns) // empty section: state only.
		case kept[i] == 0:
			out.Sections = append(out.Sections, truncatedShell(s)) // suppressed for budget.
		default:
			ns := DigestSection{Source: s.Source, State: s.State, MoreCount: s.MoreCount, Truncated: s.Truncated, ElidedByBudget: s.ElidedByBudget}
			ns.Items = append([]DigestItem{}, s.Items[:kept[i]]...)
			if dropped := len(s.Items) - kept[i]; dropped > 0 {
				ns.MoreCount += dropped
				ns.Truncated = true
				ns.ElidedByBudget += dropped
			}
			out.Sections = append(out.Sections, ns)
		}
	}
	if out.EmptyExplanation == "" && preBudgetCount > 0 && briefSurfacedItemCount(out) == 0 {
		out.EmptyExplanation = deriveEmptyExplanation(out, briefOpts{}, true)
	}
	return out, survived
}

// deriveEmptyExplanation returns a human-readable explanation when a brief has
// zero surfaced items across all sections and the urgent shelf.
func deriveEmptyExplanation(d Digest, opts briefOpts, budgetElidedAll bool) string {
	if briefSurfacedItemCount(d) > 0 && !budgetElidedAll {
		return ""
	}
	if budgetElidedAll {
		return "all items elided by token budget"
	}

	// Freshness uncertainty outranks absence claims. A filtered or mixed-state
	// request cannot honestly say "no matches" / "no changes" when one of the
	// source snapshots is stale or unavailable. SourceHealth covers explicit
	// window mode (whose empty sections have no delta state); section state is the
	// fallback for pure/unit callers that do not carry SourceHealth.
	uncertain, total := emptySourceUncertainty(d, opts.source)
	if uncertain > 0 {
		if uncertain == total {
			return "all source connectors are stale or unavailable"
		}
		return "some source connectors are stale or unavailable"
	}

	// This came from the same parsed, governance-filtered input pass as the digest
	// itself. Do not re-walk the vault here: a later read could describe a different
	// snapshot, and raw file count cannot distinguish tombstones/hidden revisions.
	if d.emptyEvidence.vaultRows == 0 {
		return "no memory items found in vault"
	}

	// since_hours is a watermark-independent window, never a delta. Keep its
	// explanation mode-aware so an empty one cannot claim "no changes since last
	// brief." When filters are combined with the window, name both constraints.
	if opts.sinceHours > 0 {
		if opts.filtered() && d.emptyEvidence.filteredRows == 0 {
			return "no memory items match the active filters in the requested time window"
		}
		return "no memory items found in requested time window"
	}
	if opts.filtered() && d.emptyEvidence.filteredRows == 0 {
		return "no memory items match the active filters"
	}

	hasColdStart := false
	hasNoChanges := false
	for _, s := range d.Sections {
		switch s.State {
		case stateColdStart:
			hasColdStart = true
		case stateNoChanges:
			hasNoChanges = true
		}
	}
	if hasColdStart {
		return "no memory items found in initial 7-day baseline window"
	}
	if hasNoChanges {
		return "no changes since last brief"
	}
	if d.emptyEvidence.vaultRows > 0 {
		return "no memory items surfaced in this brief"
	}
	return "no memory items found in vault"
}

// emptySourceUncertainty returns how many request-relevant source instances are
// stale/unavailable and how many relevant instances are represented in total.
// It merges the richer SourceHealth snapshot with delta section state by key,
// applies the same source-family filter as digest assembly, and treats either
// arm's uncertainty as authoritative. The result is count-only, so map iteration
// order cannot affect output.
func emptySourceUncertainty(d Digest, sourceFilter string) (uncertain, total int) {
	bySource := make(map[string]bool, len(d.SourceHealth)+len(d.Sections))
	for _, h := range d.SourceHealth {
		if !digestSourceMatches(h.Key, sourceFilter) {
			continue
		}
		bySource[h.Key] = h.State != healthFresh
	}
	for _, s := range d.Sections {
		if !digestSourceMatches(s.Source, sourceFilter) {
			continue
		}
		if _, ok := bySource[s.Source]; !ok {
			bySource[s.Source] = false
		}
		if s.State == stateStale || s.State == stateUnavailable {
			bySource[s.Source] = true
		}
	}
	for _, isUncertain := range bySource {
		if isUncertain {
			uncertain++
		}
	}
	return uncertain, len(bySource)
}

// budgetSourceFloor is how many items each source is guaranteed in the Markdown budget
// before any source is filled past it — the fair-budget floor that stops a noisy
// high-rank section from starving a lower-rank one (issue #62 defect 3).
const budgetSourceFloor = 2

// sectionHeading renders the per-section heading: the human label plus, in DELTA
// mode, the three-state sentinel (no-changes / stale / unavailable) or the item
// count for a live delta.
func sectionHeading(s DigestSection) string {
	label := digestSourceLabel(s.Source)
	switch s.State {
	case stateNoChanges:
		return label + " — no changes since last brief"
	case stateStale:
		return label + " — stale (no recent sync)"
	case stateUnavailable:
		return label + " — unavailable (sync error)"
	case stateColdStart:
		return fmt.Sprintf("%s — baseline (%d)", label, len(s.Items)+s.MoreCount)
	default: // stateDelta or plain-window
		return fmt.Sprintf("%s (%d)", label, len(s.Items)+s.MoreCount)
	}
}

// changePrefix renders the typed Change as a styled line prefix the human brief
// reads ([new] / [updated]); the plain-window path emits no prefix.
func changePrefix(change string) string {
	switch change {
	case "new":
		return "[new] "
	case "updated":
		return "[updated] "
	default:
		return ""
	}
}

// --- Plan 12-05 D-05: ONE budgeted MCP digest payload --------------------------

// sourceState is the per-instance three-state surfaced STRUCTURALLY in the MCP
// digest payload (SC#3 in JSON), so an agent reads the new/no-change/stale/
// unavailable state without parsing the Markdown. It is derived from the SAME
// typed Digest the human brief renders (one source of truth — M-5).
type sourceState struct {
	Instance   string `json:"instance"`    // the sourceInstanceKey ("gmail", …)
	State      string `json:"state"`       // new | no_change | stale | unavailable
	Count      int    `json:"count"`       // in-window/in-delta total (shown items + more_count)
	LastSynced string `json:"last_synced"` // "" when never synced
	Errored    bool   `json:"errored"`     // a recorded sync error (LastError/ErrorCount)
}

// mcpStateLabel maps the typed DigestSection.State (the human-brief sentinel) to
// the compact agent-facing state token. A live delta or a cold-start baseline are
// both "new" to the agent (there are surfaced items); the rest map 1:1.
func mcpStateLabel(state string) string {
	switch state {
	case stateNoChanges:
		return "no_change"
	case stateStale:
		return "stale"
	case stateUnavailable:
		return "unavailable"
	default: // stateDelta, stateColdStart (baseline), or plain-window
		return "new"
	}
}

// buildSourceStates derives the source_states array from the typed Digest. It
// reads each section's State + item count and joins the per-instance sync health
// (last_synced + errored) so the agent sees the three-state structurally. Ordered
// by the section order (rank), so it is deterministic.
func buildSourceStates(cfg Config, d Digest) []sourceState {
	out := make([]sourceState, 0, len(d.Sections))
	for _, s := range d.Sections {
		// Count is the section's OWN item total — shown items PLUS more_count
		// (truncated + low-signal-collapsed) — so the compact source_states never
		// diverges from what the section structurally reports (the bug was count:16
		// beside a section holding 16 shown + 34 more — card …liO8). It counts a
		// collapsed recurring series as the ONE line it renders (the ×N span rides in
		// that line's title, not in more_count), so count stays equal to the visible
		// line total an agent reads back, not the raw pre-collapse instance count.
		ss := sourceState{
			Instance: s.Source,
			State:    mcpStateLabel(s.State),
			Count:    len(s.Items) + s.MoreCount,
		}
		if st := loadConnectorSyncStatus(cfg, s.Source); st != nil {
			ss.LastSynced = st.LastSynced
			ss.Errored = st.LastError != "" || st.ErrorCount > 0
		}
		out = append(out, ss)
	}
	return out
}

// digestMCPPayload builds the ONE budgeted MCP digest payload (D-05): the typed-
// delta sections + the derived source_states, budgeted by budgetChars so it
// actually scales with max_tokens. It ships NO `digest` render string — that
// doubling (a clipped render string PLUS the full unclipped sections) is the bug
// this fixes; the CLI keeps the render path, the MCP payload is structured-only.
//
// Budgeting is greedy tail-trim (deterministic, byte-stable): the headers +
// source_states + StaleTasks are always included (their size is bounded), then
// sections are added highest-rank-first, item by item, until the remaining byte
// budget is exhausted. A section that can only partially fit keeps the items that
// fit and bumps its MoreCount/Truncated; sections that don't fit at all are
// dropped but STILL appear in source_states (the state is preserved, only the
// item bodies are budgeted away). This makes a 20k-token request strictly larger
// than the ~6k default whenever there is more content than the default can hold.
func digestMCPPayload(cfg Config, d Digest, budgetChars int) map[string]any {
	if budgetChars <= 0 {
		budgetChars = defaultContextTokens * charsPerToken
	}
	return digestMCPPayloadAtBudget(cfg, d, budgetChars)
}

// digestMCPPayloadAtBudget is the strict-budget implementation. Unlike the
// public/shared wrapper above, a zero budget here means zero item bytes rather
// than "use the default." The envelope path needs that distinction after its
// prompt reservation legitimately reduces a tiny request's item share to zero.
func digestMCPPayloadAtBudget(cfg Config, d Digest, budgetChars int) map[string]any {
	if budgetChars < 0 {
		budgetChars = 0
	}
	states := buildSourceStates(cfg, d)

	// The fixed (always-included) frame: everything except the item bodies. We
	// budget only the sections' items against the REMAINING space, so source_states
	// and the three-state surface are never budgeted away (they're the SC#3 signal).
	base := map[string]any{
		"generated":     d.Generated,
		"since_hours":   d.SinceHours,
		"urgent":        d.Urgent,     // issue #62 defect 2: the protected cross-source shelf.
		"urgent_more":   d.UrgentMore, // count of urgent items beyond the shelf cap.
		"source_states": states,
		"freshness":     d.Freshness,
		"stale_tasks":   d.StaleTasks,
		// source_health (HEALTH-02): the typed freshness snapshot MCP consumers read
		// structurally instead of parsing the Markdown banner. Always included in the
		// fixed frame — never budgeted away — mirroring source_states above.
		"source_health": d.SourceHealth,
		// health (C1/C4, Open Q1): the BOUNDED envelope — source_health above is the
		// rich per-connector array (kept as a documented deprecated-shape sibling for
		// one release); health.state/.index is what it never had — the aggregate
		// worst-of-3 state and the index arm, which a stale-but-present source_health
		// array cannot distinguish from a dirty/failed index.
		"health": compactHealthFrom(healthFromParts(d.SourceHealth, d.idxHealth, d.producerHealth)),
	}
	frameBytes := jsonLen(base) + jsonLen([]DigestSection{}) // + an empty sections array key
	remaining := budgetChars - frameBytes

	preBudgetCount := briefSurfacedItemCount(d)
	budgeted := budgetSections(d.Sections, remaining)
	base["sections"] = budgeted

	postBudgetCount := len(d.Urgent)
	for _, s := range budgeted {
		postBudgetCount += len(s.Items)
	}

	emptyExplanation := d.EmptyExplanation
	if emptyExplanation == "" && preBudgetCount > 0 && postBudgetCount == 0 {
		emptyExplanation = deriveEmptyExplanation(d, briefOpts{}, true)
	}
	if emptyExplanation != "" {
		base["empty_explanation"] = emptyExplanation
	}

	return base
}

// budgetSections fills a byte budget with section items in two passes, mirroring
// the Markdown budgeter (budgetDigestForMarkdown): PASS 1 gives every section up
// to budgetSourceFloor items in rank order, so a noisy high-rank source (calendar
// subscriptions) can NOT starve a lower-rank one (Emails, iMessage) below its
// floor; PASS 2 then fills the remaining budget greedily highest-rank-first. A
// section that keeps zero items becomes a TRUNCATED SHELL (its State + a MoreCount
// of all its items, empty Items) rather than being silently dropped — so the agent
// distinguishes "suppressed for budget" from "had nothing" (its true state + count
// also ride in source_states). The result is deterministic and byte-stable: input
// order is preserved and each section's cut is a pure prefix of its items.
//
// The single-pass greedy version this replaced latched an `exhausted` flag the
// moment ANY section overflowed, collapsing every lower-rank section to an empty
// shell. Because sections are rank-sorted calendar→imessage→gmail, a calendar
// flood guaranteed the MCP brief returned zero emails and zero texts (issue #62
// defect 3 was fixed in the Markdown budgeter only; this ports the fix here too).
//
// jsonSep accounts for the array/struct glue (commas, brackets, the "items" key)
// that a per-element json.Marshal length omits, so the running total is a slight
// OVER-estimate — safe against the ceiling rather than under it.
func budgetSections(sections []DigestSection, budget int) []DigestSection {
	if budget < 0 {
		budget = 0
	}
	const jsonSep = 2 // per-element comma + brace/bracket glue (conservative over-count).
	n := len(sections)
	kept := make([]int, n)    // items kept per section
	opened := make([]bool, n) // whether this section's shell cost is already paid
	used := 0

	// add tries to keep item j of section i, paying the section's shell cost once
	// (the first time an item from it is kept). Returns false if it doesn't fit.
	add := func(i, j int) bool {
		s := sections[i]
		extra := 0
		if !opened[i] {
			extra = jsonLen(DigestSection{Source: s.Source, State: s.State}) + jsonSep
		}
		itCost := jsonLen(s.Items[j]) + jsonSep
		if used+extra+itCost > budget {
			return false
		}
		if !opened[i] {
			opened[i] = true
			used += extra
		}
		used += itCost
		kept[i]++
		return true
	}

	// Pass 1 (fair floor): up to budgetSourceFloor items each, rank order.
	for i := range sections {
		for j := 0; j < len(sections[i].Items) && j < budgetSourceFloor; j++ {
			if !add(i, j) {
				break
			}
		}
	}
	// Pass 2 (greedy): fill the rest, highest-rank-first, until the budget is spent.
	for i := range sections {
		for j := kept[i]; j < len(sections[i].Items); j++ {
			if !add(i, j) {
				break
			}
		}
	}

	out := make([]DigestSection, 0, n)
	for i, s := range sections {
		switch {
		case len(s.Items) == 0:
			ns := s
			if ns.Items == nil {
				ns.Items = []DigestItem{}
			}
			out = append(out, ns) // empty section: state only, keep original MoreCount.
		case kept[i] == 0:
			out = append(out, truncatedShell(s)) // suppressed for budget.
		default:
			ns := DigestSection{Source: s.Source, State: s.State, MoreCount: s.MoreCount, Truncated: s.Truncated, ElidedByBudget: s.ElidedByBudget}
			ns.Items = append([]DigestItem{}, s.Items[:kept[i]]...)
			if dropped := len(s.Items) - kept[i]; dropped > 0 {
				ns.MoreCount += dropped
				ns.Truncated = true
				ns.ElidedByBudget += dropped
			}
			out = append(out, ns)
		}
	}
	return out
}

// truncatedShell collapses a section to its State + a MoreCount covering ALL its
// items (no bodies), marking it Truncated — the honest "suppressed for budget"
// representation that keeps the agent aware of what it isn't seeing.
func truncatedShell(s DigestSection) DigestSection {
	return DigestSection{
		Source:         s.Source,
		State:          s.State,
		Items:          []DigestItem{},
		MoreCount:      s.MoreCount + len(s.Items),
		Truncated:      true,
		ElidedByBudget: s.ElidedByBudget + len(s.Items),
	}
}

// jsonLen returns the marshaled byte length of v (the budget unit), or 0 on a
// marshal error (a degenerate value contributes nothing rather than panicking).
func jsonLen(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}
