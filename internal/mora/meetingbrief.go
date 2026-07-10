package mora

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Text       string              `json:"text"`
	Attendee   string              `json:"attendee,omitempty"`
	Citation   BriefCitation       `json:"citation"`
	Correction BriefLineCorrection `json:"correction"`
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
	return l.Correction.validate()
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
	AsOf        string                `json:"as_of"`
	Event       *CitedMeetingEvent    `json:"event"`
	Sections    []MeetingBriefSection `json:"sections"`
	EgressCalls int                   `json:"egress_calls"`
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
	empty := MeetingBrief{
		AsOf:        at.UTC().Format(time.RFC3339),
		Sections:    []MeetingBriefSection{},
		EgressCalls: 0,
	}
	mems, err := meetingBriefMemories(cfg)
	if err != nil {
		return MeetingBrief{}, err
	}
	event := selectNextEvent(mems, at, attendeeFilterIDs)
	if event == nil && len(attendeeFilterIDs) > 0 {
		event = selectNextEvent(mems, at, nil)
	}
	if event == nil {
		return empty, nil
	}
	for _, m := range mems {
		if m.ID == event.StableID {
			return buildMeetingBriefFromEvent(ctx, cfg, m, at, maxTokens, perAttendee)
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
	if len(metaStrings(eventMemory.Meta["attendees"])) > 0 && len(self) == 0 {
		return MeetingBrief{}, errors.New("cannot safely resolve meeting attendees: self email is unknown; connect a Google account first")
	}
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
	brief := MeetingBrief{
		AsOf:        at.UTC().Format(time.RFC3339),
		Event:       event,
		Sections:    []MeetingBriefSection{},
		EgressCalls: 0,
	}
	_, budgetChars := resolveContextBudgetTokens(cfg, maxTokens)
	payloadBudget := budgetChars / mcpDigestEnvelopeDivisor
	if baseSize := jsonLen(brief); baseSize > payloadBudget {
		return MeetingBrief{}, fmt.Errorf("meeting brief event requires %d compact bytes, exceeding the %d-byte max_tokens budget", baseSize, payloadBudget)
	}

	associationsByMemory := map[string][]meetingBriefCandidate{}
	governanceLedger, err := loadGovernance(cfg)
	if err != nil {
		return MeetingBrief{}, err
	}
	lineDecisions := governanceLedger.briefLineDecisions()
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
		evidence, _ := dossier["memories"].([]Memory)
		personLastSeen := latestMeetingBriefEvidenceDate(evidence, at)
		for _, m := range evidence {
			if m.ID == eventMemory.ID {
				continue
			}
			if m.DeletedAt != "" {
				continue
			}
			occurredAt := validFromOf(m)
			if ts, terr := time.Parse(time.RFC3339, occurredAt); terr != nil || ts.After(at) {
				continue
			}
			kind := classifyMeetingBriefEvidence(m, cfg, at)
			if kind == "" {
				continue
			}
			source := evidenceSource(m)
			excerpt := meetingBriefActionableEvidenceText(m, cfg, at, kind)
			if excerpt == "" {
				continue
			}
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

func meetingBriefHistoricalPrefix(asOf time.Time, date, attendee string) string {
	factAt, err := time.Parse(time.RFC3339, date)
	if err != nil {
		return ""
	}
	age := meetingBriefRelativeAge(asOf, factAt)
	attendee = strings.TrimSpace(attendee)
	if attendee == "" {
		return age + ", the cited record stated: "
	}
	return fmt.Sprintf("%s, the cited record involving %s stated: ", age, attendee)
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

func meetingBriefAttendees(event Memory, self map[string]bool) []meetingBriefAttendee {
	names := metaNames(event.Meta["names"])
	seen := map[string]bool{}
	var out []meetingBriefAttendee
	for _, raw := range metaStrings(event.Meta["attendees"]) {
		identity := strings.ToLower(strings.TrimSpace(raw))
		if identity == "" || self[identity] || seen[identity] || isStructuralNoise(identity) {
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
	for _, path := range files {
		m, perr := parseMemory(path)
		if perr != nil || m.DeletedAt != "" {
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
	for _, segment := range meetingBriefEvidenceSegments(stripFromLine(m.Text)) {
		probe := m
		probe.Title = ""
		probe.Text = segment
		if !containsPersonalTrivia(segment) && classifyMeetingBriefEvidence(probe, cfg, at) == kind {
			return truncateRunes(oneLine(segment), 360)
		}
	}
	title := oneLine(m.Title)
	if title == "" || containsPersonalTrivia(title) {
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

func meetingBriefEvidenceSegments(text string) []string {
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

func containsPersonalTrivia(text string) bool {
	return containsAnyPhrase(strings.ToLower(text), personalTriviaPhrases)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// classifyMeetingBriefEvidence performs P14's deterministic selection only.
// P15 replaces ordering, not these unfinished-business gates.
func classifyMeetingBriefEvidence(m Memory, cfg Config, at time.Time) string {
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
	"i'll ", "i will ", "i owe ", "i need to ", "i should ", "i promised ",
	"let me ", "i can send", "i can share", "i can introduce", "i'll follow up",
	"i will follow up", "i'll get back", "i will get back",
}

var directRequestPhrases = []string{
	"can you ", "could you ", "would you ", "please send", "please share",
	"please review", "please confirm", "please sign", "please introduce",
	"need your approval", "needs your approval", "need your sign-off",
	"waiting for your", "get back to me", "when can you", "do you mind",
}

var unresolvedThreadPhrases = []string{
	"still waiting", "not decided", "haven't decided", "have not decided",
	"unresolved", "open question", "tbd", "pending", "blocked on",
	"need to decide", "decide whether", "circle back", "follow up",
	"follow-up", "next steps", "awaiting",
}

var stalenessGuardPhrases = []string{
	"moved to ", "moving to ", "joined ", "leaving ", "left ",
	"new role", "new title", "now at ", "no longer ", "formerly ",
	"changed roles", "changed companies", "renamed ", "rescheduled",
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
			return containsAnyPhrase(text, firstPersonCommitmentPhrases) || actionableQuestion(text)
		case !anySelfSender && toSelf:
			return containsAnyPhrase(text, directRequestPhrases) || actionableQuestion(text)
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

func endsInActionableQuestion(m Memory) bool {
	var question string
	if isIMessageMemory(m) {
		_, question = lastConversationLine(m.Text)
	} else {
		question = signalText(m)
	}
	return actionableQuestion(question)
}

func actionableQuestion(text string) bool {
	return strings.Contains(text, "?") && !personalTriviaOnly(text)
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

func containsAnyPhrase(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func renderMeetingBrief(w io.Writer, brief MeetingBrief) error {
	if err := brief.validate(); err != nil {
		return fmt.Errorf("refusing to render uncited meeting brief: %w", err)
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
