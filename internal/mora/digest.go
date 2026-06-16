package mora

import (
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

// DigestItem is one cited entry — title + snippet + the memory id so the agent
// (or the user) can pull the full memory with read_memory. Change is the typed
// Delta seam (M-5): "new" | "updated" in DELTA mode, "" in the plain-window path.
// renderDigest AND the MCP projection (Plan 05) both read this one struct.
type DigestItem struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
	Snippet   string `json:"snippet"`
	Change    string `json:"change,omitempty"` // new | updated (M-5)
	// LowSignal flags a SERVICE-ONLY item — every participant is an automated/service
	// identity (receipts, newsletters, no-reply notices), per memoryIsServiceOnly. It
	// drives the noise-collapse in section assembly so a window of pure-service items
	// doesn't crowd out signal. It is internal (json:"-") so it never changes the MCP
	// payload shape or budget.
	LowSignal bool `json:"-"`
}

// DigestSection groups one instance's surfaced items, recency-ordered. State is
// the three-state label (D-03); MoreCount/Truncated carry the silent-data-loss
// guard's "+N more since last brief" when the per-instance delta exceeds the cap.
// These are the typed Delta seam (M-5) read by renderDigest now and the MCP
// projection in Plan 05 — one source of truth, not a render string plus a map.
type DigestSection struct {
	Source    string       `json:"source"`
	State     string       `json:"state"`
	Items     []DigestItem `json:"items"`
	MoreCount int          `json:"more_count,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
}

// Digest is the assembled brief. Generated is injected by the caller (time.Now)
// for deterministic tests; it (and the cold-start cutoff) is canonicalized to UTC
// for byte-stability without disturbing the DST-safe window math.
type Digest struct {
	Generated  string            `json:"generated"`
	SinceHours int               `json:"since_hours"`
	Sections   []DigestSection   `json:"sections"`
	Freshness  map[string]string `json:"freshness,omitempty"`
	StaleTasks []string          `json:"stale_tasks,omitempty"`
}

// briefOpts is the buildDigest options seam. advance gates the watermark commit
// (default-off on every surface — D-02; only the scheduled --advance caller sets
// it, wired in Plan 05). sinceHours>0 selects the plain-window path (SC#2) which
// never advances. perSourceCap caps each section (0 => default).
type briefOpts struct {
	advance      bool
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
	perSourceCap := opts.perSourceCap
	if perSourceCap <= 0 {
		perSourceCap = digestDefaultCap
	}

	files, err := allMemoryFiles(cfg)
	if err != nil {
		return Digest{}, err
	}

	// Parse once, group by INSTANCE key (M-1) — NOT m.Source (per-item ProviderID).
	// Skip tombstones up front (M-4) so a cancelled calendar event (new
	// content_hash) is never live [updated] nor in the cold-start 7d window; it is
	// also excluded from the baseline and dropped from the snapshot on commit.
	byInstance := map[string][]Memory{}
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil {
			continue
		}
		if m.DeletedAt != "" {
			continue // M-4: tombstone — mirror graph.go's skip.
		}
		key, ok := sourceInstanceKey(m)
		if !ok {
			continue // empty-Provider (filesystem) — never a watermark instance (M-1).
		}
		byInstance[key] = append(byInstance[key], m)
	}

	// Per-item salience map (SC#3): computed ONCE over the FULL parsed set so a
	// person's volume spans the whole vault (matching buildGraph's whole-vault
	// scoring), then threaded down into both paths. buildDigest stays file-based —
	// this reuses the already-parsed memories and opens no index DB.
	memSal := digestMemorySalience(flattenInstances(byInstance))

	// Apply the preview-only entity/scope/since-days filters AFTER memSal (P1-C): the
	// salience map must stay whole-vault (it is capRecency's primary sort key), but
	// the surfaced set narrows to matching memories. Identity when no filter is set.
	byInstance = filterByInstance(byInstance, opts, now)

	if opts.sinceHours > 0 {
		return buildWindowDigest(cfg, now, opts.sinceHours, perSourceCap, byInstance, memSal, opts.source)
	}
	return buildDeltaDigest(cfg, now, opts, perSourceCap, byInstance, memSal)
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
		if classifyIdentity(strings.TrimPrefix(id, "person:"), "") != "service" {
			return false
		}
	}
	return true
}

// buildWindowDigest is the plain ad-hoc window path (SC#2): created_at within the
// last sinceHours, no delta, no watermark, no State sentinels. It mirrors the
// legacy behavior but groups by instance key so the human labels fire.
func buildWindowDigest(cfg Config, now time.Time, sinceHours, perSourceCap int, byInstance map[string][]Memory, memSal map[string]int64, sourceFilter string) (Digest, error) {
	window := time.Duration(sinceHours) * time.Hour
	cutoff := now.Add(-window)
	forward := now.Add(window)
	var sections []DigestSection
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
			it := digestItemFor(cfg, m, key, "")
			it.LowSignal = memoryIsServiceOnly(m)
			tis = append(tis, tsItem{item: it, ts: ts, sal: memSal[m.ID], series: recurringSeriesID(m)})
		}
		if len(tis) == 0 {
			continue
		}
		tis = collapseRecurringSeries(tis, now)
		items, more := capRecency(tis, perSourceCap, upcoming)
		items, collapsed := collapseLowSignal(items)
		sections = append(sections, DigestSection{Source: key, Items: items, MoreCount: more + collapsed, Truncated: more > 0})
	}
	sortSections(sections)
	stale, _ := staleTasks(cfg, 3)
	return Digest{
		Generated:  now.UTC().Format(time.RFC3339),
		SinceHours: sinceHours,
		Sections:   sections,
		Freshness:  sourceFreshness(cfg),
		StaleTasks: stale,
	}, nil
}

// buildDeltaDigest is the delta + three-state path (SC#1, SC#3, SC#4). It is the
// behavioral heart of Phase 12.
func buildDeltaDigest(cfg Config, now time.Time, opts briefOpts, perSourceCap int, byInstance map[string][]Memory, memSal map[string]int64) (Digest, error) {
	// Preview-only guard, HOISTED above the lock and the per-filter handling (§5): a
	// filtered advance (source/entity/scope/since-days) would commit the watermark over
	// items the reader never saw (they were filtered out). Catching it here — before
	// acquireBriefLock — means an entity/scope/since-days advance can't slip past the
	// old source-only nested check.
	if opts.advance && opts.filtered() {
		return Digest{}, fmt.Errorf("a filtered digest (--source/--entity/--scope/--since-days) is preview-only and cannot --advance the watermark")
	}

	// The enumeration set for the three-state labels is the ENABLED+INGESTING
	// connectors (M-2) — not providers-found-in-memories (which would hide a
	// broken/all-deleted source) and not the sync/ dir. A connector enumerated
	// here but absent from byInstance still emits a section with its State, so a
	// zero-memory/all-deleted source surfaces "unavailable" rather than vanishing
	// (the SC#3 gap).
	enumerated, err := ingestingConnectors(cfg)
	if err != nil {
		return Digest{}, err
	}

	// Commit (opts.advance): hold the brief/ lock across the whole load→classify→
	// write so a hand-run --advance racing the cron no-ops/blocks rather than
	// interleaving (T-12-07). Preview never reaches here.
	if opts.advance {
		release, lerr := acquireBriefLock(cfg)
		if lerr != nil {
			return Digest{}, fmt.Errorf("brief commit in progress (another --advance run holds the lock): %w", lerr)
		}
		defer release()
	}

	// The set of instance keys to BUILD sections for IS exactly the enumerated
	// (enabled+ingesting) connectors (M-2): a connector enumerated but absent from
	// memories still emits a section (zero-memory → "unavailable"), while a provider
	// found in memories but NOT enabled+ingesting is NOT forced in. `enumerated` is
	// already sorted; copy so the iteration order is deterministic.
	keys := append([]string(nil), enumerated...)
	sort.Strings(keys)
	if opts.source != "" {
		// advance+filter already rejected by the hoisted guard above; here we only
		// narrow the section keys to the source filter.
		filtered := keys[:0]
		for _, k := range keys {
			if digestSourceMatches(k, opts.source) {
				filtered = append(filtered, k)
			}
		}
		keys = filtered
	}

	var sections []DigestSection
	for _, key := range keys {
		mems := byInstance[key] // may be nil for a zero-memory enumerated connector
		snap := loadBriefSnapshot(cfg, key)
		delta := classify(snap, mems, now)

		// Build the surfaced section items + the set of ids actually RENDERED (the
		// silent-data-loss guard advances the watermark only over these).
		items, shownIDs, moreCount := deltaSectionItems(cfg, delta, mems, now, key, perSourceCap, memSal)

		state := classifyState(cfg, key, now, delta, len(items) > 0)
		sec := DigestSection{Source: key, State: state, Items: items}
		if moreCount > 0 {
			sec.MoreCount = moreCount
			sec.Truncated = true
		}
		sections = append(sections, sec)

		// Commit: persist the advanced snapshot for THIS instance (under the lock).
		if opts.advance {
			next := nextSnapshot(snap, delta, shownIDs, key)
			if serr := saveBriefSnapshot(cfg, next, now); serr != nil {
				return Digest{}, fmt.Errorf("commit watermark for %q: %w", key, serr)
			}
		}
	}
	sortSections(sections)

	// StaleTasks come from vault/live-tasks.md and are sync-independent — they are
	// NOT gated by the watermark (D-03 note).
	stale, _ := staleTasks(cfg, 3)
	return Digest{
		Generated:  now.UTC().Format(time.RFC3339),
		SinceHours: 0,
		Sections:   sections,
		Freshness:  sourceFreshness(cfg),
		StaleTasks: stale,
	}, nil
}

// deltaSectionItems turns a classify result into the section's rendered items.
//
//   - cold start: surface the COURTESY window (last 7d by created_at; calendar =
//     upcoming 7d) from the instance's memories (D-04). The baseline-all behavior
//     lives in classify; here we only choose what to DISPLAY.
//   - steady state: surface delta.Items (new/updated), recency-ordered, capped.
//
// It returns the items to render, the set of stableIDs actually rendered (the
// guard advances only over these), and the count truncated past the cap.
func deltaSectionItems(cfg Config, delta briefDelta, mems []Memory, now time.Time, key string, cap int, memSal map[string]int64) (items []DigestItem, shownIDs map[string]bool, moreCount int) {
	shownIDs = map[string]bool{}
	if delta.ColdStart {
		var tis []tsItem
		isCalendar := connectorUpcoming(key)
		for _, m := range mems {
			ts, err := time.Parse(time.RFC3339, m.CreatedAt)
			if err != nil {
				continue
			}
			if !inColdStartWindow(ts, now, isCalendar) {
				continue
			}
			it := digestItemFor(cfg, m, key, "new") // cold start: everything shown is "new" to the reader.
			it.LowSignal = memoryIsServiceOnly(m)
			tis = append(tis, tsItem{item: it, ts: ts, sal: memSal[m.ID], series: recurringSeriesID(m)})
		}
		tis = collapseRecurringSeries(tis, now)
		shown, more := capRecency(tis, cap, isCalendar)
		// On cold start the WHOLE baseline is committed by nextSnapshot, so we do
		// not need shownIDs to drive the commit; leave it empty (cold-start path
		// ignores it). The cap-`more` stays unsurfaced (undisplayed archive is the
		// starting line, not a truncated delta), but we DO collapse the zero-salience
		// tail so the very first brief leads with signal instead of a wall of receipts.
		_ = more
		displayed, collapsed := collapseLowSignal(shown)
		return displayed, shownIDs, collapsed
	}

	// Steady state: surface the new/updated deltas, recency-ordered.
	byID := map[string]Memory{}
	for _, m := range mems {
		byID[m.ID] = m
	}
	var tis []tsItem
	for _, di := range delta.Items {
		m, ok := byID[di.ID]
		if !ok {
			continue // surfaced id no longer on disk (deleted between classify and here) — skip.
		}
		ts, err := time.Parse(time.RFC3339, m.CreatedAt)
		if err != nil {
			ts = time.Time{} // unparsable created_at sorts last; still shown.
		}
		it := digestItemFor(cfg, m, key, di.Change)
		it.LowSignal = memoryIsServiceOnly(m)
		tis = append(tis, tsItem{item: it, ts: ts, sal: memSal[m.ID], series: recurringSeriesID(m)})
	}
	tis = collapseRecurringSeries(tis, now)
	// Map each surfaced line to the memory ids it represents (a collapsed series
	// stands for all its instances) so the watermark advances over the WHOLE set.
	memberOf := make(map[string][]string, len(tis))
	for _, ti := range tis {
		if len(ti.members) > 0 {
			memberOf[ti.item.ID] = ti.members
		} else {
			memberOf[ti.item.ID] = []string{ti.item.ID}
		}
	}
	shown, more := capRecency(tis, cap, connectorUpcoming(key))
	// Mark the FULL capped set (and every folded series instance) acknowledged
	// (watermark unchanged), THEN collapse the zero-salience tail for display —
	// collapsed items are counted, never re-surfaced.
	for _, it := range shown {
		for _, id := range memberOf[it.ID] {
			shownIDs[id] = true
		}
	}
	displayed, collapsed := collapseLowSignal(shown)
	return displayed, shownIDs, more + collapsed
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

// nextSnapshot computes the watermark to persist for one instance on commit.
//
//   - cold start: baseline ALL current hashes (delta.Baseline) — archived backfill
//     becomes the starting line, not a lost delta (D-04). Undisplayed items are
//     intentionally baselined.
//   - steady state: keep the PREVIOUS snapshot value EXACTLY for every unshown id
//     that is still present (so an unshown updated item keeps its OLD hash and
//     re-surfaces next run — never silently marked-read), UNION the current hashes
//     of items actually SHOWN (the guard), MINUS any ids no longer present (their
//     memory was deleted/tombstoned — M-4 drop, so a later same-id recreation
//     re-surfaces as new).
func nextSnapshot(prev briefSnapshot, delta briefDelta, shownIDs map[string]bool, key string) briefSnapshot {
	items := map[string]string{}
	if delta.ColdStart {
		for id, h := range delta.Baseline {
			items[id] = h
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
	// Advance the hash ONLY for items actually shown this run (the guard).
	for id := range shownIDs {
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
// google-/imessage- filename families used by the ingest paths.
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

// renderDigest renders the brief as Markdown, clipped to budgetChars (the
// time-sensitive sections lead, so truncation drops the least-important tail).
// It reads the typed Delta seam: the per-section State sentinel, the [new]/
// [updated] item Change, and the "+N more since last brief" guard line (M-5).
func renderDigest(d Digest, budgetChars int) string {
	if budgetChars <= 0 {
		budgetChars = defaultContextTokens * charsPerToken
	}
	var b strings.Builder
	if d.SinceHours > 0 {
		fmt.Fprintf(&b, "# Mora digest — %s (last %dh)\n", d.Generated, d.SinceHours)
	} else {
		fmt.Fprintf(&b, "# Mora digest — %s (since last brief)\n", d.Generated)
	}
	if len(d.Freshness) > 0 {
		keys := make([]string, 0, len(d.Freshness))
		for k := range d.Freshness {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+" "+d.Freshness[k])
		}
		fmt.Fprintf(&b, "Fresh as of: %s\n", strings.Join(parts, " · "))
	}
	for _, s := range d.Sections {
		fmt.Fprintf(&b, "\n## %s\n", sectionHeading(s))
		for _, it := range s.Items {
			fmt.Fprintf(&b, "- %s%s — %s (id: %s)\n", changePrefix(it.Change), it.Title, it.Snippet, it.ID)
		}
		if s.MoreCount > 0 {
			fmt.Fprintf(&b, "- +%d more since last brief\n", s.MoreCount)
		}
	}
	if len(d.StaleTasks) > 0 {
		fmt.Fprintf(&b, "\n## Open tasks (%d stale)\n", len(d.StaleTasks))
		for _, task := range d.StaleTasks {
			fmt.Fprintf(&b, "- %s\n", task)
		}
	}
	return truncateRunes(b.String(), budgetChars)
}

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
		return fmt.Sprintf("%s — baseline (%d)", label, len(s.Items))
	default: // stateDelta or plain-window
		return fmt.Sprintf("%s (%d)", label, len(s.Items))
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
	states := buildSourceStates(cfg, d)

	// The fixed (always-included) frame: everything except the item bodies. We
	// budget only the sections' items against the REMAINING space, so source_states
	// and the three-state surface are never budgeted away (they're the SC#3 signal).
	base := map[string]any{
		"generated":     d.Generated,
		"since_hours":   d.SinceHours,
		"source_states": states,
		"freshness":     d.Freshness,
		"stale_tasks":   d.StaleTasks,
	}
	frameBytes := jsonLen(base) + jsonLen([]DigestSection{}) // + an empty sections array key
	remaining := budgetChars - frameBytes

	budgeted := budgetSections(d.Sections, remaining)
	base["sections"] = budgeted
	return base
}

// budgetSections greedily fills a byte budget with sections highest-rank-first
// (d.Sections is already rank-sorted), item by item. A section that partially
// fits keeps the fitting items and accumulates the rest into MoreCount/Truncated.
// Once the budget is exhausted, every REMAINING section is kept as a TRUNCATED
// SHELL (its State + a MoreCount of all its items, empty Items) rather than
// silently dropped — so the agent can distinguish "this source was suppressed
// for budget" from "this source had nothing" (its true state + count also ride in
// source_states). The result is deterministic and byte-stable: input order is
// preserved and each section's cut is a pure prefix of its items.
//
// jsonSep accounts for the array/struct glue (commas, brackets, the "items" key)
// that a per-element json.Marshal length omits, so the running total is a slight
// OVER-estimate — safe against the ceiling rather than under it.
func budgetSections(sections []DigestSection, budget int) []DigestSection {
	if budget < 0 {
		budget = 0
	}
	const jsonSep = 2 // per-element comma + brace/bracket glue (conservative over-count).
	out := make([]DigestSection, 0, len(sections))
	used := 0
	exhausted := false
	for _, s := range sections {
		if exhausted {
			// Budget already spent: keep a truncated shell so the agent sees the
			// section was suppressed, not absent.
			out = append(out, truncatedShell(s))
			continue
		}
		shellCost := jsonLen(DigestSection{Source: s.Source, State: s.State}) + jsonSep
		if used+shellCost > budget {
			// No room for even this section's shell — suppress it (and the rest) as
			// truncated shells.
			out = append(out, truncatedShell(s))
			exhausted = true
			continue
		}
		kept := DigestSection{Source: s.Source, State: s.State, MoreCount: s.MoreCount, Truncated: s.Truncated}
		used += shellCost
		dropped := 0
		for idx, it := range s.Items {
			itCost := jsonLen(it) + jsonSep
			if used+itCost > budget {
				dropped = len(s.Items) - idx
				break
			}
			kept.Items = append(kept.Items, it)
			used += itCost
		}
		if dropped > 0 {
			kept.MoreCount += dropped
			kept.Truncated = true
			exhausted = true // later sections won't fit either; suppress them as shells.
		}
		out = append(out, kept)
	}
	return out
}

// truncatedShell collapses a section to its State + a MoreCount covering ALL its
// items (no bodies), marking it Truncated — the honest "suppressed for budget"
// representation that keeps the agent aware of what it isn't seeing.
func truncatedShell(s DigestSection) DigestSection {
	return DigestSection{
		Source:    s.Source,
		State:     s.State,
		MoreCount: s.MoreCount + len(s.Items),
		Truncated: true,
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
