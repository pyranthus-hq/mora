package mora

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	meetingBriefOpenLoops       = "open_loops"
	meetingBriefUnresolved      = "unresolved_threads"
	meetingBriefStaleness       = "staleness_guards"
	meetingBriefSharedContext   = "material_shared_context"
	meetingBriefDefaultPerGuest = 3
)

var meetingBriefSectionTitles = map[string]string{
	meetingBriefOpenLoops:     "Your open loops and obligations",
	meetingBriefUnresolved:    "Unresolved decisions and threads",
	meetingBriefStaleness:     "Staleness guards",
	meetingBriefSharedContext: "Material shared context",
}

var meetingBriefSectionOrder = []string{
	meetingBriefOpenLoops,
	meetingBriefUnresolved,
	meetingBriefStaleness,
	meetingBriefSharedContext,
}

// BriefCitation is the provenance rail for every surfaced meeting-brief line.
// A line cannot be built or rendered without a complete local-memory citation.
type BriefCitation struct {
	memoryID string
	channel  string
	source   string
	date     string
}

type briefCitationJSON struct {
	MemoryID string `json:"memory_id"`
	Channel  string `json:"channel"`
	Source   string `json:"source"`
	Date     string `json:"date"`
}

func newBriefCitation(memoryID, channel, source, date string) (BriefCitation, error) {
	c := BriefCitation{
		memoryID: strings.TrimSpace(memoryID),
		channel:  strings.TrimSpace(channel),
		source:   strings.TrimSpace(source),
		date:     strings.TrimSpace(date),
	}
	if err := c.validate(); err != nil {
		return BriefCitation{}, err
	}
	return c, nil
}

func (c BriefCitation) MemoryID() string { return c.memoryID }
func (c BriefCitation) Channel() string  { return c.channel }
func (c BriefCitation) Source() string   { return c.source }
func (c BriefCitation) Date() string     { return c.date }

func (c BriefCitation) MarshalJSON() ([]byte, error) {
	return json.Marshal(briefCitationJSON{
		MemoryID: c.memoryID,
		Channel:  c.channel,
		Source:   c.source,
		Date:     c.date,
	})
}

func (c *BriefCitation) UnmarshalJSON(b []byte) error {
	var raw briefCitationJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	parsed, err := newBriefCitation(raw.MemoryID, raw.Channel, raw.Source, raw.Date)
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}

func (c BriefCitation) validate() error {
	if c.memoryID == "" {
		return errors.New("missing memory_id")
	}
	if c.channel == "" {
		return errors.New("missing channel")
	}
	if c.source == "" {
		return errors.New("missing source")
	}
	if c.date == "" {
		return errors.New("missing date")
	}
	if _, err := time.Parse(time.RFC3339, c.date); err != nil {
		return fmt.Errorf("invalid date %q: %w", c.date, err)
	}
	return nil
}

// BriefLineCorrection is the one-action correction payload attached to each cited
// line. Both actions write stable-atom keyed decisions to the governance confirm
// queue:
//   - correct (decision=confirm) keeps/pins this source line to the attendee;
//   - unlink (decision=reject) removes this source line from this attendee's brief.
//
// Unlink is destructive and therefore requires explicit confirmation (`--yes`).
type BriefLineCorrection struct {
	StableAtom     govAtom `json:"stable_atom"`
	AttendeeAtom   govAtom `json:"attendee_atom"`
	CorrectCommand string  `json:"correct_command"`
	UnlinkCommand  string  `json:"unlink_command"`
}

func newBriefLineCorrection(stableAtom, attendeeAtom govAtom) (BriefLineCorrection, error) {
	c := BriefLineCorrection{
		StableAtom:   stableAtom,
		AttendeeAtom: attendeeAtom,
		CorrectCommand: fmt.Sprintf(
			"mora brief correct --memory-id %s --attendee %s --confirm",
			stableAtom.Value, attendeeAtom.Value,
		),
		UnlinkCommand: fmt.Sprintf(
			"mora brief correct --memory-id %s --attendee %s --unlink --yes",
			stableAtom.Value, attendeeAtom.Value,
		),
	}
	if err := c.validate(); err != nil {
		return BriefLineCorrection{}, err
	}
	return c, nil
}

func (c BriefLineCorrection) validate() error {
	if c.StableAtom.Kind != atomStableID || strings.TrimSpace(c.StableAtom.Value) == "" {
		return errors.New("missing stable source atom")
	}
	if c.AttendeeAtom.Kind != atomHandle && c.AttendeeAtom.Kind != atomAddress {
		return errors.New("missing attendee atom kind")
	}
	if strings.TrimSpace(c.AttendeeAtom.Value) == "" {
		return errors.New("missing attendee atom value")
	}
	if strings.TrimSpace(c.CorrectCommand) == "" || strings.TrimSpace(c.UnlinkCommand) == "" {
		return errors.New("missing correction commands")
	}
	return nil
}

// CitedBriefLine is the only renderable evidence atom. Text is a compact extract
// from the cited memory, never an inferred conclusion.
type CitedBriefLine struct {
	Text                string               `json:"text"`
	Attendee            string               `json:"attendee,omitempty"`
	Citation            BriefCitation        `json:"citation"`
	Correction          BriefLineCorrection  `json:"correction"`
	Direction           Direction            `json:"direction,omitempty"`
	Owner               govAtom              `json:"owner,omitzero"`
	Counterparty        govAtom              `json:"counterparty,omitzero"`
	CounterpartyLabel   string               `json:"counterparty_label,omitempty"`
	CommitmentID        string               `json:"commitment_id,omitempty"`
	Lifecycle           string               `json:"lifecycle,omitempty"`
	ClosureRef          string               `json:"closure_ref,omitempty"`
	DuplicateOf         string               `json:"duplicate_of,omitempty"`
	StateUncertain      bool                 `json:"state_uncertain,omitempty"`
	CommitmentCitations []CommitmentCitation `json:"commitment_citations,omitempty"`
	DueAt               string               `json:"due_at,omitempty"`
}

func newCitedBriefLine(text, attendee string, citation BriefCitation, correction BriefLineCorrection, asOf time.Time) (CitedBriefLine, error) {
	line := CitedBriefLine{
		Attendee:   strings.TrimSpace(attendee),
		Citation:   citation,
		Correction: correction,
	}
	raw := oneLine(text)
	if raw == "" {
		return CitedBriefLine{}, errors.New("empty cited line")
	}
	if err := line.Citation.validate(); err != nil {
		return CitedBriefLine{}, err
	}
	if err := line.Correction.validate(); err != nil {
		return CitedBriefLine{}, err
	}
	line.Text = meetingBriefHistoricalText(asOf, line.Citation.Date(), line.Attendee, raw)
	return line, nil
}

func (l CitedBriefLine) validate() error {
	if strings.TrimSpace(l.Text) == "" {
		return errors.New("empty text")
	}
	if err := l.Citation.validate(); err != nil {
		return err
	}
	if err := l.Correction.validate(); err != nil {
		return err
	}
	if l.Direction == "" {
		return nil
	}
	if l.Direction != commitOwedBySelf && l.Direction != commitOwedByCounterparty {
		return fmt.Errorf("invalid commitment direction %q", l.Direction)
	}
	if !atomPresent(l.Owner) || !atomPresent(l.Counterparty) {
		return errors.New("typed commitment line is missing owner or counterparty")
	}
	if (l.Direction == commitOwedByCounterparty) != atomEqual(l.Owner, l.Counterparty) {
		return errors.New("commitment owner and direction disagree")
	}
	switch l.DueAt {
	case commitDueNone, commitDueRelative:
	case "":
		return errors.New("typed commitment line is missing due classification")
	default:
		if _, err := time.Parse("2006-01-02", l.DueAt); err != nil {
			return fmt.Errorf("invalid commitment due value %q", l.DueAt)
		}
	}
	return nil
}

// CitedMeetingEvent carries one event memory citation covering all event fields,
// including the attendee roster from that memory's structured metadata.
type CitedMeetingEvent struct {
	ID        string        `json:"id"`
	Title     string        `json:"title"`
	StartsAt  string        `json:"starts_at"`
	Attendees []string      `json:"attendees"`
	Citation  BriefCitation `json:"citation"`
}

func (e CitedMeetingEvent) validate() error {
	if strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.Title) == "" || strings.TrimSpace(e.StartsAt) == "" {
		return errors.New("meeting event is incomplete")
	}
	if _, err := time.Parse(time.RFC3339, e.StartsAt); err != nil {
		return fmt.Errorf("invalid event starts_at %q: %w", e.StartsAt, err)
	}
	return e.Citation.validate()
}

