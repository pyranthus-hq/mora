package imessage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// chatPageSize bounds how many chats one FetchPage assembles before returning a
// cursor for the next page (the cursor is the next chat ROWID).
const chatPageSize = 50

// requiredMessageColumns are the message columns the conversation query depends on.
// They are probed at construction (schema-defensive, Pitfall 9) so a chat.db on an
// unsupported macOS version fails with a clear error instead of returning wrong rows.
var requiredMessageColumns = []string{
	"guid", "date", "text", "attributedBody", "is_from_me", "handle_id", "associated_message_type",
}

// optionalMessageColumns enrich rendering when present but are NOT required (their
// names/availability vary by macOS version): item_type distinguishes system events
// from messages (D-12), date_retracted flags unsent/retracted messages (D-12). When
// absent, the query substitutes a literal 0 and every row renders as a normal message.
const (
	colItemType      = "item_type"
	colDateRetracted = "date_retracted"
)

// LiveFetcher implements Fetcher against a real local chat.db, read-only. It carries
// the normalized deny-list (IMSG-06) and which optional columns the live schema has.
// Handle→name resolution is NOT done here — it happens in the mapper (mapConversation)
// so the connector stays a thin, resolver-free reader and the fetched Item carries
// raw handles in its convInput Payload.
type LiveFetcher struct {
	db           *sql.DB
	denyContacts map[string]bool // normalized handles (sole-counterparty 1:1 skip, D-08)
	denyConvos   map[string]bool // lowercased conversation names (thread skip, D-08)
	hasItemType  bool
	hasRetracted bool
}

