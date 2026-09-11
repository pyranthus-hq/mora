package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/imessage"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func decodeLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatalf("line is not one JSON object: %v\n%q", err, line)
		}
		docs = append(docs, doc)
	}
	return docs
}

func TestConnectProgressSinkEmitsPhasesAndThrottles(t *testing.T) {
	var out bytes.Buffer
	clock := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	sink := newConnectProgressSink(&out, func() time.Time { return clock })
	sink.Phase("checking_access")
	sink.Phase("reading")
	sink.AddChat(3) // three messages read
	sink.AddWritten()
	sink.AddChat(2) // still inside the 500 ms window: no line
	sink.AddWritten()
	clock = clock.Add(600 * time.Millisecond)
	sink.AddChat(1) // window elapsed: one line
	sink.AddWritten()
	sink.Phase("done")
	docs := decodeLines(t, out.String())
	if len(docs) != 4 {
		t.Fatalf("want 4 lines (2 phases, 1 throttled tick, done), got %d:\n%s", len(docs), out.String())
	}
	for _, d := range docs {
		if d["schema"] != "mora.connect.progress" || d["schema_version"] != float64(1) {
			t.Fatalf("bad envelope: %v", d)
		}
	}
	last := docs[3]
	if last["phase"] != "done" || last["messages_read"] != float64(6) || last["chats"] != float64(3) || last["elapsed_ms"] != float64(600) {
		t.Fatalf("done line wrong: %v", last)
	}
}

func TestMessageCountReadsTheConversationPayload(t *testing.T) {
	if got := imessage.MessageCount(memory.Item{}); got != 0 {
		t.Fatalf("no payload must count 0, got %d", got)
	}
	if got := imessage.MessageCount(imessage.SyntheticConversationItem(4)); got != 4 {
		t.Fatalf("want 4, got %d", got)
	}
}

func TestConnectIMessageProgressRequiresJSON(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	if _, err := runErr(t, "connect", "imessage", "--progress"); err == nil || !strings.Contains(err.Error(), "--progress requires --json") {
		t.Fatalf("want usage error, got %v", err)
	}
}

func TestConnectIMessageProgressStreamsAndEndsWithReceipt(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	restoreFetcher := stubIMessageFetcherPages(t, 2, 3) // two chats, three messages each
	defer restoreFetcher()
	restoreReady := stubIMessageReadiness(t, true)
	defer restoreReady()
	stdout, stderr, err := runSplit(t, "connect", "imessage", "--json", "--progress")
	if err != nil {
		t.Fatalf("connect: %v (stderr %q)", err, stderr)
	}
	docs := decodeLines(t, stdout)
	if len(docs) < 3 {
		t.Fatalf("want progress lines plus receipt, got %d:\n%s", len(docs), stdout)
	}
	receipt := docs[len(docs)-1]
	if receipt["schema"] != "mora.connect.imessage" || receipt["connected"] != true || receipt["ready"] != true {
		t.Fatalf("last line must be the receipt: %v", receipt)
	}
	if receipt["chats"] != float64(2) || receipt["messages_read"] != float64(6) || receipt["cancelled"] != false {
		t.Fatalf("receipt counts wrong: %v", receipt)
	}
	for _, d := range docs[:len(docs)-1] {
		if d["schema"] != "mora.connect.progress" {
			t.Fatalf("non-progress line before the receipt: %v", d)
		}
	}
	if !strings.Contains(stderr, "enabled imessage") {
		t.Fatal("the readiness prose must go to stderr under --json")
	}
}