type MeetingBriefSection struct {
	Kind  string           `json:"kind"`
	Title string           `json:"title"`
	Lines []CitedBriefLine `json:"lines"`
}

// MeetingBrief is the shared CLI/MCP P14 shape. EgressCalls is an explicit meter:
// assembly reads only the local vault/index and therefore always reports zero.
type MeetingBrief struct {
	AsOf     string                `json:"as_of"`
	Event    *CitedMeetingEvent    `json:"event"`
	Sections []MeetingBriefSection `json:"sections"`
	// Gaps are honest, user-facing statements of what this brief could NOT establish.
	// A gap suppresses a CLAIM; it never fabricates one and never hides the artifact.
	Gaps []string `json:"gaps,omitempty"`
	// SelfUnresolved reports that Mora could not tell which invitee is the user, so it
	// attributed nothing: any of the invited addresses could BE the user, and citing a
	// record to the user as if it were a counterparty's is wrong-person attribution.
	SelfUnresolved bool `json:"self_unresolved,omitempty"`
	// NameFallback is true only when a name-filtered request found no matching
	// upcoming event and returned the next general event instead.
	NameFallback bool `json:"name_fallback,omitempty"`
	EgressCalls  int  `json:"egress_calls"`
	// SourceHealth is the per-connector freshness snapshot (HEALTH-02), computed
	// ONCE at build time so MCP meeting_prep — which returns this struct
	// directly — stops being confidently silent over a dead corpus. A brief
	// that renders confidently over dead data is a WRONG brief; this is a
	// correctness signal, not ops telemetry.
	SourceHealth []sourceHealth `json:"source_health,omitempty"`
	// idxHealth is the index arm snapshot (Gate 2), pinned at build time next to
	// SourceHealth for the aggregate banner. UNEXPORTED — the CLI render path
	// (renderMeetingBrief) reads it directly; the MCP/HTTP payload gets the
	// bounded projection via Health below instead of this raw arm.
	idxHealth indexHealth
	// producerHealth is the producer-liveness arm snapshot (Gate 2 / HEALTH-11),
	// pinned at build time so a dead automation reaches the meeting brief's banner.
	producerHealth []producerHealth
	// Health is the BOUNDED envelope (Packet C1) — meeting_prep returns this
	// struct directly (no digestMCPPayload-style wrapper to inject into), so the
	// compact state/banner is a field on the struct itself, computed once at
	// build time from the SAME SourceHealth/idxHealth/producerHealth snapshot above.
	Health compactHealth `json:"health"`
}

func (b MeetingBrief) validate() error {
	asOf, err := time.Parse(time.RFC3339, b.AsOf)
	if err != nil {
		return fmt.Errorf("invalid as_of %q: %w", b.AsOf, err)
	}
	if b.EgressCalls != 0 {
		return fmt.Errorf("meeting brief egress meter is %d, want 0", b.EgressCalls)
	}
	if b.Event == nil {
		if len(b.Sections) != 0 {
			return errors.New("meeting brief has sections without an event")
		}
		return nil
	}
	if err := b.Event.validate(); err != nil {
		return fmt.Errorf("event citation: %w", err)
	}
	for _, section := range b.Sections {
		title, known := meetingBriefSectionTitles[section.Kind]
		if !known || title != section.Title {
			return fmt.Errorf("unknown meeting brief section %q", section.Kind)
		}
		if len(section.Lines) == 0 {
			return fmt.Errorf("meeting brief section %q is empty", section.Kind)
		}
		for i, line := range section.Lines {
			if err := line.validate(); err != nil {
				return fmt.Errorf("%s line %d is uncited: %w", section.Kind, i, err)
			}
			if err := line.validateHistorical(asOf); err != nil {
				return fmt.Errorf("%s line %d violates the dated-historical rail: %w", section.Kind, i, err)
			}
		}
	}
	return nil
}

type meetingBriefCandidate struct {
	kind           string
	line           CitedBriefLine
	rank           forgettabilityCandidate
	decisionKey    string
	attendeeSender bool
}

// buildEventMeetingBrief resolves one calendar memory by stable id, then assembles
// an unfinished-business brief over each exact attendee identity. Candidate
// discovery uses the same exact-identity graph projection as get_entity, but ranking
// runs over the full cross-attendee pool before the output budget is applied.
func buildEventMeetingBrief(ctx context.Context, cfg Config, eventID string, at time.Time, maxTokens, perAttendee int) (MeetingBrief, error) {
	eventMemory, err := meetingEventByID(cfg, eventID)
	if err != nil {
		return MeetingBrief{}, err
	}
	return buildMeetingBriefFromEvent(ctx, cfg, eventMemory, at, maxTokens, perAttendee)
}

func buildNextMeetingBrief(ctx context.Context, cfg Config, at time.Time, attendeeFilterIDs map[string]bool, maxTokens, perAttendee int) (MeetingBrief, error) {
	hSnap := healthOf(cfg, at)
	empty := MeetingBrief{
		AsOf:           at.UTC().Format(time.RFC3339),
		Sections:       []MeetingBriefSection{},
		EgressCalls:    0,
		SourceHealth:   hSnap.Sources,
		idxHealth:      hSnap.Index,
		producerHealth: hSnap.Producers,
		Health:         compactHealthFrom(hSnap),
	}
	mems, err := meetingBriefMemories(cfg)
	if err != nil {
		return MeetingBrief{}, err
	}
	event := selectNextEvent(mems, at, attendeeFilterIDs)
	nameFallback := false
	if event == nil && len(attendeeFilterIDs) > 0 {
		event = selectNextEvent(mems, at, nil)
		nameFallback = event != nil
	}
	if event == nil {
		return empty, nil
	}
	for _, m := range mems {
		if m.ID == event.StableID {
			brief, err := buildMeetingBriefFromEvent(ctx, cfg, m, at, maxTokens, perAttendee)
			if err != nil {
				return MeetingBrief{}, err
			}
			brief.NameFallback = nameFallback
			return brief, nil
		}
	}
	return MeetingBrief{}, fmt.Errorf("selected calendar event %q is missing from the vault", event.StableID)
}

