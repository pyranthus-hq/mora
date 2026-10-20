package mora

import (
	"context"
	"flag"
	"io"
	"net/mail"
	"strings"
	"time"
)

const companionEventsUsage = "usage: mora companion events [--days <int>] --json"

type companionEventsPayload struct {
	Events []CompanionEvent `json:"events"`
}

type CompanionEvent struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Start        string   `json:"start"`
	Text         string   `json:"text"`
	MeetURL      string   `json:"meet_url"`
	Attendees    []string `json:"attendees"`
	Location     string   `json:"location"`
	Calendar     string   `json:"calendar"`
	HasExternal  bool     `json:"has_external"`
	DossierQuery string   `json:"dossier_query"`
}

func cmdCompanionEvents(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("companion events", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	days := fs.Int("days", 7, "number of days ahead to search (1 to 90)")
	jsonOut := fs.Bool("json", false, "emit JSON (required)")
	if err := fs.Parse(args); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"%s (unexpected argument %q)", companionEventsUsage, fs.Arg(0))
	}
	if !*jsonOut {
		return newCodedError(errCodeUsageMissingArgument, nil,
			"%s (--json is required)", companionEventsUsage)
	}
	if *days < 1 || *days > 90 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"%s (invalid --days %d: must be between 1 and 90)", companionEventsUsage, *days)
	}

	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}

	now := cfg.OperationClock()
	if now.IsZero() {
		now = briefClock()
	}
	start := now.Add(-2 * time.Hour)
	end := now.AddDate(0, 0, *days)

	memories, err := calendarEventsInRange(cfg, start, end, "")
	if err != nil {
		return err
	}

	selfSet := make(map[string]bool)
	for _, e := range cfg.SelfEmails {
		if s := strings.ToLower(strings.TrimSpace(e)); s != "" {
			selfSet[s] = true
		}
	}
	for e := range selfEmails(cfg) {
		selfSet[e] = true
	}

	events := make([]CompanionEvent, 0, len(memories))
	for _, m := range memories {
		ev := parseCompanionEvent(m, cfg, selfSet)
		if keepCompanionMeeting(ev) {
			events = append(events, ev)
		}
	}

	return emitReceipt(stdout, schemaCompanionEvents, companionSchemaVersion, companionEventsPayload{Events: dedupeCompanionMeetings(events)})
}

// keepCompanionMeeting is the Today-agenda gate: holidays, sports feeds, and
// all-day subscribed noise are calendar facts, not meetings. A meeting has a
// conferencing URL or at least one attendee.
func keepCompanionMeeting(ev CompanionEvent) bool {
	if companionCalendarIsNoise(ev.Calendar) {
		return false
	}
	if companionLooksLikeSports(ev) {
		return false
	}
	if ev.MeetURL != "" {
		return true
	}
	return len(ev.Attendees) > 0
}

func companionCalendarIsNoise(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	if strings.Contains(n, "holiday") {
		return true
	}
	switch n {
	case "chelsea", "fotmob":
		return true
	}
	return false
}

func companionLooksLikeSports(ev CompanionEvent) bool {
	cal := strings.ToLower(strings.TrimSpace(ev.Calendar))
	if cal == "chelsea" || strings.Contains(cal, "fotmob") {
		return true
	}
	blob := strings.ToLower(ev.Title + " " + ev.Text)
	return strings.Contains(blob, "premier league") || strings.Contains(blob, "fotmob.com")
}

func companionNormalizeTitle(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x1F300 && r <= 0x1FAFF {
			continue
		}
		if r >= 0x2600 && r <= 0x27BF {
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(b.String()))), " ")
}

func companionMeetingKey(ev CompanionEvent) string {
	start := strings.TrimSpace(ev.Start)
	if t, err := time.Parse(time.RFC3339, start); err == nil {
		start = t.UTC().Truncate(time.Minute).Format(time.RFC3339)
	} else if len(start) >= 16 {
		start = start[:16]
	}
	return companionNormalizeTitle(ev.Title) + "\x00" + start
}

func companionMeetingBetter(a, b CompanionEvent) bool {
	aMeet := a.MeetURL != ""
	bMeet := b.MeetURL != ""
	if aMeet != bMeet {
		return aMeet
	}
	if len(a.Attendees) != len(b.Attendees) {
		return len(a.Attendees) > len(b.Attendees)
	}
	return a.ID < b.ID
}

// dedupeCompanionMeetings collapses Apple Calendar + Google Calendar copies of
// the same timed meeting, keeping the copy with a meet URL or more attendees.
func dedupeCompanionMeetings(events []CompanionEvent) []CompanionEvent {
	if len(events) < 2 {
		return events
	}
	indexByKey := make(map[string]int, len(events))
	out := make([]CompanionEvent, 0, len(events))
	for _, ev := range events {
		key := companionMeetingKey(ev)
		if i, ok := indexByKey[key]; ok {
			if companionMeetingBetter(ev, out[i]) {
				out[i] = ev
			}
			continue
		}
		indexByKey[key] = len(out)
		out = append(out, ev)
	}
	return out
}

