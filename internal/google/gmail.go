package google

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"

	"github.com/pyranthus-hq/mora/internal/memory"
)

const gmailPageSize = 50

// gmailMessageEvidence is the ordered, per-message substrate retained alongside
// the legacy thread-level identity union. The array order is Gmail thread order;
// recipient lists are normalized and sorted for deterministic frontmatter.
//
// MessageRef and BlockRefs are evidence identity, not content. A Gmail message
// has one connector-visible authored block after quote stripping, named "body".
// The exam renderer uses the same shape with ledger message/block ids so a future
// commitment can anchor to immutable opening evidence without parsing prose.
type gmailMessageEvidence struct {
	MessageRef        string   `json:"message_ref"`
	Sender            string   `json:"sender,omitempty"`
	To                []string `json:"to,omitempty"`
	Cc                []string `json:"cc,omitempty"`
	At                string   `json:"at,omitempty"`
	BlockRefs         []string `json:"block_refs,omitempty"`
	AutomationHeaders []string `json:"automation_headers,omitempty"`
}

func (f *LiveFetcher) fetchGmailPageContext(ctx context.Context, w FetchWindow, cursor string) (Page, error) {
	if w.SyncCursor != "" {
		return f.fetchGmailHistoryPage(ctx, w, cursor)
	}
	syncCursor := ""
	if cursor == "" {
		// Capture BEFORE the initial snapshot. Changes racing pagination then
		// replay through history on the next run instead of falling into a gap.
		profile, err := f.gmail.Users.GetProfile("me").Context(ctx).Do()
		if err != nil {
			return Page{}, err
		}
		syncCursor = strconv.FormatUint(profile.HistoryId, 10)
	}
	call := f.gmail.Users.Threads.List("me").MaxResults(gmailPageSize).Context(ctx)
	if q := buildGmailQuery(w); q != "" {
		call = call.Q(q)
	}
	if len(w.Labels) > 0 {
		call = call.LabelIds(w.Labels...)
	}
	if cursor != "" {
		call = call.PageToken(cursor)
	}
	res, err := gmailRequest(ctx, f.gmailWait, call.Do)
	if err != nil {
		return Page{}, err
	}
	ids := make([]string, len(res.Threads))
	for i, th := range res.Threads {
		ids[i] = th.Id
	}
	threads, err := f.getGmailThreads(ctx, ids)
	if err != nil {
		if ctx.Err() != nil {
			return Page{}, ctx.Err()
		}
		return Page{}, fmt.Errorf("gmail snapshot thread read failed: %w", err)
	}
	var items []Item
	for _, full := range threads {
		// A thread deleted between list and get is expected. Other failures
		// must not advance the snapshot cursor and report missing mail fresh.
		if full != nil {
			items = append(items, gmailThreadToItem(full))
		}
	}
	return Page{Items: items, NextCursor: res.NextPageToken, SyncCursor: syncCursor}, nil
}

func (f *LiveFetcher) fetchGmailHistoryPage(ctx context.Context, w FetchWindow, cursor string) (Page, error) {
	start, err := strconv.ParseUint(w.SyncCursor, 10, 64)
	if err != nil {
		return Page{}, fmt.Errorf("gmail: invalid history cursor %q: %w", w.SyncCursor, err)
	}
	call := f.gmail.Users.History.List("me").StartHistoryId(start).HistoryTypes("messageAdded", "messageDeleted", "labelAdded", "labelRemoved").Context(ctx)
	if cursor != "" {
		call = call.PageToken(cursor)
	}
	res, err := gmailRequest(ctx, f.gmailWait, call.Do)
	if err != nil {
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == 404 {
			return Page{}, fmt.Errorf("%w: gmail history %s", memory.ErrIncrementalCursorExpired, w.SyncCursor)
		}
		return Page{}, err
	}
	changed := map[string]bool{}
	deleted := map[string]bool{}
	for _, history := range res.History {
		for _, message := range history.Messages {
			if message.ThreadId != "" {
				changed[message.ThreadId] = true
			}
		}
		for _, row := range history.MessagesDeleted {
			if row.Message != nil && row.Message.ThreadId != "" {
				deleted[row.Message.ThreadId] = true
			}
		}
	}
	ids := make([]string, 0, len(changed)+len(deleted))
	for id := range changed {
		ids = append(ids, id)
	}
	for id := range deleted {
		if !changed[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	threads, err := f.getGmailThreads(ctx, ids)
	if err != nil {
		return Page{}, err
	}
	items := make([]Item, 0, len(ids))
	for i, full := range threads {
		if full == nil {
			items = append(items, Item{Kind: KindGmailThread, ProviderID: ids[i], Deleted: true, Tags: []string{"gmail"}})
		} else if !gmailThreadExcluded(full, w.Labels) {
			// Apply the snapshot's rule (-category:promotions/social, configured labels).
			items = append(items, gmailThreadToItem(full))
		}
	}
	syncCursor := ""
	if res.NextPageToken == "" {
		syncCursor = strconv.FormatUint(res.HistoryId, 10)
	}
	return Page{Items: items, NextCursor: res.NextPageToken, SyncCursor: syncCursor}, nil
}

// gmailThreadConcurrency bounds outstanding reads, not request rate. The
// per-fetcher pacing gate below separately preserves the two-reads/sec budget.
const gmailThreadConcurrency = 4

// getGmailThreads overlaps independent network reads within a page. Slots retain
// list/history order regardless of completion order. A nil slot is a 404: the
// snapshot skips it while history emits a tombstone. On any other failure all
// workers are cancelled and joined before the caller can advance a checkpoint.
func (f *LiveFetcher) getGmailThreads(ctx context.Context, ids []string) ([]*gmail.Thread, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	threads := make([]*gmail.Thread, len(ids))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var wg sync.WaitGroup
	var firstErr error
	var fail sync.Once
	for range min(gmailThreadConcurrency, len(ids)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					return
				}
				full, err := f.getGmailThread(ctx, ids[i])
				if err != nil {
					var apiErr *googleapi.Error
					if ctx.Err() == nil && errors.As(err, &apiErr) && apiErr.Code == 404 {
						continue
					}
					fail.Do(func() { firstErr = err; cancel() })
					return
				}
				threads[i] = full
			}
		}()
	}