func buildMeetingBriefFromEvent(ctx context.Context, cfg Config, eventMemory Memory, at time.Time, maxTokens, perAttendee int) (MeetingBrief, error) {
	if eventMemory.DeletedAt != "" || eventMemory.Type != "event" {
		return MeetingBrief{}, fmt.Errorf("memory %q is not an active calendar event", eventMemory.ID)
	}
	start, ok := eventStart(eventMemory)
	if !ok {
		return MeetingBrief{}, fmt.Errorf("calendar event %q has no valid RFC3339 start", eventMemory.ID)
	}
	if perAttendee <= 0 {
		perAttendee = meetingBriefDefaultPerGuest
	}

	self := selfEmails(cfg)
	rawAttendees := metaStrings(eventMemory.Meta["attendees"])
	// Can Mora pick the user out of the invitee list? Knowing SOME address of the user
	// is not enough: if none of the invited addresses is one of them, the user was
	// invited under an alias, and Mora cannot tell which invitee is the user.
	selfUnresolved := len(rawAttendees) > 0 && !meetingBriefSelfAmongAttendees(eventMemory, rawAttendees, self)

	attendees := meetingBriefAttendees(eventMemory, self)
	eventCitation, err := citationForMemory(eventMemory, eventMemory.ID, validFromOf(eventMemory))
	if err != nil {
		return MeetingBrief{}, fmt.Errorf("event %s citation: %w", eventMemory.ID, err)
	}
	event := &CitedMeetingEvent{
		ID:        eventMemory.ID,
		Title:     eventMemory.Title,
		StartsAt:  start.UTC().Format(time.RFC3339),
		Attendees: attendeeDisplays(attendees),
		Citation:  eventCitation,
	}
	hSnap := healthOf(cfg, at)
	brief := MeetingBrief{
		AsOf:           at.UTC().Format(time.RFC3339),
		Event:          event,
		Sections:       []MeetingBriefSection{},
		EgressCalls:    0,
		SourceHealth:   hSnap.Sources,
		idxHealth:      hSnap.Index,
		producerHealth: hSnap.Producers,
		Health:         compactHealthFrom(hSnap),
	}
	// Refuse-to-GAP, not refuse-to-error. If Mora cannot pick the user out of the
	// invitee list, then ANY invitee could BE the user, so it must not attribute a
	// single line to any of them — citing the user's own record back as a
	// counterparty's unfinished business is wrong-person attribution (severity-1).
	//
	// Suppress the claims; keep the artifact. Erroring buys no extra safety (an
	// unattributed brief emits zero lines either way) and would take the whole
	// next-meeting brief down over one unresolvable event — `selectNextEvent` is
	// provider-agnostic, so a single unrecognized dentist appointment would break the
	// flagship surface. Gapping states the limit honestly and says how to fix it.
	if selfUnresolved {
		brief.SelfUnresolved = true
		brief.Gaps = append(brief.Gaps, fmt.Sprintf(
			"Cannot tell which invitee is you (%s), so nothing is attributed for this meeting. Add your other addresses to config.toml: self_emails = \"you@alias.com\".",
			strings.Join(rawAttendees, ", ")))
		return brief, nil
	}

	_, budgetChars := resolveContextBudgetTokens(cfg, maxTokens)
	payloadBudget := budgetChars / mcpDigestEnvelopeDivisor
	if baseSize := jsonLen(brief); baseSize > payloadBudget {
		return MeetingBrief{}, fmt.Errorf("meeting brief event requires %d compact bytes, exceeding the %d-byte max_tokens budget", baseSize, payloadBudget)
	}

	// roster is everyone in this meeting. A thread among the meeting's own people is
	// still the meeting's business; an OUTSIDER on the thread is what breaks attribution.
	roster := make([]string, 0, len(attendees))
	for _, a := range attendees {
		roster = append(roster, a.identity)
	}

	associationsByMemory := map[string][]meetingBriefCandidate{}
	governanceLedger, err := loadGovernance(cfg)
	if err != nil {
		return MeetingBrief{}, err
	}
	lineDecisions := governanceLedger.briefLineDecisions()
	commitmentsByMemory, err := readCommitmentInventory(ctx, cfg, at)
	if err != nil {
		return MeetingBrief{}, err
	}
	for _, attendee := range attendees {
		dossier, derr := graphGetEntity(ctx, cfg, attendee.identity)
		if derr != nil {
			return MeetingBrief{}, derr
		}
		if dossier["found"] != true {
			continue
		}
		display := attendee.display
		if d, _ := dossier["display_name"].(string); strings.TrimSpace(d) != "" {
			display = d
		}
		personKind, _ := dossier["kind"].(string)
		mentionCount, _ := dossier["count"].(int)
		aliases, _ := dossier["aliases"].([]string)
		evidence, _ := dossier["memories"].([]Memory)
		related := relationalEvidenceIDs(dossier)
		personLastSeen := latestMeetingBriefEvidenceDate(evidence, at)
		for _, m := range evidence {
			if m.ID == eventMemory.ID {
				continue
			}
			if m.DeletedAt != "" {
				continue
			}
			// The dossier pools every edge rel. A MENTIONS edge means the gazetteer
			// found this person's NAME in a body they were not a participant of — so
			// the record is someone writing ABOUT them, never unfinished business
			// BETWEEN the user and them. Surfacing it under their name is wrong-person
			// attribution (severity-1), so require a relationship edge.
			if !related[m.ID] {
				continue
			}
			occurredAt := validFromOf(m)
			if ts, terr := time.Parse(time.RFC3339, occurredAt); terr != nil || ts.After(at) {
				continue
			}
			kind := classifyMeetingBriefEvidence(m, cfg, at)
			// A Gmail message is unfinished business WITH this attendee only when the
			// user or the attendee actually wrote it. Inbound third-party mail the
			// attendee was merely co-copied on (a vendor emailing you both, a marketing
			// blast) is not an obligation between you and them — drop it so it can't
			// surface under their name.
			if isGmailMemory(m) && !meetingBriefSelfIsSender(m, self) && !meetingBriefSenderIs(m, attendee.identity) {
				continue
			}
			// ...and only when the two of them were actually the ones talking. A thread
			// carrying another human recipient leaves the addressee ambiguous — the ask
			// may well be aimed at them, not at the user — and Mora does not guess.
			if isGmailMemory(m) && !meetingBriefIsTwoPartyExchange(m, self, roster...) {
				continue
			}
			attendeeAtom, atomErr := attendeeAtomForIdentity(attendee.identity)
			if atomErr != nil {
				return MeetingBrief{}, fmt.Errorf("event %s attendee %s atom: %w", eventMemory.ID, attendee.identity, atomErr)
			}
			excerpt := ""
			if kind != "" {
				excerpt = meetingBriefActionableEvidenceText(m, cfg, at, kind)
			}
			commitment, typed := meetingCommitmentFor(commitmentsByMemory[m.ID], attendeeAtom, aliases, excerpt)
			if !typed {
				// Legacy line heuristics may nominate context, questions, or quoted
				// text. The materialized inventory is the eligibility authority:
				// without an exact typed commitment this is not an obligation line.
				continue
			}
			if commitment.State != commitOpen || commitment.DuplicateOf != "" {
				// Lifecycle/dedup are inventory knowledge, not display claims. A
				// closed, superseded, or duplicate obligation remains queryable in
				// the materialization but cannot leak back into the open-loop brief.
				continue
			}
			if !commitmentRefersToMeeting(commitment, eventMemory, attendeeDisplays(attendees)) {
				// A commitment with an attendee is not automatically about this
				// meeting. The named-event contract requires explicit reference;
				// mere co-occurrence in the attendee's history is insufficient.
				continue
			}
			if excerpt == "" {
				kind = meetingBriefOpenLoops
				excerpt = commitment.Summary
			}
			if kind == "" || excerpt == "" {
				continue
			}
			source := evidenceSource(m)
			citation, cerr := citationForMemory(m, source, occurredAt)
			if cerr != nil {
				return MeetingBrief{}, fmt.Errorf("event %s attendee %s evidence %s citation: %w", eventMemory.ID, attendee.identity, m.ID, cerr)
			}
			correction, corrErr := briefLineCorrectionForEvidence(m, attendee.identity)
			if corrErr != nil {
				return MeetingBrief{}, fmt.Errorf("event %s attendee %s evidence %s correction: %w", eventMemory.ID, attendee.identity, m.ID, corrErr)
			}
			line, lerr := newCitedBriefLine(
				excerpt,
				display,
				citation,
				correction,
				at,
			)
			if lerr != nil {
				return MeetingBrief{}, fmt.Errorf("event %s attendee %s evidence %s: %w", eventMemory.ID, attendee.identity, m.ID, lerr)
			}
			attachCommitment(&line, commitment)
			bulkAuthored := memoryIsServiceOnly(m)
			messageCount := metaMessageCount(m)
			candidate := meetingBriefCandidate{
				kind:           kind,
				line:           line,
				decisionKey:    briefLineDecisionKey(line.Correction.StableAtom, line.Correction.AttendeeAtom),
				attendeeSender: meetingBriefAttendeeIsSender(m, attendee.identity),
				rank: forgettabilityCandidate{
					StableID:             m.ID,
					Title:                m.Title,
					Text:                 stripFromLine(m.Text),
					CreatedAt:            m.CreatedAt,
					OccurredAt:           occurredAt,
					DeletedAt:            m.DeletedAt,
					PersonID:             personID(attendee.identity),
					PersonDisplay:        display,
					PersonKind:           personKind,
					PersonLastSeen:       personLastSeen,
					AttendeeKnown:        true,
					IdentityCorroborated: true,
					BulkAuthored:         bulkAuthored,
					HumanAuthored:        !bulkAuthored,
					ContentCorroborated:  messageCount > 1,
					Commit:               kind == meetingBriefOpenLoops,
					MessageCount:         messageCount,
					MentionCount:         mentionCount,
				},
			}
			associationsByMemory[m.ID] = append(associationsByMemory[m.ID], candidate)
		}
	}

	candidateIDs := make([]string, 0, len(associationsByMemory))
	for id := range associationsByMemory {
		candidateIDs = append(candidateIDs, id)
	}
	sort.Strings(candidateIDs)
	candidates := make([]meetingBriefCandidate, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		candidate, unambiguous := meetingBriefResolveAttribution(associationsByMemory[id], lineDecisions)
		if unambiguous {
			candidates = append(candidates, candidate)
		}
	}
	rankInputs := make([]forgettabilityCandidate, 0, len(candidates))
	candidateByID := make(map[string]meetingBriefCandidate, len(candidates))
	for _, candidate := range candidates {
		rankInputs = append(rankInputs, candidate.rank)
		candidateByID[candidate.rank.StableID] = candidate
	}
	ranking := rankForgettability(
		at,
		eventMemory.Title,
		attendeeDisplays(attendees),
		rankInputs,
		forgettabilityOptions{
			PerAttendeeCap: perAttendee,
			EvidenceCap:    meetingPrepEvidenceCap,
		},
	)
	candidates = candidates[:0]
	for _, selected := range ranking.Selected {
		candidate := candidateByID[selected.StableID]
		trial := append(candidates, candidate)
		if jsonLen(meetingBriefWithCandidates(brief, trial)) > payloadBudget {
			continue
		}
		candidates = trial
	}

	brief = meetingBriefWithCandidates(brief, candidates)
	if err := brief.validate(); err != nil {
		return MeetingBrief{}, err
	}
	return brief, nil
}

