package mora

import (
	"context"
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
	meetingBriefDefaultPerGuest = 8
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
	MemoryID string `json:"memory_id"`
	Channel  string `json:"channel"`
	Source   string `json:"source"`
	Date     string `json:"date"`
}

func (c BriefCitation) validate() error {
	if strings.TrimSpace(c.MemoryID) == "" {
		return errors.New("missing memory_id")
	}
	if strings.TrimSpace(c.Channel) == "" {
		return errors.New("missing channel")
	}
	if strings.TrimSpace(c.Source) == "" {
		return errors.New("missing source")
	}
	if strings.TrimSpace(c.Date) == "" {
		return errors.New("missing date")
	}
	if _, err := time.Parse(time.RFC3339, c.Date); err != nil {
		return fmt.Errorf("invalid date %q: %w", c.Date, err)
	}
	return nil
}

// CitedBriefLine is the only renderable evidence atom. Text is a compact extract
// from the cited memory, never an inferred conclusion.
type CitedBriefLine struct {
	Text     string        `json:"text"`
	Attendee string        `json:"attendee,omitempty"`
	Citation BriefCitation `json:"citation"`
}

func newCitedBriefLine(text, attendee string, citation BriefCitation) (CitedBriefLine, error) {
	line := CitedBriefLine{
		Text:     oneLine(text),
		Attendee: strings.TrimSpace(attendee),
		Citation: citation,
	}
	if line.Text == "" {
		return CitedBriefLine{}, errors.New("empty cited line")
	}
	if err := line.Citation.validate(); err != nil {
		return CitedBriefLine{}, err
	}
	return line, nil
}

func (l CitedBriefLine) validate() error {
	if strings.TrimSpace(l.Text) == "" {
		return errors.New("empty text")
	}
	return l.Citation.validate()
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
	if _, err := time.Parse(time.RFC3339, b.AsOf); err != nil {
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
		if meetingBriefSectionTitles[section.Kind] != section.Title {
			return fmt.Errorf("unknown meeting brief section %q", section.Kind)
		}
		if len(section.Lines) == 0 {
			return fmt.Errorf("meeting brief section %q is empty", section.Kind)
		}
		for i, line := range section.Lines {
			if err := line.validate(); err != nil {
				return fmt.Errorf("%s line %d is uncited: %w", section.Kind, i, err)
			}
		}
	}
	return nil
}

type meetingBriefCandidate struct {
	kind string
	line CitedBriefLine
}

// buildEventMeetingBrief resolves one calendar memory by stable id, then assembles
// an unfinished-business brief over each exact attendee identity. Every attendee's
// candidate pool comes from Track B's budgeted cited get_entity projection.
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
	mems, err := meetingBriefMemories(cfg)
	if err != nil {
		return MeetingBrief{}, err
	}
	memoryByID := make(map[string]Memory, len(mems))
	for _, m := range mems {
		memoryByID[m.ID] = m
	}
	start, ok := eventStart(eventMemory)
	if !ok {
		return MeetingBrief{}, fmt.Errorf("calendar event %q has no valid RFC3339 start", eventMemory.ID)
	}
	if perAttendee <= 0 {
		perAttendee = meetingBriefDefaultPerGuest
	}

	attendees := meetingBriefAttendees(eventMemory, selfEmails(cfg))
	eventCitation := citationForMemory(eventMemory, eventMemory.ID, validFromOf(eventMemory))
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

	tokenBudget, _ := resolveContextBudgetTokens(cfg, maxTokens)
	perDossierTokens := tokenBudget
	if len(attendees) > 0 {
		perDossierTokens = tokenBudget / len(attendees)
		if perDossierTokens < 200 {
			perDossierTokens = 200
		}
	}

	candidates := make([]meetingBriefCandidate, 0)
	for _, attendee := range attendees {
		dossier, derr := entityDossierForMCP(ctx, cfg, attendee.identity, perDossierTokens)
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
		evidence, _ := dossier["evidence"].([]EntityEvidence)
		kept := 0
		for _, cited := range evidence {
			if kept >= perAttendee {
				break
			}
			if cited.ID == eventMemory.ID {
				continue
			}
			m, found := memoryByID[cited.ID]
			if !found || m.DeletedAt != "" {
				continue
			}
			if ts, terr := time.Parse(time.RFC3339, cited.CreatedAt); terr != nil || ts.After(at) {
				continue
			}
			kind := classifyMeetingBriefEvidence(m, cfg, at)
			if kind == "" {
				continue
			}
			line, lerr := newCitedBriefLine(
				meetingBriefEvidenceText(cited),
				display,
				citationForMemory(m, cited.Source, cited.CreatedAt),
			)
			if lerr != nil {
				return MeetingBrief{}, fmt.Errorf("event %s attendee %s evidence %s: %w", eventMemory.ID, attendee.identity, cited.ID, lerr)
			}
			candidates = append(candidates, meetingBriefCandidate{kind: kind, line: line})
			kept++
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		pi := meetingBriefKindRank(candidates[i].kind)
		pj := meetingBriefKindRank(candidates[j].kind)
		if pi != pj {
			return pi < pj
		}
		if candidates[i].line.Citation.Date != candidates[j].line.Citation.Date {
			return candidates[i].line.Citation.Date > candidates[j].line.Citation.Date
		}
		if candidates[i].line.Citation.MemoryID != candidates[j].line.Citation.MemoryID {
			return candidates[i].line.Citation.MemoryID < candidates[j].line.Citation.MemoryID
		}
		return candidates[i].line.Attendee < candidates[j].line.Attendee
	})
	if len(candidates) > meetingPrepEvidenceCap {
		candidates = candidates[:meetingPrepEvidenceCap]
	}

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
	if err := brief.validate(); err != nil {
		return MeetingBrief{}, err
	}
	return brief, nil
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

func meetingBriefKindRank(kind string) int {
	for i, candidate := range meetingBriefSectionOrder {
		if candidate == kind {
			return i
		}
	}
	return len(meetingBriefSectionOrder)
}

func citationForMemory(m Memory, source, date string) BriefCitation {
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
	return BriefCitation{
		MemoryID: m.ID,
		Channel:  channel,
		Source:   source,
		Date:     strings.TrimSpace(date),
	}
}

func meetingBriefEvidenceText(e EntityEvidence) string {
	title := oneLine(e.Title)
	snippet := oneLine(e.Snippet)
	switch {
	case title != "" && snippet != "":
		return truncateRunes(title+" — "+snippet, 360)
	case title != "":
		return title
	default:
		return truncateRunes(snippet, 360)
	}
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
			if line.Attendee != "" {
				fmt.Fprintf(w, "- %s: %s %s\n", line.Attendee, line.Text, renderBriefCitation(line.Citation))
			} else {
				fmt.Fprintf(w, "- %s %s\n", line.Text, renderBriefCitation(line.Citation))
			}
		}
	}
	return nil
}

func renderBriefCitation(c BriefCitation) string {
	return fmt.Sprintf("{memory-id: %s, channel: %s, source: %s, date: %s}", c.MemoryID, c.Channel, c.Source, c.Date)
}
