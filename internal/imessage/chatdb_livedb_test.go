//go:build livedb

package imessage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestLiveChatDBConversation is the FDA-gated manual integration gate. It does NOT
// run in default CI (the `livedb` build tag excludes it); run it in a terminal that
// HAS Full Disk Access:
//
//	go test ./internal/imessage/ -run TestLiveChatDBConversation -tags=livedb
//
// It opens the real ~/Library/Messages/chat.db read-only, pages conversations, and
// asserts at least one conversation Item has a non-empty body and a plausible
// (post-2001) date — the real-device proof for IMSG-01/02/03/05/09.
func TestLiveChatDBConversation(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	path := filepath.Join(home, "Library", "Messages", "chat.db")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no chat.db at %s: %v", path, err)
	}

	f, err := NewLiveFetcher(path, DenyList{})
	if err != nil {
		t.Fatalf("NewLiveFetcher (FDA granted? schema supported?): %v", err)
	}
	defer f.Close()

	// Resolve names off the live AddressBook when available; falls back to raw handles.
	res, _ := NewResolver(DefaultAddressBookRoot(home))

	w := FetchWindow{Since: time.Now().AddDate(0, 0, -90)}
	var nonEmpty int
	var sawPlausibleDate bool
	// DIAGNOSTIC ONLY: count the OTHER party's (is_from_me=0) ordinary messages that
	// rendered with text. Do NOT use this as the drop-bug gate. A dropped
	// attributedBody-only message with NO attachment is filtered out by
	// conversationMessages before it reaches convInput.messages (invisible here); one
	// WITH an attachment still renders an empty body next to its marker, so it is only
	// partially visible. The ratio does shift (this corpus: ~14% buggy → ~96% fixed),
	// but it conflates text-column and attributedBody sources and cannot prove decode
	// correctness. TestLiveReceivedAttributedBodyDecodes queries the raw rows and is
	// the real regression gate.
	var recvMsgs, recvNonEmpty int
	cursor := ""
	pages := 0
	for pages < 20 { // bound the walk for a manual gate
		page, err := f.FetchPage(KindIMessageChat, w, cursor)
		if err != nil {
			t.Fatalf("FetchPage(%q): %v", cursor, err)
		}
		for _, it := range page.Items {
			c, ok := it.Payload.(convInput)
			if !ok {
				continue
			}
			if strings.TrimSpace(mapConversation(c, res, 0).Body) != "" {
				nonEmpty++
			}
			for _, m := range c.messages {
				if !m.fromMe && m.kind == msgNormal {
					recvMsgs++
					if strings.TrimSpace(m.text) != "" {
						recvNonEmpty++
					}
				}
			}
			if it.OccurredAt.Year() >= 2001 {
				sawPlausibleDate = true
			}
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if nonEmpty == 0 {
		t.Errorf("no conversation produced a non-empty body in the last 90 days")
	}
	if !sawPlausibleDate {
		t.Errorf("no conversation had a plausible post-2001 date")
	}
	t.Logf("live chat.db: %d non-empty conversation bodies across %d page(s); received (rendered) messages %d/%d with text [diagnostic — drop-bug gate is TestLiveReceivedAttributedBodyDecodes]",
		nonEmpty, pages, recvNonEmpty, recvMsgs)
}

// TestLiveReceivedAttributedBodyDecodes is the precise FDA-gated regression for the
// received-message drop bug (Phase 2.1). On modern macOS the body of a message lives
// ONLY in attributedBody (the text column is NULL); the shipped decoder returned ""
// for that layout, so every such message was silently dropped. Because the user's own
// recently-sent messages still had a populated text column at sync time, the drop
// looked like it only hit the OTHER party — but the real fault was the decoder.
//
// This pulls REAL received (is_from_me=0) attributedBody-only rows straight from
// chat.db and asserts they decode to non-empty, valid UTF-8. Run it in an FDA-granted
// terminal:
//
//	go test ./internal/imessage/ -run TestLiveReceivedAttributedBodyDecodes -tags=livedb
func TestLiveReceivedAttributedBodyDecodes(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	path := filepath.Join(home, "Library", "Messages", "chat.db")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no chat.db at %s: %v", path, err)
	}

	db, err := openChatDB(path)
	if err != nil {
		t.Fatalf("openChatDB (FDA granted? schema supported?): %v", err)
	}
	defer db.Close()

	// Received messages whose body is attributedBody-only (text NULL/blank), excluding
	// tapbacks/reactions (associated_message_type != 0) — the exact rows the drop bug
	// rendered empty.
	rows, err := db.Query(`SELECT m.attributedBody
		  FROM message m
		 WHERE m.is_from_me = 0
		   AND (m.text IS NULL OR length(trim(m.text)) = 0)
		   AND m.attributedBody IS NOT NULL
		   AND m.associated_message_type = 0
		 ORDER BY m.date DESC
		 LIMIT 300`)
	if err != nil {
		t.Fatalf("query received attributedBody rows: %v", err)
	}
	defer rows.Close()

	var total, nonEmpty, badUTF8, fffcLeaks int
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			t.Fatalf("scan attributedBody: %v", err)
		}
		total++
		got := decodeAttributedBody(blob)
		if strings.TrimSpace(got) != "" {
			nonEmpty++
		}
		if got != "" && !utf8.ValidString(got) {
			badUTF8++
		}
		if strings.Contains(got, "￼") {
			fffcLeaks++ // the inline-attachment placeholder must be stripped, not surfaced
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate received rows: %v", err)
	}

	if total < 10 {
		t.Skipf("only %d received attributedBody-only messages found — need a populated chat.db to gate", total)
	}
	if badUTF8 > 0 {
		t.Errorf("%d/%d received bodies decoded to invalid UTF-8", badUTF8, total)
	}
	if fffcLeaks > 0 {
		t.Errorf("%d/%d received bodies leaked the raw U+FFFC inline-attachment placeholder", fffcLeaks, total)
	}
	// The drop bug rendered EVERY one of these empty (ratio 0.0). After the fix the
	// real corpus decodes ~97% non-empty — the small remainder are pure inline-
	// attachment-placeholder bubbles that correctly strip to "" (they carry only an
	// image, no text). A 0.90 floor proves the drop bug is fixed with a wide margin and
	// without flaking when a sampled window happens to be image-heavy.
	if ratio := float64(nonEmpty) / float64(total); ratio < 0.90 {
		t.Errorf("only %d/%d (%.1f%%) received attributedBody-only messages decoded non-empty — the received-message drop bug (want >= 90%%)",
			nonEmpty, total, ratio*100)
	}
	t.Logf("received attributedBody-only: %d/%d decoded non-empty, %d U+FFFC leaks", nonEmpty, total, fffcLeaks)
}