// commitmentRefersToMeeting implements the named-event placement contract without
// a hidden model or corpus-specific vocabulary. Explicit full-title references
// qualify. Otherwise, the evidence must either share multiple distinctive terms
// with the title/agenda or use a definite reference ("the review", "the checklist")
// to a distinctive meeting term. One incidental shared token is not enough.
func commitmentRefersToMeeting(commitment Commitment, event Memory, attendeeNames []string) bool {
	evidence := strings.ToLower(oneLine(commitment.Summary + " " + commitment.OpenedBy.Quote))
	title := strings.ToLower(oneLine(event.Title))
	if evidence == "" || title == "" {
		return false
	}
	// "explicitly names": the evidence contains the normalized event title.
	if strings.Contains(evidence, title) {
		return true
	}

	eventTokens := forgettabilityDistinctiveTokens(event.Title+" "+event.Text, attendeeNames)
	evidenceTokens := forgettabilityDistinctiveTokens(evidence, attendeeNames)
	// "unmistakably refers": two independently distinctive title/agenda terms
	// corroborate the reference; one incidental shared term cannot qualify.
	if intersectionSize(eventTokens, evidenceTokens) >= 2 {
		return true
	}
	// "unmistakably refers": a single-token definite reference is unambiguous only
	// for the event-kind referent at the end of its title ("the review", "the
	// session"). Applying "the" to any title token would turn "the Atrium cards"
	// into a meeting link.
	titleTokens := forgettabilityDistinctiveTokens(event.Title, attendeeNames)
	titleWords := tokenizeWords(event.Title)
	for i := len(titleWords) - 1; i >= 0; i-- {
		token := titleWords[i]
		if titleTokens[token] {
			return evidenceTokens[token] && strings.Contains(evidence, "the "+token)
		}
	}
	return false
}

func meetingBriefWithCandidates(brief MeetingBrief, candidates []meetingBriefCandidate) MeetingBrief {
	brief.Sections = brief.Sections[:0]
	for _, kind := range meetingBriefSectionOrder {
		lines := make([]CitedBriefLine, 0)
		for _, candidate := range candidates {
			if candidate.kind == kind {
				lines = append(lines, candidate.line)
			}
		}
		if len(lines) > 0 {
			brief.Sections = append(brief.Sections, MeetingBriefSection{
				Kind:  kind,
				Title: meetingBriefSectionTitles[kind],
				Lines: lines,
			})
		}
	}
	return brief
}

func meetingBriefResolveAttribution(associations []meetingBriefCandidate, lineDecisions map[string]string) (meetingBriefCandidate, bool) {
	filtered := make([]meetingBriefCandidate, 0, len(associations))
	confirmed := make([]meetingBriefCandidate, 0, len(associations))
	for _, candidate := range associations {
		switch lineDecisions[candidate.decisionKey] {
		case mergeDecisionReject:
			continue
		case mergeDecisionConfirm:
			confirmed = append(confirmed, candidate)
		}
		filtered = append(filtered, candidate)
	}
	if len(confirmed) == 1 {
		return confirmed[0], true
	}
	if len(confirmed) > 1 || len(filtered) == 0 {
		return meetingBriefCandidate{}, false
	}

	byPerson := map[string]meetingBriefCandidate{}
	for _, candidate := range filtered {
		current, exists := byPerson[candidate.rank.PersonID]
		if !exists || candidate.attendeeSender {
			byPerson[candidate.rank.PersonID] = candidate
		} else {
			byPerson[candidate.rank.PersonID] = current
		}
	}
	var only meetingBriefCandidate
	senders := make([]meetingBriefCandidate, 0, len(byPerson))
	for _, candidate := range byPerson {
		only = candidate
		if candidate.attendeeSender {
			senders = append(senders, candidate)
		}
	}
	if len(senders) == 1 {
		return senders[0], true
	}
	if len(senders) == 0 && len(byPerson) == 1 {
		return only, true
	}
	return meetingBriefCandidate{}, false
}

func meetingBriefAttendeeIsSender(m Memory, identity string) bool {
	target := personID(identity)
	_, senders, _, _ := personRefs(m)
	for _, sender := range senders {
		if sender == target {
			return true
		}
	}
	return false
}

// meetingBriefSenderIs reports whether any From address canonicalizes (gmail
// dot/plus aware, via mailboxKey) to the same mailbox as want. Used so an attendee
// who wrote from an alias variant of their address still counts as the sender,
// rather than having their genuine mail dropped by an exact-string mismatch.
func meetingBriefSenderIs(m Memory, want string) bool {
	target := mailboxKey(want)
	if target == "" {
		return false
	}
	for _, from := range lowerStrings(metaStrings(m.Meta["from"])) {
		if mailboxKey(from) == target {
			return true
		}
	}
	return false
}

// meetingBriefSelfIsSender reports whether the user sent this mail (any From
// address canonicalizes to one of the user's own). Keeps the user's OWN outbound
// asks while dropping inbound third-party mail the attendee was merely co-copied on.
func meetingBriefSelfIsSender(m Memory, self map[string]bool) bool {
	for s := range self {
		if meetingBriefSenderIs(m, s) {
			return true
		}
	}
	return false
}

func latestMeetingBriefEvidenceDate(mems []Memory, at time.Time) string {
	var latest time.Time
	for _, m := range mems {
		ts, ok := parseForgettabilityTime(validFromOf(m), m.CreatedAt)
		if !ok || ts.After(at) || !ts.After(latest) {
			continue
		}
		latest = ts
	}
	if latest.IsZero() {
		return ""
	}
	return latest.UTC().Format(time.RFC3339)
}

func meetingBriefHistoricalText(asOf time.Time, date, attendee, raw string) string {
	prefix := meetingBriefHistoricalPrefix(asOf, date, attendee)
	return prefix + "“" + oneLine(raw) + "”"
}

// meetingBriefHistoricalPrefix stamps every line with its age and the person it
// concerns. The DATED framing is the P15 invariant — a fact from ten months ago must
// never read as true now — and it is load-bearing, not decoration.
//
// The wording is deliberately "· <person> —" rather than "<person> wrote:": the
// memory is one this person is INVOLVED in, and Mora does not always know they
// authored it. Claiming authorship it cannot prove would be its own wrong-person bug.
// What it can say honestly is when, who it involves, and the exact words.
func meetingBriefHistoricalPrefix(asOf time.Time, date, attendee string) string {
	factAt, err := time.Parse(time.RFC3339, date)
	if err != nil {
		return ""
	}
	age := meetingBriefRelativeAge(asOf, factAt)
	attendee = strings.TrimSpace(attendee)
	if attendee == "" {
		return age + " — "
	}
	return fmt.Sprintf("%s · %s — ", age, attendee)
}

func meetingBriefRelativeAge(asOf, factAt time.Time) string {
	age := asOf.Sub(factAt)
	if age < 0 {
		age = 0
	}
	days := int(age.Hours()/24 + 0.5)
	switch {
	case days < 1:
		return "Earlier that day"
	case days == 1:
		return "~1 day ago"
	case days < 60:
		return fmt.Sprintf("~%d days ago", days)
	case days < 730:
		months := int(float64(days)/30.4375 + 0.5)
		if months < 2 {
			months = 2
		}
		return fmt.Sprintf("~%d months ago", months)
	default:
		years := int(float64(days)/365.25 + 0.5)
		if years < 2 {
			years = 2
		}
		return fmt.Sprintf("~%d years ago", years)
	}
}

func (l CitedBriefLine) validateHistorical(asOf time.Time) error {
	prefix := meetingBriefHistoricalPrefix(asOf, l.Citation.Date(), l.Attendee)
	if prefix == "" || !strings.HasPrefix(l.Text, prefix) || !strings.HasSuffix(l.Text, "”") {
		return errors.New("evidence must be rendered as a dated, past-tense cited record")
	}
	return nil
}

type meetingBriefAttendee struct {
	identity string
	display  string
}

