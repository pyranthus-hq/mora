package google

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/oauth2"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// LiveFetcher implements Fetcher against real Google services.
type LiveFetcher struct {
	gmail *gmail.Service
	cal   *calendar.Service
}

// NewLiveFetcher builds authed Gmail+Calendar services from a stored token.
// The oauth2 TokenSource auto-refreshes using the refresh token.
func NewLiveFetcher(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (*LiveFetcher, error) {
	ts := cfg.TokenSource(ctx, tok)
	gsrv, err := gmail.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, fmt.Errorf("gmail service: %w", err)
	}
	csrv, err := calendar.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, fmt.Errorf("calendar service: %w", err)
	}
	return &LiveFetcher{gmail: gsrv, cal: csrv}, nil
}

func (f *LiveFetcher) FetchPage(kind ItemKind, w FetchWindow, cursor string) (Page, error) {
	switch kind {
	case KindGmailThread:
		return f.fetchGmailPage(w, cursor)
	case KindCalEvent:
		return f.fetchCalendarPage(w, cursor)
	default:
		return Page{}, fmt.Errorf("unsupported kind %q", kind)
	}
}

// AuthedEmail returns the signed-in account's address (Gmail profile). Connect
// uses it to detect "this Google account is already connected under another
// label" and exit gracefully instead of silently double-ingesting one mailbox.
func (f *LiveFetcher) AuthedEmail() (string, error) {
	p, err := f.gmail.Users.GetProfile("me").Do()
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(p.EmailAddress)), nil
}

// AuthedLabels enumerates Gmail labels so connect can let the user pick by ID.
func (f *LiveFetcher) AuthedLabels() (map[string]string, error) {
	res, err := f.gmail.Users.Labels.List("me").Do()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, l := range res.Labels {
		out[l.Id] = l.Name
	}
	return out, nil
}

func decodeBase64URL(s string) string {
	b, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(s)
	if err != nil {
		// gmail sometimes pads; try standard url decoding
		b, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			return ""
		}
	}
	return string(b)
}
