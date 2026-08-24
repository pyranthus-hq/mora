package google

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"

	"github.com/pyranthus-hq/mora/internal/memory"
)

const calPageSize = 100

func (f *LiveFetcher) fetchCalendarPageContext(ctx context.Context, w FetchWindow, cursor string) (Page, error) {
	calID := w.CalendarID
	if calID == "" {
		calID = "primary"
	}
	call := f.cal.Events.List(calID).MaxResults(calPageSize).ShowDeleted(true).Context(ctx)
	if w.SyncCursor != "" {
		call = call.SyncToken(w.SyncCursor)
	} else {
		call = call.SingleEvents(true).OrderBy("startTime")
		if !w.Since.IsZero() {
			call = call.TimeMin(w.Since.Format(time.RFC3339))
		}
		if !w.Until.IsZero() {
			call = call.TimeMax(w.Until.Format(time.RFC3339))
		}
	}
	if cursor != "" {
		call = call.PageToken(cursor)
	}
	res, err := call.Do()
	if err != nil {
		var apiErr *googleapi.Error
		if w.SyncCursor != "" && errors.As(err, &apiErr) && apiErr.Code == 410 {
			return Page{}, fmt.Errorf("%w: calendar sync token", memory.ErrIncrementalCursorExpired)
		}
		return Page{}, err
	}
	var items []Item
	for _, ev := range res.Items {
		items = append(items, calEventToItem(calID, ev))
	}
	return Page{Items: items, NextCursor: res.NextPageToken, SyncCursor: res.NextSyncToken}, nil
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
	// selfEmail is the authenticated user's OWN attendee entry. Google marks it with
	// Self=true, and it is frequently NOT the mailbox OAuth was granted on (a
	// Workspace alias, a custom domain). Without it Mora cannot recognize the user in
	// their own invite, admits them as an attendee of their own meeting, and cites
	// their own records back as the counterparty's unfinished business — wrong-person
	// attribution. Capture the authoritative flag rather than inferring identity.
	selfEmail := ""
	for _, a := range ev.Attendees {
		attendees.add(a.Email, a.DisplayName)
		if a.Self && a.Email != "" && selfEmail == "" {
			selfEmail = strings.ToLower(strings.TrimSpace(a.Email))
		}
	}
	if ev.Organizer != nil {
		organizer.add(ev.Organizer.Email, ev.Organizer.DisplayName)
	}
	meta := map[string]any{}
	if selfEmail != "" {
		meta["self_email"] = selfEmail
	}
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
	// source_created_at is when the event was CREATED at Google — a different clock
	// from occurred_at, which is when it starts. An invite accepted in March for a
	// meeting in December has both, months apart, and browse surfaces read them as
	// separate fields (#218); without this key a calendar row can only say when the
	// event happens, never when it came into existence. Google returns Event.Created
	// as RFC3339 and it is absent on some event kinds (birthdays, some imported
	// feeds), so it is validated rather than trusted: an unparseable or empty value
	// is omitted — an empty string would also be hash material on every such event,
	// the same reason recurring_event_id is conditional above. Normalized to UTC
	// RFC3339 like occurred_at so Meta bytes stay stable across runs.
	//
	// The gate is strictRFC3339, not time.Parse, and the difference is not cosmetic
	// here: what lands in Meta is the NORMALIZED render, so a malformed offset does
	// not produce a malformed stamp — it produces a well-formed stamp holding the
	// WRONG instant. `…+00:60` parses as ±01:00 and `…+24:00` as a fixed 24h zone,
	// so a UTC-normalized `source_created_at` would silently be an hour, or a day,
	// away from when Google actually created the event, with nothing downstream able
	// to tell. Provider text that is not RFC 3339 is not evidence of a creation
	// time, so the key is omitted.
	if created, ok := rfc3339Instant(ev.Created); ok {
		meta["source_created_at"] = created.UTC().Format(time.RFC3339)
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
		// DateTime is provider text and is normalized into occurred_at below.
		// Validate its RFC 3339 grammar before parsing so Go's permissive parser
		// cannot launder +00:60/+24:00 into a valid-looking wrong instant.
		if parsed, ok := rfc3339Instant(t.DateTime); ok {
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