// meetingBriefSelfAmongAttendees reports whether the user is identifiable among the
// event's invited addresses — either because the connector recorded Google's
// authoritative Attendee.Self, or because one invited address is a known address of
// theirs (source mailbox or a declared self_emails alias). A solo event (no other
// invitee) is not a wrong-person risk, so it is allowed through.
func meetingBriefSelfAmongAttendees(event Memory, rawAttendees []string, self map[string]bool) bool {
	if s, _ := event.Meta["self_email"].(string); strings.TrimSpace(s) != "" {
		return true
	}
	for _, raw := range rawAttendees {
		if self[strings.ToLower(strings.TrimSpace(raw))] {
			return true
		}
	}
	return false
}

func meetingBriefAttendees(event Memory, self map[string]bool) []meetingBriefAttendee {
	names := metaNames(event.Meta["names"])
	seen := map[string]bool{}
	// The event's own self_email is Google's authoritative answer to "which of these
	// attendees is the user" (Attendee.Self), and it beats every inference: the
	// calendar routinely invites an alias the connected mailbox has never seen. Union
	// it with the configured/source-derived self set so the user is never admitted as
	// an attendee of their own meeting.
	eventSelf, _ := event.Meta["self_email"].(string)
	eventSelf = strings.ToLower(strings.TrimSpace(eventSelf))
	var out []meetingBriefAttendee
	for _, raw := range metaStrings(event.Meta["attendees"]) {
		identity := strings.ToLower(strings.TrimSpace(raw))
		if identity == "" || self[identity] || seen[identity] || isStructuralNoise(identity) {
			continue
		}
		if eventSelf != "" && identity == eventSelf {
			continue
		}
		seen[identity] = true
		display := strings.TrimSpace(names[identity])
		if display == "" {
			display = identity
		}
		out = append(out, meetingBriefAttendee{identity: identity, display: display})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].identity < out[j].identity })
	return out
}

func attendeeDisplays(attendees []meetingBriefAttendee) []string {
	out := make([]string, 0, len(attendees))
	for _, attendee := range attendees {
		out = append(out, attendee.display)
	}
	return out
}

func meetingEventByID(cfg Config, eventID string) (Memory, error) {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return Memory{}, errors.New("--event-id requires a non-empty calendar memory id")
	}
	mems, err := meetingBriefMemories(cfg)
	if err != nil {
		return Memory{}, err
	}
	var matches []Memory
	for _, m := range mems {
		if m.Type == "event" && (m.ID == eventID || m.ProviderID == eventID) {
			matches = append(matches, m)
		}
	}
	if len(matches) == 0 {
		return Memory{}, fmt.Errorf("calendar event not found: %s", eventID)
	}
	if len(matches) > 1 {
		return Memory{}, fmt.Errorf("calendar event id %q is ambiguous across %d memories", eventID, len(matches))
	}
	return matches[0], nil
}

func meetingBriefMemories(cfg Config) ([]Memory, error) {
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return nil, err
	}
	mems := make([]Memory, 0, len(files))
	governance, err := loadGovernance(cfg)
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		m, perr := parseMemory(path)
		if perr != nil || m.DeletedAt != "" {
			continue
		}
		if !governance.memoryVisible(m.ID) {
			continue
		}
		mems = append(mems, m)
	}
	return mems, nil
}

func meetingBriefLineCount(brief MeetingBrief) int {
	count := 0
	for _, section := range brief.Sections {
		count += len(section.Lines)
	}
	return count
}

func citationForMemory(m Memory, source, date string) (BriefCitation, error) {
	channel := strings.TrimSpace(m.Provider)
	if channel == "" {
		channel = strings.TrimSpace(m.Type)
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = strings.TrimSpace(evidenceSource(m))
	}
	if source == "" {
		source = channel
	}
	if channel == "" {
		channel = source
	}
	if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(date)); err == nil {
		date = parsed.UTC().Format(time.RFC3339)
	}
	return newBriefCitation(m.ID, channel, source, date)
}

func attendeeAtomForIdentity(identity string) (govAtom, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return govAtom{}, errors.New("empty attendee identity")
	}
	if strings.Contains(identity, "@") {
		return govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, identity)}, nil
	}
	return govAtom{Provider: "imessage", Kind: atomHandle, Value: normalizeIdentity(atomHandle, identity)}, nil
}

func briefLineCorrectionForEvidence(m Memory, attendeeIdentity string) (BriefLineCorrection, error) {
	attendeeAtom, err := attendeeAtomForIdentity(attendeeIdentity)
	if err != nil {
		return BriefLineCorrection{}, err
	}
	return newBriefLineCorrection(itemAtom(m.Provider, m.ID), attendeeAtom)
}

func meetingBriefActionableEvidenceText(m Memory, cfg Config, at time.Time, kind string) string {
	// Only the sender's OWN prose is evidence — never a forwarded stranger, a quoted
	// reply chain, a signature, or a legal disclaimer.
	body := senderAuthoredBody(stripFromLine(m.Text))
	for _, rawSegment := range meetingBriefEvidenceSegments(body) {
		segment := stripNoiseTokens(rawSegment)
		if isIMessageMemory(m) {
			// "Me: Good morning leaving now" — the transcript's speaker label is
			// scaffolding, not part of what was said, and must never render in a line.
			segment = stripSpeakerPrefix(segment)
		}
		if segment == "" || isLeadInFragment(segment) {
			continue
		}
		probe := m
		probe.Title = ""
		probe.Text = segment
		if !containsPersonalTrivia(segment) && classifyMeetingBriefEvidence(probe, cfg, at) == kind {
			return truncateRunes(oneLine(segment), 360)
		}
	}
	// Fallback: the subject line. A FORWARDED subject is a stranger's subject — the
	// same reason the forwarded body is not the sender's words — so it is not evidence
	// of anything between the user and this attendee.
	title := oneLine(m.Title)
	if title == "" || containsPersonalTrivia(title) || isForwardedSubject(title) || isLeadInFragment(title) {
		return ""
	}
	probe := m
	probe.Title = title
	probe.Text = ""
	if classifyMeetingBriefEvidence(probe, cfg, at) == kind {
		return truncateRunes(title, 360)
	}
	return ""
}

// meetingBriefEvidenceSegments cuts a memory body into candidate sentences.
//
// The text is CLEANED BEFORE it is cut, and that order is the whole point. Cutting
// first is what produced the gibberish: a sentence split on "." shreds
// "https://meet.google.com/ctk-rdtz-jnx?hs=224" into "https://meet." + "google." +
// "com/ctk-rdtz-jnx?", and that last shard — one slash, mostly letters, ending in
// "?" — no longer looks like a URL to any downstream filter, so it read as a
// question the user owed someone. Strip URLs while they are still URLs.
//
// Hard wraps are unwrapped for the same reason. Gmail plain text wraps at ~72
// columns, and treating "\n" as a sentence terminator cut clauses in half ("Please
// share the Ahrefs findings/report prior to the"). A newline only ends a sentence
// when the line actually ended one.
func meetingBriefEvidenceSegments(text string) []string {
	text = unwrapHardWraps(stripURLs(text))

	var segments []string
	var current strings.Builder
	flush := func() {
		if segment := oneLine(current.String()); segment != "" {
			segments = append(segments, segment)
		}
		current.Reset()
	}
	for _, r := range text {
		current.WriteRune(r)
		switch r {
		case '\n', '.', '!', '?', ';':
			flush()
		}
	}
	flush()
	return segments
}

// quotedBlockMarkers open a region of a mail body that the SENDER DID NOT WRITE: a
// forwarded message, a quoted reply chain, a signature, or a legal disclaimer.
// Everything from the first such marker onward belongs to someone else (or to
// nobody), and attributing it to the sender is how "Fwd: Ai / AEO" — Gouri
// forwarding a marketing mail — put a stranger's CTA ("Open to see how the loop
// works?") into the brief as Gouri's unfinished business with the user.
var quotedBlockMarkers = []string{
	"---------- forwarded message",
	"-----original message-----",
	"begin forwarded message:",
	"________________________________",
	"this email and any files transmitted",
	"confidential and intended solely",
	"sent from my iphone",
	"unsubscribe",
}

// quotedReplyLine matches the "On <date>, <someone> wrote:" attribution line that
// opens a quoted reply chain, in the forms Gmail/Outlook actually emit.
var quotedReplyLine = regexp.MustCompile(`(?i)^\s*on .{0,120}\bwrote:\s*$`)