func TestConnectIMessageJSONWithoutProgressStillCounts(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	restoreFetcher := stubIMessageFetcherPages(t, 2, 3)
	defer restoreFetcher()
	restoreReady := stubIMessageReadiness(t, true)
	defer restoreReady()
	stdout, _, err := runSplit(t, "connect", "imessage", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal([]byte(stdout), &receipt); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if receipt["chats"] != float64(2) || receipt["messages_read"] != float64(6) {
		t.Fatalf("counts must not depend on --progress: %v", receipt)
	}
}

func TestConnectIMessageCancelMidRunKeepsPagesAndResumes(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(testCtx(t))
	// The fetcher cancels the run after it has served page 1, so page 1 is
	// written and checkpointed while page 2 is never fetched.
	restoreFetcher := stubIMessageFetcherCancelAfter(t, 3, 2, 1, cancel)
	defer func() { restoreFetcher() }()
	restoreReady := stubIMessageReadiness(t, true)
	defer restoreReady()
	var stdout, stderr bytes.Buffer
	if err := Run(ctx, []string{"connect", "imessage", "--json", "--progress"}, &stdout, &stderr, strings.NewReader("")); err != nil {
		t.Fatalf("cancelled connect must exit 0, got %v", err)
	}
	docs := decodeLines(t, stdout.String())
	receipt := docs[len(docs)-1]
	if receipt["cancelled"] != true || receipt["chats"] != float64(1) {
		t.Fatalf("receipt after cancel: %v", receipt)
	}
	written, _ := filepath.Glob(filepath.Join(sourcesRoot(cfg), "imessage", "*.md"))
	if len(written) != 1 {
		t.Fatalf("page 1 must be on disk after cancel, found %d files", len(written))
	}
	st, err := memory.LoadStatus(imessageStatusPath(cfg, "imessage"))
	if err != nil || st == nil || st.Checkpoint == "" {
		t.Fatalf("checkpoint must survive cancel: %+v %v", st, err)
	}
	restoreFetcher()
	restoreFetcher = stubIMessageFetcherPages(t, 3, 2)
	stdout.Reset()
	if err := Run(testCtx(t), []string{"connect", "imessage", "--json"}, &stdout, &stderr, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	var second map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second["cancelled"] != false || second["chats"] != float64(2) {
		t.Fatalf("resume must read only the remaining pages: %v", second)
	}
}

func TestConnectIMessageNotReadyReceiptSaysSo(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	restoreReady := stubIMessageReadiness(t, false)
	defer restoreReady()
	stdout, _, err := runSplit(t, "connect", "imessage", "--json")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var receipt map[string]any
	if err := json.Unmarshal([]byte(stdout), &receipt); err != nil {
		t.Fatalf("receipt: %v\n%s", err, stdout)
	}
	if receipt["connected"] != true || receipt["ready"] != false || receipt["chats"] != float64(0) {
		t.Fatalf("not-ready receipt wrong: %v", receipt)
	}
}

func TestConnectIMessageCancelledContextEndsCleanly(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	restoreFetcher := stubIMessageFetcherPages(t, 5, 1)
	defer restoreFetcher()
	restoreReady := stubIMessageReadiness(t, true)
	defer restoreReady()
	ctx, cancel := context.WithCancel(testCtx(t))
	cancel()
	var stdout, stderr bytes.Buffer
	if err := Run(ctx, []string{"connect", "imessage", "--json", "--progress"}, &stdout, &stderr, strings.NewReader("")); err != nil {
		t.Fatalf("cancelled connect must exit 0, got %v", err)
	}
	docs := decodeLines(t, stdout.String())
	receipt := docs[len(docs)-1]
	if receipt["cancelled"] != true {
		t.Fatalf("receipt must say cancelled: %v", receipt)
	}
}

// stubIMessageFetcherPages replaces newIMessageFetcher with a fetcher that
// yields `chats` synthetic conversations of `messagesEach` messages, one per page.
func stubIMessageFetcherPages(t *testing.T, chats, messagesEach int) func() {
	t.Helper()
	orig := newIMessageFetcher
	newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
		return imessage.NewSyntheticFetcher(chats, messagesEach), nil
	}
	return func() { newIMessageFetcher = orig }
}

// stubIMessageFetcherCancelAfter is stubIMessageFetcherPages whose fetcher
// calls cancel once `afterPage` pages have been served.
func stubIMessageFetcherCancelAfter(t *testing.T, chats, messagesEach, afterPage int, cancel context.CancelFunc) func() {
	t.Helper()
	orig := newIMessageFetcher
	newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
		f := imessage.NewSyntheticFetcher(chats, messagesEach)
		f.AfterPage(afterPage, cancel)
		return f, nil
	}
	return func() { newIMessageFetcher = orig }
}

// stubIMessageReadiness replaces the readiness printer so the test never
// touches the host Messages database.
func stubIMessageReadiness(t *testing.T, ready bool) func() {
	t.Helper()
	origGOOS := runtimeGOOS
	runtimeGOOS = func() string { return "darwin" }
	orig := imessageReadinessFn
	imessageReadinessFn = func(Config, io.Writer, bool) bool { return ready }
	return func() { imessageReadinessFn = orig; runtimeGOOS = origGOOS }
}
