package google

import (
	calendar "google.golang.org/api/calendar/v3"
	gmail "google.golang.org/api/gmail/v1"
)

// ggNewLiveFetcher builds a LiveFetcher from pre-constructed Gmail/Calendar
// services so tests can inject httptest-backed endpoints without a real OAuth
// token source or live network. It is the test-only seam that lets us exercise
// fetchGmailPage / fetchCalendarPage / AuthedEmail / AuthedLabels / FetchPage
// against a fake Google API. (Same package, _test.go only — never shipped.)
func ggNewLiveFetcher(g *gmail.Service, c *calendar.Service) *LiveFetcher {
	return &LiveFetcher{gmail: g, cal: c}
}
