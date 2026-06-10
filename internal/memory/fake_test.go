package memory

// Test-local kind constants standing in for connector kinds. The shared package
// must map any kind without importing a connector.
const (
	kindGmailThread ItemKind = "gmail_thread"
	kindCalEvent    ItemKind = "calendar_event"
)

// fakeFetcher returns canned pages keyed by cursor. errOnCursor lets a test
// simulate a crash mid-backfill to exercise resume.
type fakeFetcher struct {
	pages       map[string]Page // cursor "" is the first page
	errOnCursor map[string]error
	calls       []string // cursors requested, in order
}

func (f *fakeFetcher) FetchPage(kind ItemKind, w FetchWindow, cursor string) (Page, error) {
	f.calls = append(f.calls, cursor)
	if err := f.errOnCursor[cursor]; err != nil {
		return Page{}, err
	}
	return f.pages[cursor], nil
}

var (
	errWrite = errorString("write failed")
	errFetch = errorString("fetch failed")
)

type errorString string

func (e errorString) Error() string { return string(e) }
