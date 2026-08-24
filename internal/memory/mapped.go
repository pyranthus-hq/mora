package memory

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// MappedMemory is the plain hand-off struct. internal/mora converts it to its
// Memory type (avoids an import cycle). Field names mirror Mora frontmatter.
type MappedMemory struct {
	StableID     string
	Scope        string
	Type         string // "email" | "event" | connector-derived
	Title        string
	Body         string
	Tags         []string
	Source       string // provider id (human-traceable)
	CreatedAt    string // RFC3339
	Provider     string // "gmail" | "calendar" | connector-derived
	Account      string // multi-account label (e.g. "work"); empty = default account
	ProviderID   string
	ContentHash  string
	LastSynced   string // RFC3339, set by ingest at write time
	Truncated    bool
	OriginalSize int
	IngestedSize int
	DeletedAt    string // RFC3339 when tombstoned, else ""
	Attachments  []Attachment
	Meta         map[string]any
}

// contentHashWithMeta folds the canonical Meta into the content hash only when it
// is non-empty, so memories without identity data keep their exact legacy
// two-part hash (no spurious rewrite of every pre-Meta source file).
func contentHashWithMeta(title, body, metaJSON string) string {
	if metaJSON == "" {
		return ContentHash(title, body)
	}
	return ContentHash(title, body, metaJSON)
}

// volatileMetaKeys are Meta fields that describe an item's transient STATE rather
// than its content, so they are persisted but EXCLUDED from the content hash. Gmail
// actionability labels (issue #62) flip as a thread is read/starred/reclassified;
// hashing them would re-surface an unchanged thread as "[updated]" on every toggle.
var volatileMetaKeys = map[string]bool{"labels": true, "retrieved_at": true, "ingest_correlation_id": true}

// hashMeta returns meta with the volatile (state-only) keys removed, so the content
// hash reflects the item's CONTENT, not its read/star state. It returns the SAME map
// (no copy) when no volatile key is present — the common path stays allocation-free
// and byte-identical to the pre-#62 hash.
func hashMeta(meta map[string]any) map[string]any {
	has := false
	for k := range volatileMetaKeys {
		if _, ok := meta[k]; ok {
			has = true
			break
		}
	}
	if !has {
		return meta
	}
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		if !volatileMetaKeys[k] {
			out[k] = v
		}
	}
	return out
}

// CanonicalMeta encodes Meta as the single deterministic JSON line mora persists
// (`meta: {...}`) and folds into ContentHash. json.Marshal of a map emits
// sorted keys on one line, so the bytes are stable across runs and independent of
// insertion order. Empty/nil Meta canonicalizes to "" (no meta line, no hash
// contribution) so memories without identity data keep their legacy hash.
func CanonicalMeta(meta map[string]any) (string, error) {
	if len(meta) == 0 {
		return "", nil
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// kindMapping records the (type, provider) a registered ItemKind maps to.
type kindMapping struct {
	typ      string
	provider string
}

// kindRegistry lets connectors register the (type, provider) for their kinds
// without editing this package. Unregistered kinds fall back to a sane default
// derived from the kind string (Type "source", Provider = the kind itself) so a
// new connector works before it registers — and the registry stays the override.
var kindRegistry = map[ItemKind]kindMapping{
	"gmail_thread":   {typ: "email", provider: "gmail"},
	"calendar_event": {typ: "event", provider: "calendar"},
}

// RegisterKind associates an ItemKind with the Type/Provider MapItem should emit.
// Connectors call this from an init() so their kinds map without editing memory.
func RegisterKind(kind ItemKind, typ, provider string) {
	kindRegistry[kind] = kindMapping{typ: typ, provider: provider}
}

// kindToType maps a kind to (type, provider). Registered kinds use their
// registration; unknown kinds derive a connector-agnostic default so iMessage's
// KindIMessageChat (or any future kind) works without editing this package.
func kindToType(k ItemKind) (typ, provider string) {
	if m, ok := kindRegistry[k]; ok {
		return m.typ, m.provider
	}
	return "source", string(k)
}

// MapItem converts a fetched Item into a MappedMemory, applying a byte budget.
// Truncation keeps from the START of the body (Gmail behavior); connectors that
// need a different keep-direction build their own mapper reusing this struct.
func MapItem(it Item, scope string, bodyBudget int) MappedMemory {
	typ, provider := kindToType(it.Kind)
	body := it.Body
	orig := len(body)
	truncated := false
	if bodyBudget > 0 && orig > bodyBudget {
		body = body[:bodyBudget]
		truncated = true
	}
	created := it.OccurredAt
	if created.IsZero() {
		created = time.Now()
	}

	// Defensive copy of Tags, then sort.
	tags := append([]string(nil), it.Tags...)
	sort.Strings(tags)

	// Defensive copy of Attachments.
	var attachments []Attachment
	if it.Attachments != nil {
		attachments = make([]Attachment, len(it.Attachments))
		copy(attachments, it.Attachments)
	}

	// Defensive copy of Meta.
	var meta map[string]any
	if it.Meta != nil {
		meta = make(map[string]any, len(it.Meta))
		for k, v := range it.Meta {
			meta[k] = v
		}
	}
	// Fold the canonical Meta into the content hash so a change in structured
	// identity data (new participant, recovered address) rewrites the file instead
	// of being skipped by the content-hash guard. Empty Meta contributes nothing,
	// preserving the legacy two-part hash for memories without identity data. Volatile
	// state keys (Gmail labels — issue #62) are stripped BEFORE hashing so a read/star
	// toggle never churns the delta, while the full Meta (with labels) is still
	// persisted below.
	metaJSON, _ := CanonicalMeta(hashMeta(meta))

	m := MappedMemory{
		StableID:     StableID(it.Kind, it.ProviderID),
		Scope:        scope,
		Type:         typ,
		Title:        strings.TrimSpace(it.Title),
		Body:         body,
		Tags:         tags,
		Source:       it.ProviderID,
		CreatedAt:    created.UTC().Format(time.RFC3339),
		Provider:     provider,
		ProviderID:   it.ProviderID,
		ContentHash:  contentHashWithMeta(it.Title, it.Body, metaJSON),
		Truncated:    truncated,
		OriginalSize: orig,
		IngestedSize: len(body),
		Attachments:  attachments,
		Meta:         meta,
	}
	if it.Deleted {
		m.DeletedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return m
}
