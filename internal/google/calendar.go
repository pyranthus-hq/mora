package google

import (
	"fmt"
	"strings"
	"time"

	calendar "google.golang.org/api/calendar/v3"
)

const calPageSize = 100

// fetchCalendarPage lists events in [Since, Until], expanding recurrence into
// single instances (SingleEvents=true). Cancelled instances become tombstones.
func (f *LiveFetcher) fetchCalendarPage(w FetchWindow, cursor string) (Page, error) {
	calID := w.CalendarID
	if calID == "" {
		calID = "primary"
	}
	call := f.cal.Events.List(calID).
		SingleEvents(true).
		MaxResults(calPageSize).
		ShowDeleted(true).
		OrderBy("startTime")
	if !w.Since.IsZero() {
		call = call.TimeMin(w.Since.Format(time.RFC3339))
	}
	if !w.Until.IsZero() {
		call = call.TimeMax(w.Until.Format(time.RFC3339))
	}
	if cursor != "" {
		call = call.PageToken(cursor)
	}
	res, err := call.Do()
	if err != nil {
		return Page{}, err
	}
	var items []Item
	for _, ev := range res.Items {
		items = append(items, calEventToItem(calID, ev))
	}
	return Page{Items: items, NextCursor: res.NextPageToken}, nil
}

func calEventToItem(calID string, ev *calendar.Event) Item {
	occurred := parseCalTime(ev.Start)
	title := ev.Summary
	if title == "" {
		title = "(no title)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "When: %s\n", occurred.Format(time.RFC1123))
	if ev.Location != "" {
		fmt.Fprintf(&b, "Where: %s\n", ev.Location)
	}
	if len(ev.Attendees) > 0 {
		var who []string
		for _, a := range ev.Attendees {
			who = append(who, a.Email)
		}
		fmt.Fprintf(&b, "Attendees: %s\n", strings.Join(who, ", "))
	}
	if ev.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", ev.Description)
	}
	// Structured identity for the entity graph (S3): attendees + organizer,
	// lowercased, with display-name aliases. Calendar emails arrive pre-parsed so
	// they're added directly (no net/mail header parse needed).
	attendees, organizer := newAddrSet(), newAddrSet()
	for _, a := range ev.Attendees {
		attendees.add(a.Email, a.DisplayName)
	}
	if ev.Organizer != nil {
		organizer.add(ev.Organizer.Email, ev.Organizer.DisplayName)
	}
	meta := map[string]any{}
	// recurring_event_id only when present — an empty value would be hash material
	// and a pointless meta line on every non-recurring event (codex S2 review).
	if ev.RecurringEventId != "" {
		meta["recurring_event_id"] = ev.RecurringEventId
	}
	putAddrList(meta, "attendees", attendees)
	if orgs := organizer.list(); len(orgs) > 0 {
		meta["organizer"] = orgs[0]
	}
	mergeNames(meta, attendees, organizer)
	if !occurred.IsZero() {
		meta["occurred_at"] = occurred.UTC().Format(time.RFC3339)
	}
	return Item{
		Kind:       KindCalEvent,
		ProviderID: calID + "/" + ev.Id,
		Title:      title,
		Body:       b.String(),
		OccurredAt: occurred,
		Tags:       []string{"calendar"},
		Deleted:    ev.Status == "cancelled",
		Meta:       meta,
	}
}

func parseCalTime(t *calendar.EventDateTime) time.Time {
	if t == nil {
		return time.Time{}
	}
	if t.DateTime != "" {
		if parsed, err := time.Parse(time.RFC3339, t.DateTime); err == nil {
			return parsed
		}
	}
	if t.Date != "" {
		if parsed, err := time.Parse("2006-01-02", t.Date); err == nil {
			return parsed
		}
	}
	return time.Time{}
}
