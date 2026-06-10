package imessage

// fakeFetcher returns canned pages keyed by cursor. errOnCursor lets a test
// simulate a crash mid-backfill to exercise resume. Mirrors the google/memory
// analog, retyped to imessage's Fetcher seam — drives the grain (IMSG-03) and
// no-network (IMSG-01) tests with NO live chat.db.
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
