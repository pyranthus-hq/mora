// Package memory holds the connector-agnostic memory model, identity helpers,
// resumable ingest loop, and sync-status state shared by all connectors. It
// imports no connector and is never imported by internal/mora-side write logic
// except as a type.
package memory

import (
	"context"
	"time"
)

// ItemKind is the provider object kind a fetched Item represents. It is a
// neutral, connector-extensible string: each connector defines its own kind
// constants (e.g. "gmail_thread", "calendar_event", "imessage_chat") and the
// shared MapItem derives a sane type/provider from any kind without a hard-coded
// per-connector switch.
type ItemKind string

// Attachment is metadata-plus-location: filename/MIME/size, and — when the body
// already exists on local disk (iMessage) — the absolute Path to it. Connectors
// never open the file; Path is consumed at the wiring boundary (internal/mora)
// to extract text from supported formats (PDF). Bytes are never carried here,
// and neither Path nor bytes ever appear in rendered vault output (IMSG-07's
// user-facing guarantee is unchanged).
type Attachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
	Path     string `json:"path,omitempty"`
}

// Item is a provider-agnostic fetched object. Connector adapters produce Items;
// MapItem turns them into MappedMemory.
type Item struct {
	Kind        ItemKind
	ProviderID  string // immutable provider id, e.g. gmail threadId, "calId/eventId"
	Title       string
	Body        string
	OccurredAt  time.Time // email date / event start; used for created_at
	Tags        []string
	Attachments []Attachment
	Deleted     bool           // trashed/cancelled -> tombstone
	Meta        map[string]any // structured identity/frontmatter (participants, from/to, occurred_at); persisted as one canonical JSON line

	// Payload is an optional connector-specific value carried opaquely through the
	// generic Ingest loop to a custom IngestParams.Map. It lets a connector whose
	// mapping needs richer structure than the flat Body/Title (e.g. iMessage's
	// per-message transcript with inverted truncation) reach its own mapper without
	// the shared MapItem path interpreting it. nil for connectors that use MapItem.
	Payload any
}

// FetchWindow bounds a snapshot fetch.
type FetchWindow struct {
	Since      time.Time
	Until      time.Time // zero = open-ended (Gmail uses Since only)
	Query      string    // provider query (gmail q=); optional
	Labels     []string  // gmail label IDs to include; optional
	CalendarID string    // calendar id; default "primary"
}

// Page is one page of fetched items plus a cursor for resume.
type Page struct {
	Items      []Item
	NextCursor string // provider page token; "" when no more pages
}

// Fetcher is the unit-test seam. Live impls live in each connector package; the
// generic Ingest loop drives any Fetcher.
type Fetcher interface {
	// FetchPage returns one page starting at cursor ("" = first page).
	FetchPage(kind ItemKind, w FetchWindow, cursor string) (Page, error)
}

// ContextFetcher is an optional extension for connectors that can interrupt an
// in-flight provider request. Ingest keeps legacy Fetcher implementations valid.
type ContextFetcher interface {
	FetchPageContext(context.Context, ItemKind, FetchWindow, string) (Page, error)
}