func parseCompanionEvent(m Memory, cfg Config, selfSet map[string]bool) CompanionEvent {
	ev := CompanionEvent{
		ID:        m.ID,
		Title:     m.Title,
		Text:      m.Text,
		Attendees: []string{},
	}
	if s, ok := eventStartOf(m); ok && s != "" {
		ev.Start = s
	} else {
		ev.Start = m.CreatedAt
	}

	for _, line := range strings.Split(m.Text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Attendees:") {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "Attendees:"))
			if rest != "" {
				for _, p := range strings.Split(rest, ",") {
					p = strings.TrimSpace(p)
					if p != "" {
						ev.Attendees = append(ev.Attendees, p)
					}
				}
			}
		} else if strings.HasPrefix(trimmed, "Calendar:") {
			ev.Calendar = strings.TrimSpace(strings.TrimPrefix(trimmed, "Calendar:"))
		} else if strings.HasPrefix(trimmed, "Location:") {
			ev.Location = strings.TrimSpace(strings.TrimPrefix(trimmed, "Location:"))
		} else if strings.HasPrefix(trimmed, "Where:") {
			if ev.Location == "" {
				ev.Location = strings.TrimSpace(strings.TrimPrefix(trimmed, "Where:"))
			}
		}
	}

	if ev.Calendar == "" && m.Meta != nil {
		if c, ok := m.Meta["calendar"].(string); ok {
			ev.Calendar = strings.TrimSpace(c)
		}
	}
	if ev.Location == "" && m.Meta != nil {
		if l, ok := m.Meta["location"].(string); ok {
			ev.Location = strings.TrimSpace(l)
		}
	}
	if len(ev.Attendees) == 0 && m.Meta != nil {
		for _, raw := range metaStrings(m.Meta["attendees"]) {
			if r := strings.TrimSpace(raw); r != "" {
				ev.Attendees = append(ev.Attendees, r)
			}
		}
	}

	ev.MeetURL = extractMeetURL(m.Text)

	localSelf := make(map[string]bool, len(selfSet)+1)
	for k, v := range selfSet {
		localSelf[k] = v
	}
	if m.Meta != nil {
		if s, ok := m.Meta["self_email"].(string); ok && strings.TrimSpace(s) != "" {
			localSelf[strings.ToLower(strings.TrimSpace(s))] = true
		}
	}

	var firstExternal string
	for _, att := range ev.Attendees {
		email := extractAttendeeEmail(att)
		if email == "" || isLocalUsernameOrDomain(email) {
			continue
		}
		emailLower := strings.ToLower(email)
		if !localSelf[emailLower] {
			ev.HasExternal = true
			if firstExternal == "" {
				firstExternal = emailLower
			}
		}
	}

	if firstExternal != "" {
		ev.DossierQuery = firstExternal
	} else {
		ev.DossierQuery = m.Title
	}

	return ev
}

func extractMeetURL(text string) string {
	prefixes := []string{
		"https://meet.google.com/",
		"https://zoom.us/",
		"https://teams.microsoft.com/",
	}
	for _, word := range strings.Fields(text) {
		clean := strings.Trim(word, "<>()[]\"',;.")
		for _, p := range prefixes {
			if strings.HasPrefix(clean, p) {
				return clean
			}
			if idx := strings.Index(clean, p); idx != -1 {
				sub := clean[idx:]
				sub = strings.Trim(sub, "<>()[]\"',;.")
				return sub
			}
		}
	}
	return ""
}

func extractAttendeeEmail(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if start := strings.Index(raw, "<"); start != -1 {
		if end := strings.Index(raw[start+1:], ">"); end != -1 {
			candidate := strings.TrimSpace(raw[start+1 : start+1+end])
			if strings.Contains(candidate, "@") {
				return candidate
			}
		}
	}
	if start := strings.Index(raw, "("); start != -1 {
		if end := strings.Index(raw[start+1:], ")"); end != -1 {
			candidate := strings.TrimSpace(raw[start+1 : start+1+end])
			if strings.Contains(candidate, "@") {
				return candidate
			}
		}
	}
	if addr, err := mail.ParseAddress(raw); err == nil && strings.Contains(addr.Address, "@") {
		return addr.Address
	}
	for _, word := range strings.Fields(raw) {
		clean := strings.Trim(word, "<>()[]\"',;:")
		if strings.Contains(clean, "@") {
			return clean
		}
	}
	return ""
}

func isLocalUsernameOrDomain(email string) bool {
	if !strings.Contains(email, "@") {
		return true
	}
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return true
	}
	domain := strings.ToLower(strings.TrimSpace(parts[1]))
	return domain == "localhost" || domain == "local" || strings.HasSuffix(domain, ".local") || domain == ""
}