schedule:
	for i := range ids {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break schedule
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return threads, nil
}

func (f *LiveFetcher) getGmailThread(ctx context.Context, id string) (*gmail.Thread, error) {
	call := f.gmail.Users.Threads.Get("me", id).Format("full").Context(ctx)
	return gmailRequest(ctx, f.gmailWait, func(opts ...googleapi.CallOption) (*gmail.Thread, error) {
		// threads.get costs 40 units against a 6,000-unit/minute/user budget.
		// Serialize only pacing, not HTTP latency. Independent worker sleeps
		// would multiply the rate by the worker count. Retries use this same
		// gate after their existing bounded backoff.
		// https://developers.google.com/workspace/gmail/api/reference/quota
		f.gmailThreadPace.Lock()
		err := ctx.Err()
		if err == nil && f.gmailWait != nil {
			err = f.gmailWait(ctx, 500*time.Millisecond)
		}
		f.gmailThreadPace.Unlock()
		if err != nil {
			return nil, err
		}
		return call.Do(opts...)
	})
}

// gmailThreadExcluded mirrors the snapshot query on the history path: skip a
// thread when every message is Promotions/Social, or when labels are
// configured and no single message carries ALL of them (threads.list LabelIds
// is an AND filter; per-message matching is our reading of it at thread level).
// w.Labels are label IDs. shortcut: the Since/Query filters are not re-applied.
func gmailThreadExcluded(th *gmail.Thread, labels []string) bool {
	if len(th.Messages) == 0 {
		return false
	}
	allBulk, labelHit := true, len(labels) == 0
	for _, msg := range th.Messages {
		have := map[string]bool{}
		for _, id := range msg.LabelIds {
			have[id] = true
		}
		allBulk = allBulk && (have["CATEGORY_PROMOTIONS"] || have["CATEGORY_SOCIAL"])
		all := true
		for _, l := range labels {
			all = all && have[l]
		}
		labelHit = labelHit || all
	}
	return allBulk || !labelHit
}

func buildGmailQuery(w FetchWindow) string {
	parts := []string{"-category:promotions", "-category:social"}
	if !w.Since.IsZero() {
		parts = append(parts, "after:"+w.Since.Format("2006/01/02"))
	}
	if w.Query != "" {
		parts = append(parts, w.Query)
	}
	return strings.Join(parts, " ")
}