// signatureDelimiter is the RFC 3676 signature separator ("-- ") plus the bare "--"
// that most clients emit in practice.
func isSignatureDelimiter(line string) bool {
	t := strings.TrimRight(line, " \t")
	return t == "--" || t == "-"
}

// senderAuthoredBody returns only the prose the sender actually wrote: the body up to
// the first forwarded block, quoted reply, signature, or legal disclaimer. Quoted
// lines (">") are dropped wherever they appear.
//
// Without this, every reply chain re-litigates its own history and every forward puts
// a stranger's words in the sender's mouth — and the brief attributes both to the
// attendee as things they are waiting on the user for.
func senderAuthoredBody(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		lower := strings.ToLower(strings.TrimSpace(line))
		if quotedReplyLine.MatchString(line) || isSignatureDelimiter(line) {
			break
		}
		stop := false
		for _, marker := range quotedBlockMarkers {
			if strings.Contains(lower, marker) {
				stop = true
				break
			}
		}
		if stop {
			break
		}
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			continue // quoted remnant
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// speakerPrefix matches the transcript label an iMessage line carries ("Me: ",
// "Gouri Karode: "). It is scaffolding the renderer added, not words anybody spoke.
var speakerPrefix = regexp.MustCompile(`^[\p{L}][\p{L}\p{N} .'’\-]{0,30}:\s+`)

// stripSpeakerPrefix removes a leading transcript speaker label from a line.
func stripSpeakerPrefix(segment string) string {
	return strings.TrimSpace(speakerPrefix.ReplaceAllString(segment, ""))
}

// isForwardedSubject reports whether a subject line marks the mail as a forward.
// "Re:" is deliberately NOT included: a reply is still the sender writing to you.
func isForwardedSubject(title string) bool {
	lower := strings.ToLower(strings.TrimSpace(title))
	return strings.HasPrefix(lower, "fwd:") || strings.HasPrefix(lower, "fw:")
}

// isLeadInFragment reports whether a sentence merely ANNOUNCES content instead of
// carrying it. "Based on our conversation, here are the next steps and deliverables:"
// ends in a colon and states nothing the user must do — it points at a list that the
// sentence splitter then threw away. A brief line has to stand on its own.
func isLeadInFragment(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return true
	}
	if strings.HasSuffix(t, ":") {
		return true
	}
	// A "sentence" of one or two words is a header, not a statement.
	return len(strings.Fields(t)) < 3
}

// urlPattern matches a URL while it is still whole: an explicit scheme, a www.
// host, or a bare host-with-path ("meet.google.com/ctk-rdtz-jnx?hs=224",
// "aka.ms/JoinTeamsMeeting?omkt=en-US") — the shape that survives a plain-text
// email once the mail client has dropped the angle brackets.
var urlPattern = regexp.MustCompile(`(?i)(?:https?://|www\.)\S+|[a-z0-9][a-z0-9.\-]*\.[a-z]{2,}/\S*`)

// stripURLs removes whole URLs from a body before it is segmented, so URL debris
// can never be mistaken for prose. It runs on the RAW text — after segmentation the
// pieces no longer look like URLs, which is exactly how the shards got through.
func stripURLs(text string) string {
	return urlPattern.ReplaceAllString(text, " ")
}

// unwrapHardWraps joins a line to the next when the line did not actually end a
// sentence and the next line continues it (a lowercase or digit start). Plain-text
// email is hard-wrapped at a fixed column, so those newlines are typography, not
// punctuation. Blank lines, and lines that end in real terminators, still break.
//
// The lowercase-continuation test is deliberately conservative: it never merges two
// iMessage turns, because a new turn starts with a capitalized speaker name.
func unwrapHardWraps(text string) string {
	lines := strings.Split(text, "\n")
	var out strings.Builder
	for i, line := range lines {
		out.WriteString(line)
		if i == len(lines)-1 {
			break
		}
		trimmed := strings.TrimRight(line, " \t")
		next := strings.TrimLeft(lines[i+1], " \t")
		if continuesSentence(trimmed, next) {
			out.WriteByte(' ')
			continue
		}
		out.WriteByte('\n')
	}
	return out.String()
}

