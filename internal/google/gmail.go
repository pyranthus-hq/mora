package google

import (
	"fmt"
	"strings"
	"time"

	gmail "google.golang.org/api/gmail/v1"
)

const gmailPageSize = 50

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
	var subject, from string
	var occurred time.Time
	var bodies []string
	var atts []Attachment
	// Structured identity for the entity graph (S3): every sender (From) and every
	// recipient (To/Cc) across the thread, parsed with net/mail so quoted names and
	// lists don't mint bogus people. Thread-level union; per-message edges are not
	// modeled (capture stays thread-grained, matching the rest of Gmail ingest).
	senders, recipientsTo, recipientsCc := newAddrSet(), newAddrSet(), newAddrSet()
	for _, msg := range th.Messages {
		for _, h := range msg.Payload.Headers {
			switch h.Name {
			case "Subject":
				if subject == "" {
					subject = h.Value
				}
			case "From":
				if from == "" {
					from = h.Value
				}
				senders.addHeader(h.Value)
			case "To":
				recipientsTo.addHeader(h.Value)
			case "Cc":
				recipientsCc.addHeader(h.Value)
			}
		}
		if msg.InternalDate > 0 {
			t := time.UnixMilli(msg.InternalDate)
			if t.After(occurred) {
				occurred = t
			}
		}
		bodies = append(bodies, stripQuoted(decodeGmailBody(msg.Payload)))
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
	if !occurred.IsZero() {
		meta["occurred_at"] = occurred.UTC().Format(time.RFC3339)
	}
	return Item{
		Kind:        KindGmailThread,
		ProviderID:  th.Id,
		Title:       subject,
		Body:        fmt.Sprintf("From: %s\n\n%s", from, strings.Join(bodies, "\n\n---\n\n")),
		OccurredAt:  occurred,
		Tags:        []string{"gmail"},
		Attachments: atts,
		Meta:        meta,
	}
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