// NewLiveFetcher opens chat.db read-only (mode=ro + Ping forces a real read so an
// FDA-denied open surfaces now), runs the schema-defensive column probe, captures
// which optional columns exist, and precomputes the normalized deny-list. It never
// writes or checkpoints the DB.
func NewLiveFetcher(path string, deny DenyList) (*LiveFetcher, error) {
	db, err := openChatDB(path)
	if err != nil {
		return nil, err
	}
	have, err := probeMessageSchema(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &LiveFetcher{
		db:           db,
		denyContacts: normalizedContactSet(deny.Contacts),
		denyConvos:   lowercasedSet(deny.Conversations),
		hasItemType:  have[colItemType],
		hasRetracted: have[colDateRetracted],
	}, nil
}

// normalizedContactSet normalizes denied handles (phone strip-non-digits, email
// lowercase) so deny matching agrees with addressbook resolution across formatting.
func normalizedContactSet(handles []string) map[string]bool {
	set := map[string]bool{}
	for _, h := range handles {
		if n := normalizeHandle(h); n != "" {
			set[n] = true
		}
	}
	return set
}

// lowercasedSet lowercases + trims denied conversation names for case-insensitive
// exact-title matching.
func lowercasedSet(names []string) map[string]bool {
	set := map[string]bool{}
	for _, n := range names {
		if t := strings.ToLower(strings.TrimSpace(n)); t != "" {
			set[t] = true
		}
	}
	return set
}

// Close releases the underlying DB handle.
func (f *LiveFetcher) Close() error {
	if f.db == nil {
		return nil
	}
	return f.db.Close()
}

// probeMessageSchema confirms the message columns the query needs exist, via PRAGMA
// table_info(message), and returns the full set of present columns so the caller can
// detect optional columns. On a missing required column it returns a clear
// "unsupported chat.db schema" error rather than letting the query fail cryptically
// or return wrong rows (Pitfall 9).
func probeMessageSchema(db *sql.DB) (map[string]bool, error) {
	rows, err := pragmaTableInfo(db, "message")
	if err != nil {
		return nil, fmt.Errorf("probe chat.db schema: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      sql.NullString
			notNull    int
			dfltValue  sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("probe chat.db schema: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("probe chat.db schema: %w", err)
	}
	if len(have) == 0 {
		return nil, fmt.Errorf("unsupported chat.db schema: no `message` table found")
	}
	var missing []string
	for _, c := range requiredMessageColumns {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("unsupported chat.db schema (missing message columns: %s)", strings.Join(missing, ", "))
	}
	return have, nil
}

// FetchPage pages over chats ordered by chat ROWID. The cursor is the next chat
// ROWID to start from ("" = first page, "" returned when the last chat is reached).
// Each chat becomes exactly one Item (one memory per conversation, IMSG-03), carrying
// its structured convInput as Payload for the mapper. Denied conversations are
// skipped here so they never reach rendering (IMSG-06).
func (f *LiveFetcher) FetchPage(kind ItemKind, w FetchWindow, cursor string) (Page, error) {
	if kind != KindIMessageChat {
		return Page{}, fmt.Errorf("unsupported kind %q", kind)
	}

	startROWID := int64(0)
	if cursor != "" {
		if _, err := fmt.Sscan(cursor, &startROWID); err != nil {
			return Page{}, fmt.Errorf("bad cursor %q: %w", cursor, err)
		}
	}
	sinceNanos := timeToCocoaNanos(w.Since)

	// List the next page of chat ROWIDs.
	chatRows, err := f.db.Query(
		`SELECT ROWID, guid, display_name, chat_identifier
		   FROM chat
		  WHERE ROWID > ?
		  ORDER BY ROWID ASC
		  LIMIT ?`,
		startROWID, chatPageSize,
	)
	if err != nil {
		return Page{}, fmt.Errorf("list chats: %w", err)
	}
	type chatRow struct {
		rowid      int64
		guid       string
		display    sql.NullString
		identifier sql.NullString
	}
	var chats []chatRow
	for chatRows.Next() {
		var c chatRow
		if err := chatRows.Scan(&c.rowid, &c.guid, &c.display, &c.identifier); err != nil {
			chatRows.Close()
			return Page{}, fmt.Errorf("scan chat: %w", err)
		}
		chats = append(chats, c)
	}
	if err := chatRows.Err(); err != nil {
		chatRows.Close()
		return Page{}, fmt.Errorf("iterate chats: %w", err)
	}
	chatRows.Close()

	var items []Item
	var lastROWID int64
	for _, c := range chats {
		lastROWID = c.rowid
		it, ok, err := f.assembleConversation(c.guid, c.display, c.identifier, c.rowid, sinceNanos)
		if err != nil {
			// Per-conversation failure: skip, never abort the page (schema-defensive,
			// honest-snapshot — the caller's Ingest loop counts the gap).
			continue
		}
		if ok {
			items = append(items, it)
		}
	}

	next := ""
	if len(chats) == chatPageSize {
		next = fmt.Sprintf("%d", lastROWID)
	}
	return Page{Items: items, NextCursor: next}, nil
}

// chatParticipants returns the conversation's non-self participant handles from
// chat_handle_join (the authoritative roster, independent of who spoke in the
// window). Used for group/1:1 classification and the sole-counterparty deny rule.
func (f *LiveFetcher) chatParticipants(chatROWID int64) ([]string, error) {
	rows, err := f.db.Query(
		`SELECT h.id
		   FROM chat_handle_join chj
		   JOIN handle h ON h.ROWID = chj.handle_id
		  WHERE chj.chat_id = ?
		  ORDER BY h.ROWID ASC`,
		chatROWID,
	)
	if err != nil {
		return nil, fmt.Errorf("participants query: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h sql.NullString
		if err := rows.Scan(&h); err != nil {
			continue
		}
		if h.Valid && strings.TrimSpace(h.String) != "" {
			out = append(out, h.String)
		}
	}
	return out, rows.Err()
}

// denySkipConversation applies the deny-list predicate BEFORE assembly (IMSG-06,
// D-08): skip the whole conversation when its display name / identifier is denied, or
// when it is a 1:1 whose sole counterparty handle is denied. A denied handle inside a
// multi-party group never drops the group (thread-granularity only); the transcript
// is never per-message stripped.
func (f *LiveFetcher) denySkipConversation(display, identifier sql.NullString, participants []string) bool {
	if len(f.denyConvos) > 0 {
		if display.Valid && f.denyConvos[strings.ToLower(strings.TrimSpace(display.String))] {
			return true
		}
		if identifier.Valid && f.denyConvos[strings.ToLower(strings.TrimSpace(identifier.String))] {
			return true
		}
	}
	if len(f.denyContacts) > 0 {
		// Sole-counterparty 1:1 rule: a single non-self participant whose handle is
		// denied. A group (>1 participant) is never dropped by a denied member.
		sole := ""
		if len(participants) == 1 {
			sole = participants[0]
		} else if len(participants) == 0 && identifier.Valid {
			sole = identifier.String // roster empty: the 1:1 identifier is the handle
		}
		if sole != "" && f.denyContacts[normalizeHandle(sole)] {
			return true
		}
	}
	return false
}

// assembleConversation builds the structured convInput for one chat (raw handles;
// resolution happens in the mapper) and wraps it in an Item. ok is false when the
// conversation is denied or has no renderable messages in the window.
func (f *LiveFetcher) assembleConversation(guid string, display, identifier sql.NullString, chatROWID, sinceNanos int64) (Item, bool, error) {
	participants, err := f.chatParticipants(chatROWID)
	if err != nil {
		return Item{}, false, err
	}
	if f.denySkipConversation(display, identifier, participants) {
		return Item{}, false, nil
	}
	isGroup := len(participants) > 1

	msgs, atts, latest, err := f.conversationMessages(chatROWID, sinceNanos)
	if err != nil {
		return Item{}, false, err
	}
	if len(msgs) == 0 {
		return Item{}, false, nil
	}

	chat := conversation{
		participants: participants,
		isGroup:      isGroup,
	}
	if display.Valid {
		chat.displayName = display.String
	}
	if identifier.Valid {
		chat.identifier = identifier.String
	}

	return Item{
		Kind:       KindIMessageChat,
		ProviderID: guid,
		OccurredAt: cocoaEpochToTime(latest),
		Tags:       []string{"imessage"},
		Payload: convInput{
			guid:        guid,
			chat:        chat,
			messages:    msgs,
			attachments: atts,
		},
	}, true, nil
}

// conversationMessages runs the message join for one chat and returns the renderable
// messages (chronological), the collapsed attachment metadata, and the newest message
// date (Cocoa-epoch). It filters tapbacks (associated_message_type != 0, D-12) and
// classifies system/retracted messages from the optional columns when present.
func (f *LiveFetcher) conversationMessages(chatROWID, sinceNanos int64) ([]renderMessage, []Attachment, int64, error) {
	// Optional columns become literal 0 when the live schema lacks them, keeping the
	// SELECT arity fixed and the scan simple (schema-defensive). The identifier
	// check mirrors pragmaTableInfo: the names are package constants today, and the
	// guard keeps this splice inert if they ever stop being (#176).
	itemTypeExpr := "0"
	if f.hasItemType && sqlIdentifier.MatchString(colItemType) {
		itemTypeExpr = "m." + colItemType
	}
	retractedExpr := "0"
	if f.hasRetracted && sqlIdentifier.MatchString(colDateRetracted) {
		retractedExpr = "m." + colDateRetracted
	}
	query := fmt.Sprintf(`SELECT m.ROWID, m.guid, m.date, m.is_from_me, m.text, m.attributedBody,
	        m.associated_message_type, %s, %s, h.id, a.filename, a.mime_type, a.total_bytes
	   FROM message m
	   JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
	   LEFT JOIN handle h ON h.ROWID = m.handle_id
	   LEFT JOIN message_attachment_join maj ON maj.message_id = m.ROWID
	   LEFT JOIN attachment a ON a.ROWID = maj.attachment_id
	  WHERE cmj.chat_id = ? AND m.date >= ?
	  ORDER BY m.date ASC, m.ROWID ASC`, itemTypeExpr, retractedExpr)

	rows, err := f.db.Query(query, chatROWID, sinceNanos)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("conversation query: %w", err)
	}
	defer rows.Close()

	// One row per message×attachment; collapse to one logical message keyed by ROWID
	// (a message with N attachments yields N rows). seen preserves first-seen order.
	type msg struct {
		rowid    int64
		guid     string
		date     int64
		fromMe   bool
		kind     messageKind
		sender   string // raw handle; "" for self
		body     string
		attaches []Attachment
	}
	byID := map[int64]*msg{}
	var order []int64
	var latest int64

	for rows.Next() {
		var (
			rowid       int64
			messageGUID sql.NullString
			date        int64
			isFromMe    int
			text        sql.NullString
			attrBody    []byte
			assocType   sql.NullInt64
			itemType    sql.NullInt64
			dateRetract sql.NullInt64
			handleID    sql.NullString
			attFile     sql.NullString
			attMime     sql.NullString
			attBytes    sql.NullInt64
		)
		if err := rows.Scan(&rowid, &messageGUID, &date, &isFromMe, &text, &attrBody,
			&assocType, &itemType, &dateRetract, &handleID, &attFile, &attMime, &attBytes); err != nil {
			// Tolerate an anomalous row: skip it, never crash the conversation.
			continue
		}
		// Filter tapbacks/reactions entirely (D-12): associated_message_type != 0.
		if assocType.Valid && assocType.Int64 != 0 {
			continue
		}

		m, ok := byID[rowid]
		if !ok {
			body := ""
			if text.Valid {
				// Strip the inline-attachment placeholder here too: the text column
				// (read first) can carry U+FFFC, which would bypass the decoder's
				// strip. A text that is only a placeholder collapses to "" and falls
				// through to the attributedBody / attachment-only path below.
				body = stripAttachmentPlaceholder(text.String)
			}
			if strings.TrimSpace(body) == "" && len(attrBody) > 0 {
				body = decodeAttributedBody(attrBody) // IMSG-02 fallback (also strips U+FFFC)
			}
			kind := msgNormal
			if dateRetract.Valid && dateRetract.Int64 != 0 {
				kind = msgRetracted
			} else if itemType.Valid && itemType.Int64 != 0 {
				kind = msgSystem
			}
			sender := ""
			if isFromMe == 0 && handleID.Valid {
				sender = handleID.String // raw handle; mapper resolves to a name (D-09)
			}
			m = &msg{rowid: rowid, guid: strings.TrimSpace(messageGUID.String), date: date, fromMe: isFromMe != 0, kind: kind, sender: sender, body: body}
			byID[rowid] = m
			order = append(order, rowid)
			if date > latest {
				latest = date
			}
		}
		if attFile.Valid && attFile.String != "" {
			att := Attachment{Filename: baseName(attFile.String), Path: expandHome(attFile.String)}
			if attMime.Valid {
				att.MimeType = attMime.String
			}
			if attBytes.Valid {
				att.Size = attBytes.Int64
			}
			m.attaches = append(m.attaches, att)
		} else if attMime.Valid && attMime.String != "" {
			// Attachment row with a MIME type but no filename — still metadata only.
			m.attaches = append(m.attaches, Attachment{MimeType: attMime.String})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, fmt.Errorf("iterate conversation: %w", err)
	}

	var msgs []renderMessage
	var allAtt []Attachment
	for _, id := range order {
		m := byID[id]
		hasBody := strings.TrimSpace(m.body) != ""
		// Keep a message that renders to something: text, an attachment, a retracted
		// marker, or a system event WITH text (an empty system row renders nothing).
		switch {
		case hasBody, len(m.attaches) > 0, m.kind == msgRetracted:
		default:
			continue
		}
		allAtt = append(allAtt, m.attaches...)
		msgs = append(msgs, renderMessage{
			guid:        m.guid,
			date:        cocoaEpochToTime(m.date),
			fromMe:      m.fromMe,
			sender:      m.sender,
			text:        m.body,
			attachments: m.attaches,
			kind:        m.kind,
		})
	}
	return msgs, allAtt, latest, nil
}

// baseName strips any directory portion from an attachment filename: it keeps the
// display name path-free for rendered output (D-11/IMSG-07's user-facing guarantee).
// The on-disk location now travels separately in Attachment.Path per the IMSG-07
// amendment, for the wiring boundary only — never for rendering.
func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// expandHome resolves the leading "~" chat.db stores in attachment paths to the
// real home directory, yielding an absolute path the wiring boundary can read.
// The connector itself never opens the file (no-bytes invariant intact); a path
// that doesn't start with "~/" is returned as-is.
func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
