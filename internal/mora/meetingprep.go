package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	meetingPrepEvidenceCap  = 24
	meetingPrepMaxAttendees = 25
)

// meetingPrepMCPPayload applies the MCP budget discipline (like digestMCPPayload):
// cap attendees, greedily byte-budget the evidence against budgetChars reserving
// ~1/3 for the synthesis prompt, and REBUILD the prompt so it cites only surviving
// evidence (no dangling citations). Returns the trimmed result for the MCP envelope.
func meetingPrepMCPPayload(mp MeetingPrepResult, budgetChars int) MeetingPrepResult {
	if mp.Event == nil {
		return mp
	}
	if len(mp.Attendees) > meetingPrepMaxAttendees {
		mp.Attendees = mp.Attendees[:meetingPrepMaxAttendees]
	}
	reserve := budgetChars / 3
	used := 0
	kept := make([]ThinkEvidence, 0, len(mp.Evidence))
	for _, e := range mp.Evidence {
		sz := len(e.StableID) + len(e.Title) + len(e.Scope) + len(e.CreatedAt) + len(e.Snippet) + 16
		if len(kept) > 0 && used+sz > budgetChars-reserve {
			break
		}
		used += sz
		kept = append(kept, e)
	}
	mp.Evidence = kept
	mp.SynthesisPrompt = meetingPrepPrompt(*mp.Event, mp.Attendees, mp.Evidence, mp.Gaps)
	return mp
}

// prepClock is the meeting-prep wall clock — a var so --at and tests can pin it
// (mirrors briefClock). The logic site never calls time.Now() directly.
var prepClock = time.Now

