// Package google ingests Gmail and Calendar data into Mora-renderable memories.
// It must NOT import internal/mora (mora imports google). It returns plain structs.
// The connector-agnostic memory model, identity helpers, resumable ingest loop,
// and sync-status state live in internal/memory; this package re-exports thin
// aliases so the gmail/calendar adapters read unchanged.
package google

import "github.com/pyranthus-hq/mora/internal/memory"

// ItemKind, Item, Attachment, FetchWindow, Page, and Fetcher are the
// connector-facing seam shapes. They are defined in internal/memory so the
// generic Ingest loop references no specific connector; aliased here so the
// gmail/calendar adapters keep using google.Item etc.
type (
	ItemKind    = memory.ItemKind
	Item        = memory.Item
	Attachment  = memory.Attachment
	FetchWindow = memory.FetchWindow
	Page        = memory.Page
	Fetcher     = memory.Fetcher
)

// Gmail/Calendar kind constants stay in the connector package.
const (
	KindGmailThread ItemKind = "gmail_thread"
	KindCalEvent    ItemKind = "calendar_event"
)
