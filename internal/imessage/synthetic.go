package imessage

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// SyntheticConversationItem builds a conversation Item with n placeholder
// messages. Test-only shape exported so other packages' tests can drive the
// connect path without a Messages database.
func SyntheticConversationItem(n int) memory.Item {
	msgs := make([]renderMessage, 0, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		msgs = append(msgs, renderMessage{
			guid:   "synthetic-guid-" + strconv.Itoa(i+1),
			date:   base.Add(time.Duration(i) * time.Minute),
			fromMe: i%2 == 0,
			sender: "synthetic-1",
			text:   fmt.Sprintf("synthetic message %d", i+1),
		})
	}
	return memory.Item{
		Kind:       KindIMessageChat,
		ProviderID: "synthetic-chat",
		Title:      "synthetic-1",
		OccurredAt: base,
		Payload: convInput{
			guid:     "synthetic-chat",
			chat:     conversation{displayName: "synthetic-1", identifier: "synthetic-1", participants: []string{"synthetic-1"}},
			messages: msgs,
		},
	}
}

// SyntheticFetcher yields one synthetic conversation per page.
type SyntheticFetcher struct {
	chats, messagesEach int
	served              int
	afterPage           int
	onAfterPage         func()
}

func NewSyntheticFetcher(chats, messagesEach int) *SyntheticFetcher {
	return &SyntheticFetcher{chats: chats, messagesEach: messagesEach}
}

// AfterPage runs fn once n pages have been served (used to cancel a run mid-way).
func (f *SyntheticFetcher) AfterPage(n int, fn func()) {
	f.afterPage, f.onAfterPage = n, fn
}

func (f *SyntheticFetcher) Close() error { return nil }

func (f *SyntheticFetcher) FetchPage(kind memory.ItemKind, w memory.FetchWindow, cursor string) (memory.Page, error) {
	return f.FetchPageContext(context.Background(), kind, w, cursor)
}

func (f *SyntheticFetcher) FetchPageContext(ctx context.Context, _ memory.ItemKind, _ memory.FetchWindow, cursor string) (memory.Page, error) {
	if err := ctx.Err(); err != nil {
		return memory.Page{}, err
	}
	i := 0
	if cursor != "" {
		i, _ = strconv.Atoi(cursor)
	}
	if i >= f.chats {
		return memory.Page{}, nil
	}
	it := SyntheticConversationItem(f.messagesEach)
	it.ProviderID = "synthetic-chat-" + strconv.Itoa(i+1)
	if c, ok := it.Payload.(convInput); ok {
		c.guid = it.ProviderID
		it.Payload = c
	}
	page := memory.Page{Items: []memory.Item{it}}
	if i+1 < f.chats {
		page.NextCursor = strconv.Itoa(i + 1)
	}
	f.served++
	if f.onAfterPage != nil && f.served == f.afterPage {
		f.onAfterPage()
	}
	return page, nil
}