// continuesSentence reports whether next is the wrapped remainder of line.
func continuesSentence(line, next string) bool {
	if line == "" || next == "" {
		return false
	}
	switch line[len(line)-1] {
	case '.', '!', '?', ';', ':', '|', '-', '*':
		return false
	}
	r := rune(next[0])
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

func containsPersonalTrivia(text string) bool {
	return containsAnyPhrase(strings.ToLower(text), personalTriviaPhrases)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// meetingNotificationSubjects are the Google Calendar RSVP/invite subject prefixes.
// These mails are EVENT PLUMBING, machine-generated by the calendar server — nobody
// wrote them to the user.
var meetingNotificationSubjects = []string{
	"invitation:", "updated invitation:", "declined:", "accepted:", "tentative:",
	"canceled:", "cancelled:", "rescheduled:", "reminder:", "notification:",
}

// meetingNotificationBodyMarkers are boilerplate blocks emitted by conferencing
// tools. Their presence means the body is join-instructions, not correspondence.
var meetingNotificationBodyMarkers = []string{
	"has declined this invitation", "has accepted this invitation",
	"you have been invited to the following event", "view all guest info",
	"join with google meet", "microsoft teams meeting", "join zoom meeting",
	"join by phone", "meeting id:", "passcode:", "rsvp to this event",
}

// isMeetingNotification reports whether a mail is a machine-generated calendar /
// conferencing notification rather than something a human wrote to the user.
//
// These were being mined for evidence, and they are where the gibberish came from:
// a "Declined: Sync up meeting" RSVP notice contributed the Google Meet URL that got
// shredded into "com/ctk-rdtz-jnx?", and a Microsoft Teams invite contributed
// "Need help?" — its own support-link footer — as an unresolved thread with an
// attendee. There is no unfinished business inside an RSVP receipt.
func isMeetingNotification(m Memory) bool {
	title := strings.ToLower(strings.TrimSpace(m.Title))
	for _, prefix := range meetingNotificationSubjects {
		if strings.HasPrefix(title, prefix) {
			return true
		}
	}
	return containsAnyPhrase(strings.ToLower(m.Text), meetingNotificationBodyMarkers)
}

// selfNameTokens derives the user's own name tokens from the addresses Mora already
// knows (source mailboxes + declared self_emails), so the third-party check has
// something to compare an explicit assignee against. Only the LOCAL part is used —
// the domain is a company, not a person.
func selfNameTokens(self map[string]bool) map[string]bool {
	out := map[string]bool{}
	for addr := range self {
		local, _, found := strings.Cut(addr, "@")
		if !found {
			local = addr
		}
		for _, part := range strings.FieldsFunc(local, func(r rune) bool {
			return r == '.' || r == '_' || r == '-' || r == '+'
		}) {
			if len(part) >= 2 {
				out[strings.ToLower(part)] = true
			}
		}
	}
	return out
}

// thirdPartyAssignmentPrefixes are the ways a line names whose job something is.
var thirdPartyAssignmentPrefixes = []string{"action item for", "action items for", "todo for", "to do for"}

// assignedToThirdParty reports whether a line explicitly assigns its obligation to
// someone who is NOT the user.
//
// The brief surfaced "*Action Item for Kim:* Please share the Ahrefs findings" as
// the USER's open loop. It is Kim's. A line that names its assignee has told us who
// owes it; if that is not the user, it is not the user's unfinished business, and
// FMB doctrine is explicit that every surfaced line must answer "what must the USER
// do or not-get-wrong" — never "what did somebody say to somebody else".
func assignedToThirdParty(text string, self map[string]bool) bool {
	lower := strings.ToLower(text)
	for _, prefix := range thirdPartyAssignmentPrefixes {
		idx := strings.Index(lower, prefix)
		if idx < 0 {
			continue
		}
		rest := strings.TrimLeft(lower[idx+len(prefix):], " \t*:")
		assignee := strings.FieldsFunc(rest, func(r rune) bool {
			return r < 'a' || r > 'z'
		})
		if len(assignee) == 0 {
			continue
		}
		if !self[assignee[0]] {
			return true
		}
	}
	return false
}

// meetingBriefIsTwoPartyExchange reports whether a Gmail message is business among
// THIS MEETING'S people, rather than a thread the user merely sits on.
//
// The sender gate alone is not enough. Gouri (an attendee) really did send "Is there
// a pdf export version from Ahrefs?", and Adit really was a recipient — so the sender
// gate passed it — but the mail also went to Beth, a client, and opened "Hi Beth".
// Gouri was asking BETH. Adit was a bystander on his parents' client thread, and the
// brief told him it was his obligation.
//
// The line is not "only two people". A thread among several people who are ALL in the
// meeting is still that meeting's business, and the sender decides attribution (a
// rule the brief already relies on). What breaks attribution is an OUTSIDER on the
// thread: once someone who is not in this meeting is being spoken to, the ask may
// well be aimed at them, and Mora does not guess — the same refusal it already makes
// outbound, where "a group record that cannot be assigned to exactly one attendee is
// dropped rather than attributed arbitrarily".
//
// Alias-safe via mailboxKey, so a dotted/plussed alias is still the same human.
func meetingBriefIsTwoPartyExchange(m Memory, self map[string]bool, attendees ...string) bool {
	inRoom := map[string]bool{}
	for addr := range self {
		inRoom[mailboxKey(addr)] = true
	}
	for _, a := range attendees {
		if key := mailboxKey(a); key != "" {
			inRoom[key] = true
		}
	}

	for _, field := range []string{"to", "cc", "bcc"} {
		for _, raw := range metaStrings(m.Meta[field]) {
			key := mailboxKey(strings.ToLower(strings.TrimSpace(raw)))
			if key == "" || inRoom[key] {
				continue
			}
			return false // an outsider was being spoken to; who owes what is unknowable
		}
	}
	return true
}

// classifyMeetingBriefEvidence performs P14's deterministic selection only.
// P15 replaces ordering, not these unfinished-business gates.
func classifyMeetingBriefEvidence(m Memory, cfg Config, at time.Time) string {
	// Machine-generated calendar/conferencing notifications are event plumbing, not
	// correspondence. Excluded from EVERY section before anything else looks at them.
	if isMeetingNotification(m) {
		return ""
	}
	// An obligation the text explicitly hands to someone else is not the user's.
	if assignedToThirdParty(signalText(m), selfNameTokens(selfEmails(cfg))) {
		return ""
	}
	// Bulk/marketing/transactional mail (every sender a service identity) is never
	// unfinished business with a person — exclude it from every section so a
	// co-recipient blast can't surface as an attendee's "open loop". Framing: the
	// brief is the obligations the USER owns, not noise their address co-occurs with.
	if memoryIsServiceOnly(m) {
		return ""
	}
	if userOwnedOpenLoop(m, cfg) {
		return meetingBriefOpenLoops
	}
	text := signalText(m)
	if containsAnyPhrase(text, unresolvedThreadPhrases) || endsInActionableQuestion(m) {
		return meetingBriefUnresolved
	}
	if containsAnyPhrase(text, stalenessGuardPhrases) {
		return meetingBriefStaleness
	}
	if materialSharedContext(m, text, at) {
		return meetingBriefSharedContext
	}
	return ""
}

var firstPersonCommitmentPhrases = []string{
	"i'll ", "i'd ", "i will ", "i owe ", "i need to ", "i should ", "i promised ",
	"let me ", "i can send", "i can share", "i can introduce", "i'll follow up",
	"i will follow up", "i'll get back", "i will get back",
}

var directRequestPhrases = []string{
	"can you ", "could you ", "would you ", "please send", "please share",
	"please review", "please confirm", "please sign", "please introduce",
	"please add",
	"need your approval", "needs your approval", "need your sign-off",
	"waiting for your", "get back to me", "when can you", "do you mind",
}

var unresolvedThreadPhrases = []string{
	"still waiting", "not decided", "haven't decided", "have not decided",
	"unresolved", "open question", "tbd", "pending", "blocked on",
	"need to decide", "decide whether", "circle back", "follow up",
	"follow-up", "next steps", "awaiting",
}

// stalenessGuardPhrases mark a fact that has CHANGED, so the user does not walk in
// and assert something now-wrong ("she's still at Acme", "you're still in Boston").
//
// They must denote an identity/role/location change, and nothing else. The bare verbs
// that used to live here — "joined ", "leaving ", "left " — matched everyday speech:
// "Good morning leaving now" (the user walking out the door) was rendered as a
// staleness guard about an attendee. A guard has to be about who someone now IS.
var stalenessGuardPhrases = []string{
	"moved to ", "moving to ", "relocated to ",
	"new role", "new title", "new job", "new company",
	"now at ", "no longer at", "no longer with", "formerly at", "formerly with",
	"changed roles", "changed companies", "changed jobs",
}

var materialContextPhrases = []string{
	"decision", "proposal", "contract", "pilot", "launch", "roadmap",
	"budget", "pricing", "fundraising", "funding", "hiring", "partnership",
	"introduction", "intro", "document", "deck", "review", "approval",
	"deadline", "next step", "project:", "milestone",
}

var personalTriviaPhrases = []string{
	"kid's name", "kids' names", "son's name", "daughter's name",
	"birthday", "favorite food", "favourite food", "favorite drink",
	"favourite drink", "hobby", "vacation", "spouse", "wife", "husband",
}

func userOwnedOpenLoop(m Memory, cfg Config) bool {
	text := signalText(m)
	if userAuthoredTask(m) {
		return true
	}
	if isIMessageMemory(m) {
		speaker, body := lastConversationLine(m.Text)
		if body == "" || personalTriviaOnly(body) {
			return false
		}
		if speaker == "me" {
			return containsAnyPhrase(strings.ToLower(body), firstPersonCommitmentPhrases) || strings.Contains(body, "?")
		}
		return containsAnyPhrase(strings.ToLower(body), directRequestPhrases) || strings.Contains(body, "?")
	}
	if isGmailMemory(m) {
		self := selfEmails(cfg)
		if len(self) == 0 {
			return false
		}
		senders := lowerStrings(metaStrings(m.Meta["from"]))
		recipients := append(lowerStrings(metaStrings(m.Meta["to"])), lowerStrings(metaStrings(m.Meta["cc"]))...)
		allSelf := len(senders) > 0
		anySelfSender := false
		for _, sender := range senders {
			if self[sender] {
				anySelfSender = true
			} else {
				allSelf = false
			}
		}
		toSelf := false
		for _, recipient := range recipients {
			if self[recipient] {
				toSelf = true
				break
			}
		}
		switch {
		case allSelf:
			return containsAnyPhrase(text, firstPersonCommitmentPhrases) || gmailActionableAsk(text)
		case !anySelfSender && toSelf:
			return containsAnyPhrase(text, directRequestPhrases) || gmailActionableAsk(text)
		default:
			return false
		}
	}
	if m.Provider == "" && (m.Source == "manual" || m.Source == "mcp") {
		return containsAnyPhrase(text, firstPersonCommitmentPhrases)
	}
	return false
}

func userAuthoredTask(m Memory) bool {
	if !strings.EqualFold(m.Type, "task") {
		return false
	}
	return m.Provider == "" || m.Source == "manual" || m.Source == "mcp"
}

func isIMessageMemory(m Memory) bool {
	return strings.EqualFold(m.Provider, "imessage") || strings.Contains(strings.ToLower(m.ProviderID), "imessage")
}

func isGmailMemory(m Memory) bool {
	return strings.EqualFold(m.Provider, "gmail") || strings.Contains(strings.ToLower(m.ProviderID), "gmail")
}

// relationalEvidenceIDs returns the dossier evidence ids this person is actually a
// PARTY to — reached by a relationship edge (PARTICIPATED_IN / EMAILED / ATTENDED /
// TEXTED ...) rather than a body-text MENTIONS edge.
//
// buildGraph emits MENTIONS only for a gazetteer name-hit on a memory the person is
// NOT a participant of (it skips anyone already covered by a participant edge), so a
// mention-only memory is by construction "a third party wrote this person's name" —
// e.g. a note reading "I spoke to Neil about the pilot; can you follow up?" is an ask
// owed to its AUTHOR, not to Neil. graphGetEntity pools every rel into one evidence
// list (correct for a dossier, which is about the person), but the brief is about the
// user's unfinished business WITH the person, so it must take only the relational
// slice. Attributing a mention is wrong-person attribution — severity-1.
func relationalEvidenceIDs(dossier map[string]any) map[string]bool {
	out := map[string]bool{}
	edges, _ := dossier["edges"].([]map[string]any)
	for _, e := range edges {
		if rel, _ := e["rel"].(string); rel == graphRelMentions {
			continue
		}
		if id, _ := e["evidence_id"].(string); id != "" {
			out[id] = true
		}
	}
	return out
}

func lowerStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(strings.TrimSpace(s)))
	}
	return out
}

