package mora

import (
	meetingpkg "github.com/pyranthus-hq/mora/internal/meeting"
	"time"
)

const (
	meetingPrepEvidenceCap = 24
	meetingPrepHorizonDays = meetingpkg.DefaultHorizonDays
)

var prepClock = time.Now

// MeetingGaps is the deterministic "what the vault does NOT know" analysis.
type MeetingGaps struct {
	UnknownAttendees []string `json:"unknown_attendees,omitempty"`
	ThinAttendees    []string `json:"thin_attendees,omitempty"`
	NoEvidence       []string `json:"no_evidence,omitempty"`
	NoAttendees      bool     `json:"no_attendees,omitempty"`
	SelfUnknown      bool     `json:"self_unknown,omitempty"`
}

var meetingPrepGrace = meetingpkg.DefaultGrace

type MeetingEvent = meetingpkg.Event

func eventStart(m Memory) (time.Time, bool) { return meetingpkg.EventStart(m) }
func selectNextEvent(mems []Memory, now time.Time, attendeeFilterIDs map[string]bool) *MeetingEvent {
	return meetingpkg.SelectNextEvent(mems, now, attendeeFilterIDs, memoryMentionsEntity, meetingPrepGrace, meetingPrepHorizonDays)
}
