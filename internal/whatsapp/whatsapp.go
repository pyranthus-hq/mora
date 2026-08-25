// Package whatsapp ingests conversations from WhatsApp Desktop's local macOS
// ChatStorage.sqlite. It is deliberately local and read-only: this package has
// no network imports and never opens WhatsApp's Axolotl.sqlite key store.
package whatsapp

import (
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
	_ "modernc.org/sqlite"
)

const KindConversation memory.ItemKind = "whatsapp_conversation"

const (
	pageSize               = 50
	cocoaUnixOffsetSeconds = 978307200
)

func init() { memory.RegisterKind(KindConversation, "whatsapp", "whatsapp") }

// Reader is the backend boundary. V1 ships only LiveFetcher; a future backend
// must be selected explicitly and implement the same local snapshot contract.
type Reader interface {
	memory.Fetcher
	Close() error
}

type message struct {
	id       string
	at       time.Time
	fromMe   bool
	sender   string
	body     string
	typeCode int
}

type conversation struct {
	jid      string
	title    string
	group    bool
	messages []message
}

type LiveFetcher struct{ db *sql.DB }

func DefaultDBPath(home string) string {
	return filepath.Join(home, "Library", "Group Containers", "group.net.whatsapp.WhatsApp.shared", "ChatStorage.sqlite")
}

func databaseDSN(path string) string {
	return "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
}

func NewLiveFetcher(path string) (*LiveFetcher, error) {
	db, err := sql.Open("sqlite", databaseDSN(path))
	if err != nil {
		return nil, err
	}
	if err := probeSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &LiveFetcher{db: db}, nil
}

func (f *LiveFetcher) Close() error {
	if f == nil || f.db == nil {
		return nil
	}
	return f.db.Close()
}