func lastConversationLine(text string) (speaker, body string) {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "*") {
			continue
		}
		label, content, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(content) == "" {
			continue
		}
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "me" {
			return "me", strings.TrimSpace(content)
		}
		return "other", strings.TrimSpace(content)
	}
	return "", ""
}

// endsInActionableQuestion reports whether this memory carries a question the user
// actually has to answer.
//
// Gmail goes through the STRICT gate (a real interrogative opener or a direct
// request), the same one the open-loops path uses. It did not, and that gap is why
// "Need help?" — the footer of a Microsoft Teams invite — surfaced as an unresolved
// thread with an attendee. A bare "?" in email is far more often a CTA, a footer, or
// URL debris than an obligation. iMessage deliberately keeps the loose bare-"?" test:
// terse real conversation is exactly where "the deck?" is a genuine ask.
func endsInActionableQuestion(m Memory) bool {
	if isIMessageMemory(m) {
		_, question := lastConversationLine(m.Text)
		return actionableQuestion(question)
	}
	question := signalText(m)
	if isGmailMemory(m) {
		return gmailActionableAsk(question)
	}
	return actionableQuestion(question)
}

func actionableQuestion(text string) bool {
	return strings.Contains(text, "?") &&
		!personalTriviaOnly(text) &&
		!containsAnyPhrase(strings.ToLower(text), nonObligationQuestionPhrases)
}

// gmailActionableAsk is the stricter open-loop question gate for Gmail. Email —
// unlike iMessage — is where marketing and cold outreach live, so a bare "?" there
// is far more often a CTA or a co-recipient blast than an obligation the user owns.
// A Gmail "?" therefore counts only when it carries a genuine interrogative opener
// ("can you", "when", "how") or a direct request. iMessage keeps the looser bare-"?"
// test (real terse conversation), so this gate is Gmail-only by design.
func gmailActionableAsk(text string) bool {
	if !actionableQuestion(text) {
		return false
	}
	lower := strings.ToLower(text)
	return containsAnyPhrase(lower, interrogativeOpeners) || containsAnyPhrase(lower, directRequestPhrases)
}

// tokenIsNoise reports whether a single whitespace-delimited token is a URL, a URL
// path/encoded blob, or mostly non-letter punctuation — i.e. not a real word. Used
// to STRIP such tokens (not drop the whole segment), so a genuine ask that merely
// contains a link ("can you review <url>?") keeps a clean excerpt.
func tokenIsNoise(tok string) bool {
	lower := strings.ToLower(tok)
	for _, marker := range urlNoiseMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	// 2+ "letter/letter" slashes = a URL path ("com/calendar/event"), not a lone
	// slash pair ("A/B", "yes/no", "CA/NY") or a numeric date ("3/15").
	slashes := 0
	for i := 1; i+1 < len(tok); i++ {
		a, b := tok[i-1], tok[i+1]
		if tok[i] == '/' &&
			((a >= 'a' && a <= 'z') || (a >= 'A' && a <= 'Z')) &&
			((b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')) {
			slashes++
		}
	}
	if slashes >= 2 {
		return true
	}
	// A real word is mostly letters; a URL-encoded or address blob
	// (",+Dublin,+CA+94568?") is mostly "+", "%", digits, and punctuation.
	letters := 0
	for _, r := range tok {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			letters++
		}
	}
	total := len([]rune(tok))
	return total >= 8 && letters*2 < total
}

// stripNoiseTokens removes URL / path / encoded tokens from a text segment, keeping
// the prose. A pure URL shard ("com/maps/search/...+United+States?") collapses to
// "", while a genuine ask that merely contains a link keeps its words — so the URL
// noise is removed without dropping the obligation. Applied per-segment ONLY.
func stripNoiseTokens(text string) string {
	kept := make([]string, 0, 8)
	for _, tok := range strings.Fields(text) {
		if !tokenIsNoise(tok) {
			kept = append(kept, tok)
		}
	}
	return strings.Join(kept, " ")
}

var urlNoiseMarkers = []string{
	"http://", "https://", "://", "www.", ".com/", ".org/", ".net/",
	"/search", "/maps", "/url?", "%0a", "%20", "%2f", "utm_",
}

// interrogativeOpeners mark a "?" as a real question aimed at a person rather than a
// marketing hook; a Gmail open loop requires one of these (or a direct request).
var interrogativeOpeners = []string{
	"who ", "what ", "when ", "where ", "why ", "how ", "which ", "whose ",
	"is there", "are there", "is it", "do we", "can you", "could you",
	"would you", "will you", "do you", "did you", "are you", "have you",
	"should we", "should i", "any chance", "when can", "let me know if",
}

// nonObligationQuestionPhrases are bulk/marketing/transactional "questions" that end
// in "?" yet are never a user obligation — order confirmations, surveys, CTAs.
var nonObligationQuestionPhrases = []string{
	"questions about your order", "any questions", "how did we do",
	"how are we doing", "rate your", "your feedback", "give feedback",
	"leave a review", "take our survey", "was this helpful",
	"view in browser", "unsubscribe", "manage your subscription",
}

func personalTriviaOnly(text string) bool {
	lower := strings.ToLower(text)
	return containsAnyPhrase(lower, personalTriviaPhrases) && !containsAnyPhrase(lower, materialContextPhrases)
}

func materialSharedContext(m Memory, text string, at time.Time) bool {
	if personalTriviaOnly(text) || !containsAnyPhrase(text, materialContextPhrases) {
		return false
	}
	ts := itemOccurredAt(m)
	return ts.IsZero() || !ts.After(at)
}

func signalText(m Memory) string {
	return strings.ToLower(oneLine(m.Title + " " + stripFromLine(m.Text)))
}

// containsAnyPhrase reports whether text contains any phrase AT A WORD BOUNDARY.
//
// A raw substring test is not good enough here, and the difference is not academic:
// "pending" matched "de-pending-on", so a vendor's price quote ("proper SEM setup
// will cost $2,000+ depending on how many campaigns") was filed as an unresolved
// decision on the strength of a word that wasn't there. Same class of error would
// let "intro" match "introduction" and "left" match "leftover".
func containsAnyPhrase(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if containsPhrase(text, phrase) {
			return true
		}
	}
	return false
}

func containsPhrase(text, phrase string) bool {
	if phrase == "" {
		return false
	}
	for start := 0; start < len(text); {
		i := strings.Index(text[start:], phrase)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(phrase)
		// A phrase that already begins/ends with a space or punctuation carries its
		// own boundary; only check the side that ends in a word character.
		okBefore := i == 0 || !isWordByte(text[i-1]) || !isWordByte(phrase[0])
		okAfter := end == len(text) || !isWordByte(text[end]) || !isWordByte(phrase[len(phrase)-1])
		if okBefore && okAfter {
			return true
		}
		start = i + 1
	}
	return false
}

func renderMeetingBrief(w io.Writer, brief MeetingBrief) error {
	if err := brief.validate(); err != nil {
		return fmt.Errorf("refusing to render uncited meeting brief: %w", err)
	}
	// The red health banner (HEALTH-02) renders FIRST, unconditionally — even
	// when there's no next meeting (brief.Event == nil below): a broken source
	// is worth surfacing regardless of whether there happens to be an upcoming
	// event. Pure over the pre-built brief.SourceHealth (no cfg/now at render
	// time — D-03).
	if banner := healthBannerFrom(Health{Sources: brief.SourceHealth, Index: brief.idxHealth, Producers: brief.producerHealth}); banner != "" {
		fmt.Fprintln(w, banner)
		fmt.Fprintln(w)
	}
	if brief.Event == nil {
		return nil
	}
	fmt.Fprintln(w, "# Meeting brief")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "- %s — %s", brief.Event.Title, brief.Event.StartsAt)
	if len(brief.Event.Attendees) > 0 {
		fmt.Fprintf(w, " — attendees: %s", strings.Join(brief.Event.Attendees, ", "))
	}
	fmt.Fprintf(w, " %s\n", renderBriefCitation(brief.Event.Citation))
	for _, section := range brief.Sections {
		fmt.Fprintf(w, "\n## %s\n", section.Title)
		for _, line := range section.Lines {
			fmt.Fprintf(w, "- %s %s\n", line.Text, renderBriefCitation(line.Citation))
			fmt.Fprintf(w, "  actions: correct=`%s` unlink=`%s`\n", line.Correction.CorrectCommand, line.Correction.UnlinkCommand)
		}
	}
	return nil
}

func renderBriefCitation(c BriefCitation) string {
	return fmt.Sprintf("{memory-id: %s, channel: %s, source: %s, date: %s}", c.MemoryID(), c.Channel(), c.Source(), c.Date())
}