func gmailThreadToItem(th *gmail.Thread) Item {
	var subject string
	var occurred time.Time
	var bodies []string
	var atts []Attachment
	var messages []gmailMessageEvidence
	// Structured identity for the entity graph (S3): retain the backward-compatible
	// thread-level union while also preserving ordered per-message edges for typed
	// obligation direction and stable evidence identity.
	senders, recipientsTo, recipientsCc := newAddrSet(), newAddrSet(), newAddrSet()
	for i, msg := range th.Messages {
		messageFrom := ""
		messageSenders, messageTo, messageCc := newAddrSet(), newAddrSet(), newAddrSet()
		var automationHeaders []string
		for _, h := range msg.Payload.Headers {
			switch strings.ToLower(strings.TrimSpace(h.Name)) {
			case "subject":
				if subject == "" {
					subject = h.Value
				}
			case "from":
				if messageFrom == "" {
					messageFrom = h.Value
				}
				senders.addHeader(h.Value)
				messageSenders.addHeader(h.Value)
			case "to":
				recipientsTo.addHeader(h.Value)
				messageTo.addHeader(h.Value)
			case "cc":
				recipientsCc.addHeader(h.Value)
				messageCc.addHeader(h.Value)
			case "list-unsubscribe":
				if strings.TrimSpace(h.Value) != "" {
					automationHeaders = append(automationHeaders, "list-unsubscribe")
				}
			case "precedence":
				if v := strings.ToLower(strings.TrimSpace(h.Value)); v == "bulk" || v == "list" || v == "junk" {
					automationHeaders = append(automationHeaders, "precedence: "+v)
				}
			}
		}
		automationHeaders = normalizedAutomationHeaders(automationHeaders)
		body := stripQuoted(decodeGmailBody(msg.Payload))
		evidence := gmailMessageEvidence{
			MessageRef:        gmailMessageRef(th.Id, msg.Id, i),
			Sender:            singleAddress(messageSenders),
			To:                messageTo.list(),
			Cc:                messageCc.list(),
			AutomationHeaders: automationHeaders,
		}
		if msg.InternalDate > 0 {
			t := time.UnixMilli(msg.InternalDate)
			evidence.At = t.UTC().Format(time.RFC3339)
			if t.After(occurred) {
				occurred = t
			}
		}
		if body != "" {
			evidence.BlockRefs = []string{"body"}
		}
		messages = append(messages, evidence)
		bodies = append(bodies, fmt.Sprintf("From: %s\n\n%s", messageFrom, body))
		atts = append(atts, gmailAttachments(msg.Payload)...)
	}
	if subject == "" {
		subject = "(no subject)"
	}
	meta := map[string]any{"message_count": fmt.Sprintf("%d", len(th.Messages))}
	putAddrList(meta, "from", senders)
	putAddrList(meta, "to", recipientsTo)
	putAddrList(meta, "cc", recipientsCc)
	mergeNames(meta, senders, recipientsTo, recipientsCc)
	if len(messages) > 0 {
		meta["messages"] = messages
		if last := messages[len(messages)-1].Sender; last != "" {
			meta["last_sender"] = last
		}
	}
	if !occurred.IsZero() {
		meta["occurred_at"] = occurred.UTC().Format(time.RFC3339)
	}
	// Actionability labels for the brief's urgent lane (issue #62). These are captured
	// into Meta but EXCLUDED from the content hash (memory.MapItem) so a read/star
	// toggle never churns the delta. Absent when the thread carries none (backward
	// compatible with pre-#62 ingests, which just get no urgency boost).
	if labels := gmailUrgencyLabels(th); len(labels) > 0 {
		meta["labels"] = labels
	}
	return Item{
		Kind:        KindGmailThread,
		ProviderID:  th.Id,
		Title:       subject,
		Body:        strings.Join(bodies, "\n\n---\n\n"),
		OccurredAt:  occurred,
		Tags:        []string{"gmail"},
		Attachments: atts,
		Meta:        meta,
	}
}

func normalizedAutomationHeaders(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func gmailMessageRef(threadID, messageID string, index int) string {
	if messageID == "" {
		// Live Gmail messages always carry an id. The positional fallback keeps
		// malformed/test inputs deterministic without pretending it is provider
		// identity.
		messageID = "message-" + strconv.Itoa(index)
	}
	return "gmail_thread/" + threadID + "#" + messageID
}

func singleAddress(set *addrSet) string {
	values := set.list()
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

// gmailUrgencyLabels collects the actionability labels (UNREAD / IMPORTANT / STARRED)
// present on ANY message in the thread, sorted for determinism. Routing/category
// labels (INBOX, CATEGORY_*) are ignored — only the three signals the brief's urgent
// lane ranks on are captured (issue #62 defect 2 enrichment).
func gmailUrgencyLabels(th *gmail.Thread) []string {
	want := map[string]bool{"UNREAD": true, "IMPORTANT": true, "STARRED": true}
	seen := map[string]bool{}
	for _, msg := range th.Messages {
		for _, lid := range msg.LabelIds {
			if want[lid] {
				seen[lid] = true
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// decodeGmailBody walks the MIME tree for the first text/plain part.
func decodeGmailBody(p *gmail.MessagePart) string {
	if p == nil {
		return ""
	}
	if p.MimeType == "text/plain" && p.Body != nil && p.Body.Data != "" {
		return decodeBase64URL(p.Body.Data)
	}
	for _, part := range p.Parts {
		if s := decodeGmailBody(part); s != "" {
			return s
		}
	}
	return ""
}

func gmailAttachments(p *gmail.MessagePart) []Attachment {
	var out []Attachment
	if p == nil {
		return out
	}
	if p.Filename != "" && p.Body != nil {
		out = append(out, Attachment{Filename: p.Filename, MimeType: p.MimeType, Size: p.Body.Size})
	}
	for _, part := range p.Parts {
		out = append(out, gmailAttachments(part)...)
	}
	return out
}

// stripQuoted drops trailing quoted reply lines (cheap heuristic).
func stripQuoted(body string) string {
	var keep []string
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, ">") {
			continue
		}
		if strings.HasPrefix(t, "On ") && strings.HasSuffix(t, "wrote:") {
			break
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}