func ProbeReadable(path string) (bool, error) {
	f, err := NewLiveFetcher(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM ZWACHATSESSION`).Scan(&n); err != nil {
		return false, err
	}
	return true, nil
}

var requiredSchema = map[string][]string{
	"ZWACHATSESSION": {"Z_PK", "ZCONTACTJID", "ZCONTACTIDENTIFIER", "ZPARTNERNAME"},
	"ZWAMESSAGE":     {"Z_PK", "ZCHATSESSION", "ZGROUPMEMBER", "ZMEDIAITEM", "ZISFROMME", "ZMESSAGETYPE", "ZMESSAGEDATE", "ZPUSHNAME", "ZSTANZAID", "ZTEXT", "ZFROMJID"},
	"ZWAGROUPMEMBER": {"Z_PK", "ZCONTACTNAME", "ZFIRSTNAME", "ZMEMBERJID"},
	"ZWAMEDIAITEM":   {"Z_PK", "ZTITLE", "ZVCARDNAME", "ZLATITUDE", "ZLONGITUDE"},
}

func probeSchema(db *sql.DB) error {
	for table, required := range requiredSchema {
		rows, err := db.Query("PRAGMA table_info(" + table + ")") // table is a compile-time identifier above.
		if err != nil {
			return fmt.Errorf("unsupported ChatStorage.sqlite schema: probe %s: %w", table, err)
		}
		have := map[string]bool{}
		for rows.Next() {
			var cid, notnull, pk int
			var name string
			var typ, dflt sql.NullString
			if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
				rows.Close()
				return fmt.Errorf("unsupported ChatStorage.sqlite schema: probe %s: %w", table, err)
			}
			have[name] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("unsupported ChatStorage.sqlite schema: probe %s: %w", table, err)
		}
		rows.Close()
		var missing []string
		for _, col := range required {
			if !have[col] {
				missing = append(missing, col)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Errorf("unsupported ChatStorage.sqlite schema: %s missing %s", table, strings.Join(missing, ", "))
		}
	}
	return nil
}

func (f *LiveFetcher) FetchPage(kind memory.ItemKind, window memory.FetchWindow, cursor string) (memory.Page, error) {
	if kind != KindConversation {
		return memory.Page{}, fmt.Errorf("unsupported kind %q", kind)
	}
	start := int64(0)
	if cursor != "" {
		if _, err := fmt.Sscan(cursor, &start); err != nil {
			return memory.Page{}, fmt.Errorf("bad cursor %q: %w", cursor, err)
		}
	}
	rows, err := f.db.Query(`SELECT Z_PK,
		COALESCE(NULLIF(ZCONTACTJID,''), NULLIF(ZCONTACTIDENTIFIER,''), ''),
		COALESCE(NULLIF(ZPARTNERNAME,''), NULLIF(ZCONTACTJID,''), NULLIF(ZCONTACTIDENTIFIER,''), '')
		FROM ZWACHATSESSION WHERE Z_PK > ? ORDER BY Z_PK LIMIT ?`, start, pageSize)
	if err != nil {
		return memory.Page{}, fmt.Errorf("list WhatsApp chats: %w", err)
	}
	type chat struct {
		pk         int64
		jid, title string
	}
	var chats []chat
	for rows.Next() {
		var c chat
		if err := rows.Scan(&c.pk, &c.jid, &c.title); err != nil {
			rows.Close()
			return memory.Page{}, fmt.Errorf("scan WhatsApp chat: %w", err)
		}
		chats = append(chats, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return memory.Page{}, fmt.Errorf("iterate WhatsApp chats: %w", err)
	}
	rows.Close()

	items := make([]memory.Item, 0, len(chats))
	for _, c := range chats {
		if c.jid == "" || (!strings.HasSuffix(c.jid, "@g.us") && !strings.HasSuffix(c.jid, "@s.whatsapp.net")) {
			continue
		}
		conv, err := f.readConversation(c.pk, c.jid, c.title, window.Since)
		if err != nil {
			return memory.Page{}, err // honest snapshot: a query failure invalidates the page.
		}
		if len(conv.messages) == 0 {
			continue
		}
		items = append(items, memory.Item{Kind: KindConversation, ProviderID: c.jid, Title: conv.title, Payload: conv})
	}
	next := ""
	if len(chats) == pageSize {
		next = strconv.FormatInt(chats[len(chats)-1].pk, 10)
	}
	return memory.Page{Items: items, NextCursor: next}, nil
}

func (f *LiveFetcher) readConversation(chatPK int64, jid, title string, since time.Time) (conversation, error) {
	cutoff := float64(0)
	if !since.IsZero() {
		cutoff = float64(since.Unix() - cocoaUnixOffsetSeconds)
	}
	rows, err := f.db.Query(`SELECT m.Z_PK, COALESCE(m.ZSTANZAID,''), COALESCE(m.ZMESSAGEDATE,0),
		COALESCE(m.ZISFROMME,0), COALESCE(m.ZMESSAGETYPE,-1), COALESCE(m.ZTEXT,''),
		COALESCE(NULLIF(g.ZCONTACTNAME,''), NULLIF(g.ZFIRSTNAME,''), NULLIF(m.ZPUSHNAME,''), NULLIF(g.ZMEMBERJID,''), NULLIF(m.ZFROMJID,''), 'Unknown'),
		COALESCE(mi.ZTITLE,''), COALESCE(mi.ZVCARDNAME,''), COALESCE(mi.ZLATITUDE,0), COALESCE(mi.ZLONGITUDE,0)
		FROM ZWAMESSAGE m
		LEFT JOIN ZWAGROUPMEMBER g ON g.Z_PK = m.ZGROUPMEMBER
		LEFT JOIN ZWAMEDIAITEM mi ON mi.Z_PK = m.ZMEDIAITEM
		WHERE m.ZCHATSESSION = ? AND COALESCE(m.ZMESSAGEDATE,0) >= ?
		ORDER BY m.ZMESSAGEDATE, m.Z_PK`, chatPK, cutoff)
	if err != nil {
		return conversation{}, fmt.Errorf("read WhatsApp conversation %s: %w", jid, err)
	}
	defer rows.Close()
	conv := conversation{jid: jid, title: strings.TrimSpace(title), group: strings.HasSuffix(jid, "@g.us")}
	if conv.title == "" {
		conv.title = jid
	}
	for rows.Next() {
		var pk int64
		var stanza, text, sender, mediaTitle, vcard string
		var rawDate float64
		var fromMe int
		var typeCode int
		var lat, lon float64
		if err := rows.Scan(&pk, &stanza, &rawDate, &fromMe, &typeCode, &text, &sender, &mediaTitle, &vcard, &lat, &lon); err != nil {
			return conversation{}, fmt.Errorf("scan WhatsApp message: %w", err)
		}
		body := renderPayload(typeCode, text, mediaTitle, vcard, lat, lon)
		if body == "" {
			continue
		}
		id := strings.TrimSpace(stanza)
		if id == "" {
			id = "row:" + strconv.FormatInt(pk, 10)
		}
		conv.messages = append(conv.messages, message{id: id, at: cocoaSecondsToTime(rawDate), fromMe: fromMe != 0, sender: sender, body: body, typeCode: typeCode})
	}
	if err := rows.Err(); err != nil {
		return conversation{}, fmt.Errorf("iterate WhatsApp conversation %s: %w", jid, err)
	}
	return conv, nil
}

func cocoaSecondsToTime(raw float64) time.Time {
	if raw <= 0 {
		return time.Time{}
	}
	sec, frac := math.Modf(raw)
	return time.Unix(int64(sec)+cocoaUnixOffsetSeconds, int64(frac*1e9)).UTC()
}

func renderPayload(typeCode int, text, title, vcard string, lat, lon float64) string {
	if text = strings.TrimSpace(text); text != "" {
		return text
	}
	if vcard = strings.TrimSpace(vcard); vcard != "" {
		return "[contact: " + vcard + "]"
	}
	if validLocation(lat, lon) {
		return fmt.Sprintf("[location: %.6f, %.6f]", lat, lon)
	}
	if title = strings.TrimSpace(title); title != "" {
		return "[document: " + title + "]"
	}
	labels := map[int]string{
		1: "image", 2: "voice note", 3: "video", 4: "location", 5: "contact",
		6: "system event", 7: "document", 8: "animated media", 10: "deleted message",
		12: "sticker", 15: "contact", 19: "live location", 59: "reaction", 66: "poll",
	}
	if label := labels[typeCode]; label != "" {
		return "[" + label + "]"
	}
	return fmt.Sprintf("[WhatsApp message type %d]", typeCode)
}

func validLocation(lat, lon float64) bool {
	return math.Abs(lat) <= 90 && math.Abs(lon) <= 180 && (math.Abs(lat) > 0.000001 || math.Abs(lon) > 0.000001)
}

// MapConversationFn is the connector-specific inverted-truncation mapper. The
// newest transcript bytes survive, unlike memory.MapItem's head-biased budget.
func MapConversationFn() func(memory.Item, string, int) memory.MappedMemory {
	return func(it memory.Item, scope string, budget int) memory.MappedMemory {
		conv, ok := it.Payload.(conversation)
		if !ok {
			return memory.MappedMemory{StableID: memory.StableID(KindConversation, it.ProviderID), Type: "whatsapp", Title: it.Title, Body: it.Body, Scope: scope, Source: it.ProviderID, Provider: "whatsapp", ProviderID: it.ProviderID, ContentHash: memory.ContentHash(it.Title, it.Body)}
		}
		body, retained, truncated, original := renderConversation(conv, budget)
		lane := "personal_action"
		rationale := "direct conversation; eligible for verified obligation extraction"
		ownerParticipated := true
		if conv.group {
			ownerParticipated = ownerParticipatedInGroup(conv.messages)
			if ownerParticipated && groupHasSubstantiveContent(conv.messages) {
				lane = "intelligence"
				rationale = "owner participated in this group during the ingest window and the conversation has substantive content; informational only"
			} else if !ownerParticipated {
				lane = "none"
				rationale = "owner sent no messages in this group during the ingest window; volume alone never earns brief priority"
			} else {
				lane = "none"
				rationale = "owner participated, but the group contains only reactions, media, system events, or low-information chatter"
			}
		}
		participants := uniqueParticipants(conv)
		meta := map[string]any{
			"chat_kind": convKind(conv.group), "relevance_lane": lane, "owner_participated": ownerParticipated,
			"inclusion_rationale": rationale, "message_count": strconv.Itoa(len(conv.messages)),
			"participants": participants, "message_evidence_schema": 1,
		}
		if newest := newestTime(conv.messages); !newest.IsZero() {
			meta["occurred_at"] = newest.Format(time.RFC3339)
		}
		if evidence := evidenceMeta(memory.StableID(KindConversation, conv.jid), body, retained); len(evidence) > 0 {
			meta["message_evidence"] = evidence
		}
		metaJSON, _ := memory.CanonicalMeta(meta)
		mm := memory.MappedMemory{
			StableID: memory.StableID(KindConversation, conv.jid), Type: "whatsapp", Title: conv.title,
			Body: body, Scope: scope, Tags: []string{"whatsapp", lane}, Source: conv.jid,
			Provider: "whatsapp", ProviderID: conv.jid, ContentHash: memory.ContentHash(conv.title, body, metaJSON),
			Truncated: truncated, OriginalSize: original, IngestedSize: len(body), Meta: meta,
		}
		if newest := newestTime(conv.messages); !newest.IsZero() {
			mm.CreatedAt = newest.Format(time.RFC3339)
		}
		return mm
	}
}

func convKind(group bool) string {
	if group {
		return "group"
	}
	return "direct"
}

func renderConversation(conv conversation, budget int) (string, []message, bool, int) {
	lines := make([]string, 0, len(conv.messages))
	for _, m := range conv.messages {
		label := "Me"
		if !m.fromMe {
			label = strings.TrimSpace(m.sender)
			if label == "" {
				label = "Unknown"
			}
		}
		lines = append(lines, label+": "+m.body)
	}
	full := strings.Join(lines, "\n")
	if budget <= 0 || len(full) <= budget {
		return full, append([]message(nil), conv.messages...), false, len(full)
	}
	marker := "[earlier WhatsApp messages omitted]\n"
	remaining := budget - len(marker)
	if remaining < 0 {
		remaining = 0
	}
	kept := []message{}
	keptLines := []string{}
	used := 0
	for i := len(lines) - 1; i >= 0; i-- {
		need := len(lines[i])
		if len(keptLines) > 0 {
			need++
		}
		if used+need > remaining {
			break
		}
		used += need
		keptLines = append([]string{lines[i]}, keptLines...)
		kept = append([]message{conv.messages[i]}, kept...)
	}
	return marker + strings.Join(keptLines, "\n"), kept, true, len(full)
}

func newestTime(messages []message) time.Time {
	var newest time.Time
	for _, m := range messages {
		if m.at.After(newest) {
			newest = m.at
		}
	}
	return newest
}

func ownerParticipatedInGroup(messages []message) bool {
	for _, m := range messages {
		if m.fromMe {
			return true
		}
	}
	return false
}

func groupHasSubstantiveContent(messages []message) bool {
	lowInfo := map[string]bool{"ok": true, "okay": true, "yes": true, "no": true, "thanks": true, "thank you": true, "lol": true, "haha": true, "+1": true}
	for _, m := range messages {
		body := strings.TrimSpace(m.body)
		lower := strings.ToLower(strings.Trim(body, " .,!?:;"))
		if body == "" || m.typeCode == 59 || lowInfo[lower] {
			continue
		}
		if strings.HasPrefix(body, "[contact:") || strings.HasPrefix(body, "[location:") || strings.HasPrefix(body, "[document:") {
			return true
		}
		if !strings.HasPrefix(body, "[") && len([]rune(body)) >= 20 {
			return true
		}
	}
	return false
}

func uniqueParticipants(conv conversation) []map[string]string {
	if !conv.group {
		return []map[string]string{{"handle": conv.jid, "name": conv.title}}
	}
	seen := map[string]bool{}
	var out []map[string]string
	for _, m := range conv.messages {
		if m.fromMe {
			continue
		}
		name := strings.TrimSpace(m.sender)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, map[string]string{"handle": name, "name": name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["handle"] < out[j]["handle"] })
	return out
}

func evidenceMeta(parentID, body string, messages []message) []map[string]any {
	var out []map[string]any
	cursor := 0
	for _, m := range messages {
		label := "Me"
		if !m.fromMe {
			label = m.sender
			if strings.TrimSpace(label) == "" {
				label = "Unknown"
			}
		}
		line := label + ": " + m.body
		rel := strings.Index(body[cursor:], line)
		if rel < 0 {
			continue
		}
		start := cursor + rel
		end := start + len(line)
		cursor = end
		at := ""
		if !m.at.IsZero() {
			at = m.at.Format(time.RFC3339)
		}
		out = append(out, map[string]any{"evidence_ref": parentID + "#" + m.id, "at": at, "from_me": m.fromMe, "sender": label, "block_start": start, "block_end": end})
	}
	return out
}
