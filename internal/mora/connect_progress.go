package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

const schemaConnectProgress = "mora.connect.progress"

const connectProgressEvery = 500 * time.Millisecond

type connectProgressEvent struct {
	Schema        string `json:"schema"`
	SchemaVersion int    `json:"schema_version"`
	Phase         string `json:"phase"`
	MessagesRead  int    `json:"messages_read"`
	Chats         int    `json:"chats"`
	ElapsedMs     int64  `json:"elapsed_ms"`
}

// connectProgressSink writes one compact JSON object per line. It is the only
// writer on stdout while --progress is on, so the stream stays byte-clean.
type connectProgressSink struct {
	writeMu  sync.Mutex
	mu       sync.Mutex
	w        io.Writer
	now      func() time.Time
	start    time.Time
	last     time.Time
	phase    string
	messages int
	chats    int
}

// newConnectProgressSink with a nil writer counts but never emits; the receipt
// counts therefore never depend on whether --progress was given.
func newConnectProgressSink(w io.Writer, now func() time.Time) *connectProgressSink {
	start := now()
	return &connectProgressSink{w: w, now: now, start: start, last: start.Add(-connectProgressEvery)}
}

// StartTicker keeps elapsed time moving even while a conversation is being read.
// stop is idempotent and joins the writer before the caller emits its receipt.
func (s *connectProgressSink) StartTicker(ctx context.Context) (stop func()) {
	if s.w == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(connectProgressEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.writeMu.Lock()
				if ctx.Err() != nil {
					s.writeMu.Unlock()
					return
				}
				s.mu.Lock()
				var body []byte
				if s.now().Sub(s.last) >= connectProgressEvery {
					body = s.emitLocked()
				}
				s.mu.Unlock()
				s.writeLine(body)
				s.writeMu.Unlock()
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (s *connectProgressSink) Phase(phase string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	s.phase = phase
	body := s.emitLocked()
	s.mu.Unlock()
	s.writeLine(body)
}

// AddChat records one conversation read with n messages; it emits at most
// every connectProgressEvery.
func (s *connectProgressSink) AddChat(n int) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	s.messages += n
	var body []byte
	if s.now().Sub(s.last) >= connectProgressEvery {
		body = s.emitLocked()
	}
	s.mu.Unlock()
	s.writeLine(body)
}

func (s *connectProgressSink) Counts() (messages, chats int, elapsed time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messages, s.chats, s.now().Sub(s.start)
}

// AddWritten records one conversation persisted; chats count what was kept,
// messages count what was read (spec section 14: counts come from receipts).
func (s *connectProgressSink) AddWritten() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chats++
}

// emitLocked snapshots a line under mu; callers serialize writes with writeMu.
func (s *connectProgressSink) emitLocked() []byte {
	now := s.now()
	s.last = now
	if s.w == nil {
		return nil
	}
	ev := connectProgressEvent{
		Schema: schemaConnectProgress, SchemaVersion: 1, Phase: s.phase,
		MessagesRead: s.messages, Chats: s.chats, ElapsedMs: now.Sub(s.start).Milliseconds(),
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return nil
	}
	return body
}

func (s *connectProgressSink) writeLine(body []byte) {
	if body != nil {
		fmt.Fprintf(s.w, "%s\n", body)
	}
}

type connectProgressKey struct{}

func withConnectProgress(ctx context.Context, s *connectProgressSink) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, connectProgressKey{}, s)
}

func connectProgressFrom(ctx context.Context) *connectProgressSink {
	s, _ := ctx.Value(connectProgressKey{}).(*connectProgressSink)
	return s
}

// connectPageFetcher observes cancellation at page boundaries. The ingest loop
// can finish and checkpoint a page already returned by the source.
type connectPageFetcher struct {
	iMessageFetcher
	ctx context.Context
}

func (f connectPageFetcher) FetchPageContext(ctx context.Context, kind memory.ItemKind, window memory.FetchWindow, cursor string) (memory.Page, error) {
	if err := f.ctx.Err(); err != nil {
		return memory.Page{}, err
	}
	fetchCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(f.ctx, cancel)
	defer stop()
	defer cancel()
	if fetcher, ok := f.iMessageFetcher.(interface {
		FetchPageContext(context.Context, memory.ItemKind, memory.FetchWindow, string) (memory.Page, error)
	}); ok {
		return fetcher.FetchPageContext(fetchCtx, kind, window, cursor)
	}
	return f.iMessageFetcher.FetchPage(kind, window, cursor)
}
