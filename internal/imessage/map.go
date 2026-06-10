package imessage

import (
	"strconv"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// convInput is the structured per-conversation input the mapper renders into a
// MappedMemory. chatdb.assembleConversation (same package) builds it so the
// per-message structure (date / is_from_me / sender handle / text / attachment
// metadata) survives intact to the renderer — a flat memory.Item Body would lose
// it. The mapper, not the renderer, owns the MappedMemory shape (identity, Meta,
// truncation fields).
type convInput struct {
	guid        string          // chat GUID → provider identity (StableID/ProviderID)
	chat        conversation    // title-relevant shape (display name, participants, isGroup)
	messages    []renderMessage // every rendered message (renderer sorts chronologically)
	attachments []Attachment    // metadata-only attachment markers across the conversation (IMSG-07)
}

// imessageProvider / imessageType are the frontmatter provider/type for an iMessage
// conversation memory (mirrors google's "gmail"/"email"). Set directly because this
// mapper bypasses memory.MapItem (D-03 inverted truncation), so the kind-registry
// default ("source"/"imessage_chat") never applies here.
const (
	imessageProvider = "imessage"
	imessageType     = "imessage"
)

// mapConversation turns one structured conversation into exactly one MappedMemory
// (IMSG-03, D-04). It mirrors memory.MapItem's STRUCTURE but INVERTS truncation:
// renderBody keeps the NEWEST messages with the marker at the TOP (D-03), so this
// mapper must NOT call memory.MapItem (its start-keep byte slice). budget <= 0 is
// unbounded.
//
// ContentHash(Title, Body) makes an untouched conversation hash identically across
// syncs so writeMappedMemory skips the rewrite (D-05). Scope is intentionally left
// for the wiring boundary (ingestIMessage knows the Source's scope).
func mapConversation(c convInput, r *Resolver, budget int) memory.MappedMemory {
	title := renderTitle(c.chat, r)
	body, res := renderBody(c.messages, r, budget)

	// Defensive copy of attachment metadata (filename/MIME/size only — never the
	// bytes or on-disk path, IMSG-07).
	var attachments []Attachment
	if len(c.attachments) > 0 {
		attachments = make([]Attachment, len(c.attachments))
		copy(attachments, c.attachments)
	}

	meta := conversationMeta(c, r)
	// Fold the canonical participant Meta into the hash so a recovered name or a
	// new participant rewrites the file instead of being skipped (D-05 still holds
	// for an untouched conversation: same title+body+meta -> same hash).
	metaJSON, _ := memory.CanonicalMeta(meta)
	contentHash := memory.ContentHash(title, body)
	if metaJSON != "" {
		contentHash = memory.ContentHash(title, body, metaJSON)
	}

	mm := memory.MappedMemory{
		StableID:     memory.StableID(KindIMessageChat, c.guid),
		Type:         imessageType,
		Title:        title,
		Body:         body,
		Tags:         []string{"imessage"},
		Source:       c.guid,
		Provider:     imessageProvider,
		ProviderID:   c.guid,
		ContentHash:  contentHash,
		Truncated:    res.Truncated,
		OriginalSize: res.OriginalSize,
		IngestedSize: res.IngestedSize,
		Attachments:  attachments,
		Meta:         meta,
	}
	if created := newestMessageTime(c.messages); !created.IsZero() {
		mm.CreatedAt = created.UTC().Format(time.RFC3339)
	}
	return mm
}

// MapConversationFn returns an memory.IngestParams.Map adapter bound to a resolver.
// It recovers the structured convInput a LiveFetcher attached to Item.Payload and
// renders it (names, day-grouping, inverted truncation) into a MappedMemory, setting
// Scope from the ingest. An Item lacking a convInput payload degrades to a minimal
// memory from its flat fields rather than panicking (defensive; never fabricated).
func MapConversationFn(r *Resolver) func(memory.Item, string, int) memory.MappedMemory {
	return func(it memory.Item, scope string, budget int) memory.MappedMemory {
		c, ok := it.Payload.(convInput)
		if !ok {
			mm := memory.MappedMemory{
				StableID:    memory.StableID(it.Kind, it.ProviderID),
				Type:        imessageType,
				Title:       it.Title,
				Body:        it.Body,
				Source:      it.ProviderID,
				Provider:    imessageProvider,
				ProviderID:  it.ProviderID,
				ContentHash: memory.ContentHash(it.Title, it.Body),
				Scope:       scope,
			}
			return mm
		}
		mm := mapConversation(c, r, budget)
		mm.Scope = scope
		return mm
	}
}

// newestMessageTime returns the most-recent message timestamp — the conversation's
// recency anchor used for CreatedAt (D-03). Zero when there are no messages (never a
// fabricated date).
func newestMessageTime(msgs []renderMessage) time.Time {
	var newest time.Time
	for _, m := range msgs {
		if m.date.After(newest) {
			newest = m.date
		}
	}
	return newest
}

// conversationMeta carries the participant handles and their resolved names for the
// Phase 8 entity graph (no NER) — identity metadata only, NEVER message bytes. The
// two parallel comma-joined lists keep the handle→name correspondence positional.
func conversationMeta(c convInput, r *Resolver) map[string]any {
	handles := participantHandles(c.chat)
	// Structured handle↔name pairs (S3), NOT two parallel comma-joined lists: a
	// resolved name containing a comma broke the positional correspondence the old
	// format relied on. Each pair is self-contained, so the graph (S4) resolves
	// handle→name without re-splitting.
	pairs := make([]map[string]string, 0, len(handles))
	for _, h := range handles {
		pairs = append(pairs, map[string]string{"handle": h, "name": r.Resolve(h)})
	}
	meta := map[string]any{
		"participants":  pairs,
		"message_count": strconv.Itoa(len(c.messages)),
	}
	if t := newestMessageTime(c.messages); !t.IsZero() {
		meta["occurred_at"] = t.UTC().Format(time.RFC3339)
	}
	return meta
}

// participantHandles is the conversation's other-party handles in chat.db order: the
// group participants, else the 1:1 identifier. Used for the Meta identity pairs.
func participantHandles(c conversation) []string {
	if len(c.participants) > 0 {
		return append([]string(nil), c.participants...)
	}
	if c.identifier != "" {
		return []string{c.identifier}
	}
	return nil
}
