// Package imessage ingests local iMessage conversations into Mora-renderable
// memories. It must NOT import internal/mora (mora imports imessage). It returns
// plain structs. The connector-agnostic memory model, identity helpers, resumable
// ingest loop, and sync-status state live in internal/memory; this package reuses
// those shapes and defines only its own ItemKind constant + LiveFetcher.
//
// HARD RULE: internal/imessage must NOT import internal/mora and must NOT import
// internal/google (connector-must-not-import-connector). It also makes ZERO
// network calls — it reads only the local ~/Library/Messages/chat.db read-only.
package imessage

import "github.com/pyranthus-hq/mora/internal/memory"

// ItemKind, Item, Attachment, FetchWindow, Page, and Fetcher are the
// connector-facing seam shapes defined in internal/memory so the generic Ingest
// loop references no specific connector. Reused here as thin aliases.
type (
	ItemKind    = memory.ItemKind
	Item        = memory.Item
	Attachment  = memory.Attachment
	FetchWindow = memory.FetchWindow
	Page        = memory.Page
	Fetcher     = memory.Fetcher
)

// KindIMessageChat is this connector's object kind: one conversation per chat.
// FetchWindow.Since is the lookback cutoff (D-06, default 90 days at the wiring
// boundary; all-time when extended).
const KindIMessageChat ItemKind = "imessage_chat"

// DenyList scopes ingestion (IMSG-06/D-07/D-08). It is populated mora-side from the
// setup menu (Plan 02-05) and honored during fetch:
//
//   - Contacts: phone/email handles. A denied handle excludes a conversation ONLY
//     when it is the SOLE counterparty — i.e. a 1:1 chat whose single non-self
//     participant is that handle (D-08 sole-counterparty rule). A denied handle who
//     is merely one member of a multi-party group does NOT drop the group, and the
//     group's transcript is never per-message-stripped; group exclusion is
//     thread-granularity only (deny the conversation instead).
//   - Conversations: conversation display names. A conversation whose explicit
//     display name (or chat identifier) matches is skipped entirely
//     (case-insensitive exact match).
type DenyList struct {
	Contacts      []string
	Conversations []string
}
