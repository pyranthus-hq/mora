package google

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	gmail "google.golang.org/api/gmail/v1"
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
	MessageRef string   `json:"message_ref"`
	Sender     string   `json:"sender,omitempty"`
	To         []string `json:"to,omitempty"`
	Cc         []string `json:"cc,omitempty"`
	At         string   `json:"at,omitempty"`
	BlockRefs  []string `json:"block_refs,omitempty"`
}

// fetchGmailPage lists one page of threads, fetches each thread, and maps it to
// a single Item (thread-level). Quote-stripping keeps bodies lean.
func (f *LiveFetcher) fetchGmailPage(w FetchWindow, cursor string) (Page, error) {
	call := f.gmail.Users.Threads.List("me").MaxResults(gmailPageSize)
	if q := buildGmailQuery(w); q != "" {
		call = call.Q(q)
	}
	if len(w.Labels) > 0 {
		call = call.LabelIds(w.Labels...)
	}
	if cursor != "" {
		call = call.PageToken(cursor)
	}
	res, err := call.Do()
	if err != nil {
		return Page{}, err
	}
	var items []Item
	for _, th := range res.Threads {
		full, err := f.gmail.Users.Threads.Get("me", th.Id).Format("full").Do()
		if err != nil {
			continue // per-thread failure: skip
		}
		items = append(items, gmailThreadToItem(full))
	}
	return Page{Items: items, NextCursor: res.NextPageToken}, nil
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
		for _, h := range msg.Payload.Headers {
			switch h.Name {
			case "Subject":
				if subject == "" {
					subject = h.Value
				}
			case "From":
				if messageFrom == "" {
					messageFrom = h.Value
				}
				senders.addHeader(h.Value)
				messageSenders.addHeader(h.Value)
			case "To":
				recipientsTo.addHeader(h.Value)
				messageTo.addHeader(h.Value)
			case "Cc":
				recipientsCc.addHeader(h.Value)
				messageCc.addHeader(h.Value)
			}
		}
		body := stripQuoted(decodeGmailBody(msg.Payload))
		evidence := gmailMessageEvidence{
			MessageRef: gmailMessageRef(th.Id, msg.Id, i),
			Sender:     singleAddress(messageSenders),
			To:         messageTo.list(),
			Cc:         messageCc.list(),
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