// cmdPrep is the `mora prep [name]` CLI: a cited prep pack for the next (or
// in-progress) calendar event, optionally with one attendee by name.
func cmdPrep(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("prep", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit the typed MeetingPrepResult as JSON")
	limit := fs.Int("limit", mcpSearchDefaultLimit, "max evidence memories per attendee")
	at := fs.String("at", "", "override 'now' (RFC3339) for deterministic prep/tests")
	fs.Bool("next-meeting", false, "prep the next meeting regardless of attendee (the default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	now := prepClock()
	if *at != "" {
		t, perr := time.Parse(time.RFC3339, *at)
		if perr != nil {
			return fmt.Errorf("invalid --at %q (want RFC3339): %w", *at, perr)
		}
		now = t
	}
	name := strings.TrimSpace(strings.Join(fs.Args(), " "))
	var filter map[string]bool
	if name != "" {
		idSet, rerr := resolveEntityFilter(ctx, cfg, name)
		if rerr != nil {
			return rerr
		}
		filter = idSet
	}
	mp, err := buildMeetingPrep(ctx, cfg, now, name, filter, *limit)
	if err != nil {
		return humanizeIndexBusy(err)
	}
	logUsage(cfg, usageEvent{Tool: "prep", Results: len(mp.Evidence)})
	if *jsonOut {
		return emit(stdout, mp, true)
	}
	printMeetingPrep(stdout, mp)
	return nil
}

// gapLines flattens the gap analysis into honest one-liners (human + prompt order).
func (g MeetingGaps) lines() []string {
	var out []string
	out = append(out, g.UnknownAttendees...)
	out = append(out, g.ThinAttendees...)
	out = append(out, g.NoEvidence...)
	if g.NoAttendees {
		out = append(out, "This event lists no other attendees — nothing to prep against.")
	}
	if g.SelfUnknown {
		out = append(out, "Self identity is unknown (no Google account connected), so the attendee list may include you.")
	}
	return out
}

func printMeetingPrep(w io.Writer, mp MeetingPrepResult) {
	if mp.Event == nil {
		fmt.Fprintln(w, "No upcoming meeting found.")
		for _, s := range mp.Gaps.lines() {
			fmt.Fprintf(w, "  - %s\n", s)
		}
		return
	}
	if mp.FallbackNote != "" {
		fmt.Fprintln(w, mp.FallbackNote)
	}
	fmt.Fprintf(w, "Next meeting: %s — %s (%s)\n", mp.Event.Title, mp.Event.OccurredAt, mp.Event.Source)
	names := make([]string, 0, len(mp.Attendees))
	for _, a := range mp.Attendees {
		names = append(names, a.Display)
	}
	if len(names) > 0 {
		fmt.Fprintf(w, "Attendees: %s\n", strings.Join(names, ", "))
	}
	if len(mp.Evidence) > 0 {
		fmt.Fprintln(w, "\nRecent context (cited):")
		for _, e := range mp.Evidence {
			fmt.Fprintf(w, "  [%s] (%s, %s) %s — %s\n", e.StableID, e.Scope, e.CreatedAt, e.Title, e.Snippet)
		}
	}
	if !mp.Gaps.empty() {
		fmt.Fprintln(w, "\nWhat the vault does NOT know:")
		for _, s := range mp.Gaps.lines() {
			fmt.Fprintf(w, "  - %s\n", s)
		}
	}
	fmt.Fprintln(w, "\n— To compose a grounded brief, run this prompt with your model: —")
	fmt.Fprintln(w, mp.SynthesisPrompt)
}

const meetingPrepNoEventPrompt = "No upcoming calendar event found in the next 14 days. Tell the user there's nothing on the calendar to prep for, and (if asked) suggest checking that the calendar connector is synced."

// PrepAttendee is one non-self attendee of the selected meeting.
type PrepAttendee struct {
	PersonID      string `json:"person_id"`
	Identity      string `json:"identity"`
	Display       string `json:"display"`
	Known         bool   `json:"known"`
	MentionCount  int    `json:"mention_count"`
	EvidenceCount int    `json:"evidence_count"`
}

// MeetingGaps is the deterministic "what the vault does NOT know" analysis.
type MeetingGaps struct {
	UnknownAttendees []string `json:"unknown_attendees,omitempty"`
	ThinAttendees    []string `json:"thin_attendees,omitempty"`
	NoEvidence       []string `json:"no_evidence,omitempty"`
	NoAttendees      bool     `json:"no_attendees,omitempty"`
	SelfUnknown      bool     `json:"self_unknown,omitempty"`
}

func (g MeetingGaps) empty() bool {
	return len(g.UnknownAttendees) == 0 && len(g.ThinAttendees) == 0 && len(g.NoEvidence) == 0 && !g.NoAttendees && !g.SelfUnknown
}

// MeetingPrepResult is the cited prep pack: the event, self-excluded attendees,
// recency-ranked evidence, an honest gap analysis, and a model-free synthesis
// prompt. Event is nil when there is no upcoming meeting.
type MeetingPrepResult struct {
	Event           *MeetingEvent   `json:"event"`
	Attendees       []PrepAttendee  `json:"attendees"`
	Evidence        []ThinkEvidence `json:"evidence"`
	Gaps            MeetingGaps     `json:"gaps"`
	FallbackNote    string          `json:"note,omitempty"` // UX FORK 2 forgiving fallback
	SynthesisPrompt string          `json:"synthesis_prompt"`
}

// selfEmails returns lowercased Source.Email over enabled gmail/calendar sources —
// the user's own addresses, used to exclude self from the attendee list. Empty when
// no Google account is connected (iMessage-only vault): self-exclusion becomes a
// no-op and Gaps.SelfUnknown is set, rather than guessing self from a name/handle.
func selfEmails(cfg Config) map[string]bool {
	out := map[string]bool{}
	for _, s := range loadSourcesOrEmpty(cfg) {
		if (s.Type == "gmail" || s.Type == "calendar") && s.Email != "" {
			out[strings.ToLower(s.Email)] = true
		}
	}
	return out
}

// entInfo is a canonical entity row keyed by every one of its alias ids.
type entInfo struct {
	canonical, display string
	mention            int
}

// buildAliasIndex maps EVERY alias id (canonical ∪ address/handle aliases) to its
// canonical entity, in one scan — so a raw personRefs id (pre-merge) resolves to the
// canonical row the edges table is keyed by (P1-A).
func buildAliasIndex(ctx context.Context, db *sql.DB) (map[string]entInfo, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, display_name, aliases, mention_count FROM entities WHERE id NOT LIKE 'memory:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx := map[string]entInfo{}
	for rows.Next() {
		var id, display, aliasesJSON string
		var mention int
		if err := rows.Scan(&id, &display, &aliasesJSON, &mention); err != nil {
			return nil, err
		}
		var aliases []string
		if aliasesJSON != "" {
			_ = json.Unmarshal([]byte(aliasesJSON), &aliases)
		}
		info := entInfo{canonical: id, display: display, mention: mention}
		for member := range aliasIDSet(id, aliases) {
			idx[member] = info
		}
	}
	return idx, rows.Err()
}

// evidenceIDsFor returns the live evidence memory ids for a canonical person id —
// the edges keyed by the resolved id, NOT a name round-trip through graphGetEntity
// (which re-resolves by display name and risks a homograph leak — P1-A §7a).
func evidenceIDsFor(ctx context.Context, db *sql.DB, canonical string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT evidence_id FROM edges WHERE dst = ? AND invalidated_at IS NULL`, canonical)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// buildMeetingPrep assembles the cited prep pack for the next (or in-progress)
// calendar event, optionally restricted to a named attendee. now is injected for
// determinism. Makes NO model call; never invents decisions or open questions.
func buildMeetingPrep(ctx context.Context, cfg Config, now time.Time, attendeeName string, attendeeFilterIDs map[string]bool, limitPerAttendee int) (MeetingPrepResult, error) {
	if limitPerAttendee <= 0 {
		limitPerAttendee = mcpSearchDefaultLimit
	}
	var res MeetingPrepResult

	files, err := allMemoryFiles(cfg)
	if err != nil {
		return res, err
	}
	var mems []Memory
	for _, p := range files {
		m, err := parseMemory(p)
		if err != nil || m.DeletedAt != "" {
			continue
		}
		mems = append(mems, m)
	}

	// Select the event. UX FORK 2 (forgiving): when a named attendee has no upcoming
	// meeting, fall back to the next meeting regardless of attendee, with an honest note.
	ev := selectNextEvent(mems, now, attendeeFilterIDs)
	if ev == nil && len(attendeeFilterIDs) > 0 {
		if fb := selectNextEvent(mems, now, nil); fb != nil {
			ev = fb
			who := attendeeName
			if who == "" {
				who = "that person"
			}
			res.FallbackNote = fmt.Sprintf("No upcoming meeting with %s — showing your next meeting instead.", who)
		}
	}
	if ev == nil {
		res.SynthesisPrompt = meetingPrepNoEventPrompt
		return res, nil
	}
	res.Event = ev

	var eventMemory Memory
	for _, m := range mems {
		if m.ID == ev.StableID {
			eventMemory = m
			break
		}
	}

	db, err := ensureIndexDB(ctx, cfg)
	if err != nil {
		return res, err
	}
	defer db.Close()
	aliasIdx, err := buildAliasIndex(ctx, db)
	if err != nil {
		return res, err
	}

	self := selfEmails(cfg)
	if len(self) == 0 {
		res.Gaps.SelfUnknown = true
	}

	// Event itself is evidence #0 (so the agent has the meeting body/notes); seed
	// dedup with its id so attendee retrieval can't re-add it.
	seen := map[string]bool{ev.StableID: true}
	res.Evidence = append(res.Evidence, ThinkEvidence{
		StableID: ev.StableID, Title: ev.Title, Scope: eventMemory.Scope,
		CreatedAt: ev.OccurredAt, Snippet: snippet(eventMemory.Text, thinkSnippetLen),
	})

	// Evidence quality: the selected meeting's own recurring siblings are not
	// "context" for it, and future calendar entries aren't "recent". seriesByID is
	// built from the full-Meta mems (the index doesn't store Meta, so edge-loaded
	// evidence memories can't report their own recurring_event_id).
	selectedSeries := recurringSeriesID(eventMemory)
	seriesByID := map[string]string{}
	for _, m := range mems {
		if sid := recurringSeriesID(m); sid != "" {
			seriesByID[m.ID] = sid
		}
	}

	parts, _, _, _ := personRefs(eventMemory)
	for _, p := range parts {
		if self[p.identity] {
			continue // self-exclusion (§6d)
		}
		display := p.name
		if display == "" {
			display = p.identity
		}
		att := PrepAttendee{PersonID: p.id, Identity: p.identity, Display: display}
		info, known := aliasIdx[p.id]
		if !known {
			res.Gaps.UnknownAttendees = append(res.Gaps.UnknownAttendees, fmt.Sprintf("The vault has no record of %s.", display))
			res.Attendees = append(res.Attendees, att)
			continue
		}
		att.Known = true
		att.MentionCount = info.mention
		if info.display != "" {
			display = info.display
			att.Display = info.display
		}
		evIDs, err := evidenceIDsFor(ctx, db, info.canonical)
		if err != nil {
			return res, err
		}
		ems, err := loadMemoriesByID(ctx, db, evIDs)
		if err != nil {
			return res, err
		}
		ems = snippetMemories(ems, display)
		sort.Slice(ems, func(i, j int) bool {
			ti, _ := time.Parse(time.RFC3339, ems[i].CreatedAt)
			tj, _ := time.Parse(time.RFC3339, ems[j].CreatedAt)
			if !ti.Equal(tj) {
				return ti.After(tj) // recency desc
			}
			return ems[i].ID < ems[j].ID
		})
		n := 0
		for _, m := range ems {
			if n >= limitPerAttendee || len(res.Evidence) >= meetingPrepEvidenceCap {
				break
			}
			if seen[m.ID] {
				continue
			}
			if ts, perr := time.Parse(time.RFC3339, m.CreatedAt); perr == nil && ts.After(now) {
				continue // future-dated calendar entries aren't "recent context"
			}
			if selectedSeries != "" && seriesByID[m.ID] == selectedSeries {
				continue // the selected meeting's own recurring series — not context for itself
			}
			seen[m.ID] = true
			res.Evidence = append(res.Evidence, ThinkEvidence{
				StableID: m.ID, Title: m.Title, Scope: m.Scope, CreatedAt: m.CreatedAt, Snippet: m.Text,
			})
			att.EvidenceCount++
			n++
		}
		if info.mention < thinkThinK {
			res.Gaps.ThinAttendees = append(res.Gaps.ThinAttendees, fmt.Sprintf("Only %d memory about %s — coverage is thin.", info.mention, display))
		}
		if att.EvidenceCount == 0 {
			res.Gaps.NoEvidence = append(res.Gaps.NoEvidence, fmt.Sprintf("No recent context with %s.", display))
		}
		res.Attendees = append(res.Attendees, att)
	}
	if len(res.Attendees) == 0 {
		res.Gaps.NoAttendees = true
	}

	res.SynthesisPrompt = meetingPrepPrompt(*ev, res.Attendees, res.Evidence, res.Gaps)
	return res, nil
}

// meetingPrepPrompt mirrors thinkPrompt's envelope with a meeting frame and an
// anti-fabrication clause. Pure string builder — no model call, no network.
func meetingPrepPrompt(ev MeetingEvent, attendees []PrepAttendee, evidence []ThinkEvidence, gaps MeetingGaps) string {
	var b strings.Builder
	b.WriteString("You are preparing the user for an upcoming meeting. Using ONLY the evidence below, write a short prep brief: who the attendees are, the most recent relevant context with each, and anything time-sensitive. Cite every claim with its [stable_id]. Do NOT invent decisions, action items, or open questions that are not in the evidence — if the vault is thin, say so plainly using the KNOWN GAPS below.\n\n")
	fmt.Fprintf(&b, "MEETING: %s — %s (%s)\n", ev.Title, ev.OccurredAt, ev.Source)
	names := make([]string, 0, len(attendees))
	for _, a := range attendees {
		names = append(names, a.Display)
	}
	fmt.Fprintf(&b, "ATTENDEES (self excluded): %s\n\nEVIDENCE:\n", strings.Join(names, ", "))
	for _, e := range evidence {
		fmt.Fprintf(&b, "- [%s] (%s, %s) %s — %s\n", e.StableID, e.Scope, e.CreatedAt, e.Title, e.Snippet)
	}
	if !gaps.empty() {
		b.WriteString("\nKNOWN GAPS (surface these honestly in a 'What the vault does not know' section):\n")
		for _, s := range gaps.UnknownAttendees {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.ThinAttendees {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, s := range gaps.NoEvidence {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		if gaps.NoAttendees {
			b.WriteString("- This event lists no other attendees — nothing to prep against.\n")
		}
		if gaps.SelfUnknown {
			b.WriteString("- Self identity is unknown (no Google account connected), so the attendee list may include you.\n")
		}
	}
	return b.String()
}

// meetingprep.go — the meeting-prep pack: pick the user's next (or in-progress)
// calendar event, assemble cited evidence about each attendee, and emit a
// model-free synthesis prompt with an honest gap analysis. Mirrors buildThink's
// shape; makes NO model call and never invents decisions or open questions.

const meetingPrepHorizonDays = 14

// meetingPrepGrace is how long after an event's start it still counts as "current"
// (the meeting you just walked into). A var so tests can pin it; without a persisted
// end time, true end>now in-progress detection is deferred to a connector change
// (P1-F). 30 minutes covers the common "running a few minutes late" case.
var meetingPrepGrace = 30 * time.Minute

// MeetingEvent is the selected calendar event the prep pack is built around.
type MeetingEvent struct {
	StableID   string   `json:"stable_id"`
	Title      string   `json:"title"`
	OccurredAt string   `json:"occurred_at"`
	Source     string   `json:"source"`
	AllDay     bool     `json:"all_day"`
	Attendees  []string `json:"attendees"`
}

// eventStart parses an event memory's start instant: Meta["occurred_at"] (RFC3339,
// written by both the google and applecal connectors), falling back to CreatedAt.
// ok is false if nothing parses.
func eventStart(m Memory) (time.Time, bool) {
	candidates := []string{}
	if m.Meta != nil {
		if s, _ := m.Meta["occurred_at"].(string); s != "" {
			candidates = append(candidates, s)
		}
	}
	candidates = append(candidates, m.CreatedAt)
	for _, s := range candidates {
		if s == "" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

// dayStartUTC truncates an instant to the start of its UTC calendar day.
func dayStartUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// selectNextEvent picks the meeting to prep for: the in-progress event (started
// within the grace window) if any, else the earliest upcoming one, bounded to a
// 14-day horizon, optionally restricted to events whose attendees intersect
// attendeeFilterIDs (the resolved alias-id SET — P1-A). Pure over parsed memories +
// injected now; deterministic (StableID tie-break). Returns nil if none qualifies.
//
// All-day events are detected uniformly as a midnight-UTC start (both connectors —
// P1-B) and compared at calendar-day granularity to dodge the midnight boundary
// flake. Timed events use the grace-extended lower bound start >= now-grace (P1-F);
// true end>now detection awaits a persisted end time (deferred connector change).
func selectNextEvent(mems []Memory, now time.Time, attendeeFilterIDs map[string]bool) *MeetingEvent {
	type cand struct {
		m       Memory
		start   time.Time
		allDay  bool
		current bool // started/today (vs strictly future)
	}
	today := dayStartUTC(now)
	horizonDay := today.AddDate(0, 0, meetingPrepHorizonDays)
	graceFloor := now.Add(-meetingPrepGrace)

	var cands []cand
	for _, m := range mems {
		if m.DeletedAt != "" || m.Type != "event" {
			continue
		}
		start, ok := eventStart(m)
		if !ok {
			continue
		}
		allDay := start.Equal(dayStartUTC(start))
		var inWindow, current bool
		if allDay {
			day := dayStartUTC(start)
			inWindow = !day.Before(today) && !day.After(horizonDay)
			current = day.Equal(today)
		} else {
			inWindow = !start.Before(graceFloor) && !start.After(now.AddDate(0, 0, meetingPrepHorizonDays))
			current = !start.After(now)
		}
		if !inWindow {
			continue
		}
		if len(attendeeFilterIDs) > 0 && !memoryMentionsEntity(m, attendeeFilterIDs) {
			continue
		}
		cands = append(cands, cand{m: m, start: start, allDay: allDay, current: current})
	}
	if len(cands) == 0 {
		return nil
	}

	// Current-first: a started-within-grace event beats a future one. Among current
	// events pick the LATEST start (closest to now — the one you're in); among future
	// events pick the EARLIEST start. StableID (memory id) breaks ties deterministically.
	var current, future []cand
	for _, c := range cands {
		if c.current {
			current = append(current, c)
		} else {
			future = append(future, c)
		}
	}
	var pick cand
	if len(current) > 0 {
		sort.Slice(current, func(i, j int) bool {
			if !current[i].start.Equal(current[j].start) {
				return current[i].start.After(current[j].start) // latest first
			}
			return current[i].m.ID < current[j].m.ID
		})
		pick = current[0]
	} else {
		sort.Slice(future, func(i, j int) bool {
			if !future[i].start.Equal(future[j].start) {
				return future[i].start.Before(future[j].start) // earliest first
			}
			return future[i].m.ID < future[j].m.ID
		})
		pick = future[0]
	}

	return &MeetingEvent{
		StableID:   pick.m.ID,
		Title:      pick.m.Title,
		OccurredAt: pick.start.UTC().Format(time.RFC3339),
		Source:     pick.m.Provider,
		AllDay:     pick.allDay,
		Attendees:  metaStrings(pick.m.Meta["attendees"]),
	}
}
