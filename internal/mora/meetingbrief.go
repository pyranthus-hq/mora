package mora

import (
	"context"
	"errors"
	"fmt"
	evidencepkg "github.com/pyranthus-hq/mora/internal/evidence"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	meetingpkg "github.com/pyranthus-hq/mora/internal/meeting"
	saliencepkg "github.com/pyranthus-hq/mora/internal/salience"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	meetingBriefOpenLoops       = meetingpkg.OpenLoops
	meetingBriefUnresolved      = meetingpkg.Unresolved
	meetingBriefStaleness       = meetingpkg.Staleness
	meetingBriefSharedContext   = meetingpkg.SharedContext
	meetingBriefDefaultPerGuest = 3
)

var meetingBriefSectionTitles = meetingpkg.SectionTitles

type BriefCitation = meetingpkg.Citation
type BriefLineCorrection = meetingpkg.LineCorrection
type CitedBriefLine = meetingpkg.CitedLine
type CitedMeetingEvent = meetingpkg.CitedEvent
type MeetingBriefSection = meetingpkg.BriefSection
type MeetingBrief = meetingpkg.Brief

func newBriefCitation(memoryID, channel, source, date string) (BriefCitation, error) {
	return meetingpkg.NewCitation(memoryID, channel, source, date)
}
func newBriefLineCorrection(stableAtom, attendeeAtom govAtom) (BriefLineCorrection, error) {
	return meetingpkg.NewLineCorrection(stableAtom, attendeeAtom)
}
func newCitedBriefLine(text, attendee string, citation BriefCitation, correction BriefLineCorrection, asOf time.Time) (CitedBriefLine, error) {
	return meetingpkg.NewCitedLine(text, attendee, citation, correction, asOf)
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
		IndexHealth:    hSnap.Index,
		ProducerHealth: hSnap.Producers,
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
		IndexHealth:    hSnap.Index,
		ProducerHealth: hSnap.Producers,
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
			messageCount := saliencepkg.MessageCount(m)
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
	if err := brief.Validate(); err != nil {
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
	items := make([]meetingpkg.SectionCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		items = append(items, meetingpkg.SectionCandidate{Kind: candidate.kind, Line: candidate.line})
	}
	return meetingpkg.WithCandidates(brief, items)
}

func meetingBriefResolveAttribution(associations []meetingBriefCandidate, lineDecisions map[string]string) (meetingBriefCandidate, bool) {
	items := make([]meetingpkg.AttributionAssociation, 0, len(associations))
	for _, candidate := range associations {
		items = append(items, meetingpkg.AttributionAssociation{DecisionKey: candidate.decisionKey, PersonID: candidate.rank.PersonID, AttendeeSender: candidate.attendeeSender})
	}
	index, ok := meetingpkg.ResolveAttribution(items, lineDecisions, mergeDecisionConfirm, mergeDecisionReject)
	if !ok {
		return meetingBriefCandidate{}, false
	}
	return associations[index], true
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

// meetingBriefHistoricalPrefix stamps every line with its age and the person it
// concerns. The DATED framing is the P15 invariant — a fact from ten months ago must
// never read as true now — and it is load-bearing, not decoration.
//
// The wording is deliberately "· <person> —" rather than "<person> wrote:": the
// memory is one this person is INVOLVED in, and Mora does not always know they
// authored it. Claiming authorship it cannot prove would be its own wrong-person bug.
// What it can say honestly is when, who it involves, and the exact words.

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

func meetingBriefLineCount(brief MeetingBrief) int { return meetingpkg.LineCount(brief) }

func citationForMemory(m Memory, source, date string) (BriefCitation, error) {
	return evidencepkg.ForMemory(m, source, date)
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
			return genericutil.TruncateRunes(oneLine(segment), 360)
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
		return genericutil.TruncateRunes(title, 360)
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
func meetingBriefEvidenceSegments(text string) []string { return meetingpkg.EvidenceSegments(text) }

// quotedBlockMarkers open a region of a mail body that the SENDER DID NOT WRITE: a
// forwarded message, a quoted reply chain, a signature, or a legal disclaimer.
// Everything from the first such marker onward belongs to someone else (or to
// nobody), and attributing it to the sender is how "Fwd: Ai / AEO" — Gouri
// forwarding a marketing mail — put a stranger's CTA ("Open to see how the loop
// works?") into the brief as Gouri's unfinished business with the user.

// quotedReplyLine matches the "On <date>, <someone> wrote:" attribution line that
// opens a quoted reply chain, in the forms Gmail/Outlook actually emit.

// signatureDelimiter is the RFC 3676 signature separator ("-- ") plus the bare "--"
// that most clients emit in practice.

// senderAuthoredBody returns only the prose the sender actually wrote: the body up to
// the first forwarded block, quoted reply, signature, or legal disclaimer. Quoted
// lines (">") are dropped wherever they appear.
//
// Without this, every reply chain re-litigates its own history and every forward puts
// a stranger's words in the sender's mouth — and the brief attributes both to the
// attendee as things they are waiting on the user for.
func senderAuthoredBody(text string) string { return meetingpkg.SenderAuthoredBody(text) }

// speakerPrefix matches the transcript label an iMessage line carries ("Me: ",
// "Gouri Karode: "). It is scaffolding the renderer added, not words anybody spoke.

// stripSpeakerPrefix removes a leading transcript speaker label from a line.
func stripSpeakerPrefix(text string) string { return meetingpkg.StripSpeakerPrefix(text) }

// isForwardedSubject reports whether a subject line marks the mail as a forward.
// "Re:" is deliberately NOT included: a reply is still the sender writing to you.
func isForwardedSubject(text string) bool { return meetingpkg.IsForwardedSubject(text) }

// isLeadInFragment reports whether a sentence merely ANNOUNCES content instead of
// carrying it. "Based on our conversation, here are the next steps and deliverables:"
// ends in a colon and states nothing the user must do — it points at a list that the
// sentence splitter then threw away. A brief line has to stand on its own.
func isLeadInFragment(text string) bool { return meetingpkg.IsLeadInFragment(text) }

// urlPattern matches a URL while it is still whole: an explicit scheme, a www.
// host, or a bare host-with-path ("meet.google.com/ctk-rdtz-jnx?hs=224",
// "aka.ms/JoinTeamsMeeting?omkt=en-US") — the shape that survives a plain-text
// email once the mail client has dropped the angle brackets.

// stripURLs removes whole URLs from a body before it is segmented, so URL debris
// can never be mistaken for prose. It runs on the RAW text — after segmentation the
// pieces no longer look like URLs, which is exactly how the shards got through.

// unwrapHardWraps joins a line to the next when the line did not actually end a
// sentence and the next line continues it (a lowercase or digit start). Plain-text
// email is hard-wrapped at a fixed column, so those newlines are typography, not
// punctuation. Blank lines, and lines that end in real terminators, still break.
//
// The lowercase-continuation test is deliberately conservative: it never merges two
// iMessage turns, because a new turn starts with a capitalized speaker name.

// continuesSentence reports whether next is the wrapped remainder of line.

func containsPersonalTrivia(text string) bool { return meetingpkg.ContainsPersonalTrivia(text) }

func oneLine(text string) string { return meetingpkg.OneLine(text) }

// meetingNotificationSubjects are the Google Calendar RSVP/invite subject prefixes.
// These mails are EVENT PLUMBING, machine-generated by the calendar server — nobody
// wrote them to the user.

// meetingNotificationBodyMarkers are boilerplate blocks emitted by conferencing
// tools. Their presence means the body is join-instructions, not correspondence.

// isMeetingNotification reports whether a mail is a machine-generated calendar /
// conferencing notification rather than something a human wrote to the user.
//
// These were being mined for evidence, and they are where the gibberish came from:
// a "Declined: Sync up meeting" RSVP notice contributed the Google Meet URL that got
// shredded into "com/ctk-rdtz-jnx?", and a Microsoft Teams invite contributed
// "Need help?" — its own support-link footer — as an unresolved thread with an
// attendee. There is no unfinished business inside an RSVP receipt.

// selfNameTokens derives the user's own name tokens from the addresses Mora already
// knows (source mailboxes + declared self_emails), so the third-party check has
// something to compare an explicit assignee against. Only the LOCAL part is used —
// the domain is a company, not a person.

// thirdPartyAssignmentPrefixes are the ways a line names whose job something is.

// assignedToThirdParty reports whether a line explicitly assigns its obligation to
// someone who is NOT the user.
//
// The brief surfaced "*Action Item for Kim:* Please share the Ahrefs findings" as
// the USER's open loop. It is Kim's. A line that names its assignee has told us who
// owes it; if that is not the user, it is not the user's unfinished business, and
// FMB doctrine is explicit that every surfaced line must answer "what must the USER
// do or not-get-wrong" — never "what did somebody say to somebody else".

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
	return meetingpkg.IsTwoPartyExchange(m, self, attendees...)
}

// classifyMeetingBriefEvidence performs P14's deterministic selection only.
// P15 replaces ordering, not these unfinished-business gates.
func classifyMeetingBriefEvidence(m Memory, cfg Config, at time.Time) string {
	return meetingpkg.ClassifyEvidence(meetingpkg.ClassifierInput{Memory: m, SignalText: signalText(m), Self: selfEmails(cfg), OccurredAt: itemOccurredAt(m), At: at, ServiceOnly: memoryIsServiceOnly(m)})
}

func isIMessageMemory(m Memory) bool { return meetingpkg.IsIMessage(m) }

func isGmailMemory(m Memory) bool { return meetingpkg.IsGmail(m) }

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

// tokenIsNoise reports whether a single whitespace-delimited token is a URL, a URL
// path/encoded blob, or mostly non-letter punctuation — i.e. not a real word. Used
// to STRIP such tokens (not drop the whole segment), so a genuine ask that merely
// contains a link ("can you review <url>?") keeps a clean excerpt.

// stripNoiseTokens removes URL / path / encoded tokens from a text segment, keeping
// the prose. A pure URL shard ("com/maps/search/...+United+States?") collapses to
// "", while a genuine ask that merely contains a link keeps its words — so the URL
// noise is removed without dropping the obligation. Applied per-segment ONLY.
func stripNoiseTokens(text string) string { return meetingpkg.StripNoiseTokens(text) }

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

func renderMeetingBrief(w io.Writer, brief MeetingBrief) error {
	banner := healthBannerFrom(Health{Sources: brief.SourceHealth, Index: brief.IndexHealth, Producers: brief.ProducerHealth})
	return meetingpkg.Render(w, brief, banner)
}
