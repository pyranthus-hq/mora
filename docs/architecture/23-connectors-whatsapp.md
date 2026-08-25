# WhatsApp connector

## Boundary

The v1 WhatsApp connector reads only WhatsApp Desktop's local macOS Core Data
store:

`~/Library/Group Containers/group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite`

`internal/whatsapp` imports `internal/memory` and the pure-Go
`modernc.org/sqlite` driver. It has no network imports and never opens
`Axolotl.sqlite`, which contains Signal-protocol key material. The reader
interface is the backend seam; v1 ships exactly one backend and never combines
stores.

The DSN uses `mode=ro`, `query_only(1)`, and a bounded `busy_timeout`. It does
not use `immutable=1`, because the live WAL can contain the newest messages.
Construction forces real schema reads. Missing tables or columns fail closed as
`unsupported ChatStorage.sqlite schema`; an unreadable store is surfaced by
ingest and `mora doctor` as a Full Disk Access/readiness failure.

## Mapping

`LiveFetcher` pages chats by `ZWACHATSESSION.Z_PK`. Chat kind is derived only
from the JID suffix: `@g.us` is a group and `@s.whatsapp.net` is a direct chat.
Each chat maps to one `whatsapp_conversation/<jid>` memory. Message rows join
`ZWAGROUPMEMBER` for group attribution and `ZWAMEDIAITEM` for recoverable
contact, location, and document structure.

The custom mapper keeps the newest transcript blocks when the body budget is
exceeded. Empty text is never silently discarded: known media types render as
typed placeholders and unknown numeric types remain explicit. `CreatedAt` and
`meta.occurred_at` use the newest retained conversation event. Per-message
evidence refs use `ZSTANZAID`, falling back to the local Core Data row identity
when WhatsApp did not provide a stanza id.

## Two-lane relevance gate

The mapper records `relevance_lane` and `inclusion_rationale` in memory metadata.

- `personal_action`: direct conversations. These use Mora's existing
  participant- and message-evidence-based commitment classifier.
- `intelligence`: groups where the owner sent at least one message during the
  ingest window and the chat has substantive text or recoverable contact,
  location, or document structure. They may appear in the brief only as
  informational context. Raw incoming volume is never a relevance signal.
- `none`: groups with no owner-authored message in the window, plus
  reaction-only, media-only, system-only, or low-information group changes.
  The digest advances its watermark over these records without displaying them.

Both commitment materialization and urgent-shelf classification reject the
`intelligence` lane before inspecting message text. Thus no group message can
create a task or urgent item. Every displayed WhatsApp item includes the lane
and rationale in both JSON and Markdown digest forms.

## Wiring and health

The catalog type and memory provider are both `whatsapp`. The connector is
macOS-only, default-disabled, and uses the consent-first flow:

```text
mora connectors enable whatsapp
mora ingest run --source whatsapp
```

The default local window is 90 days; negative `SinceDays` selects all locally
available history. Ingest runs through `memory.Ingest`, `writeMappedMemory`, and
`persistSyncStatus`. Status is stored as `sync/whatsapp-<name>.json`, so sync
status, doctor, and digest health share the same honest-snapshot receipt.

Hermetic tests build a real SQLite fixture and exercise the production query,
decode, mapping, placeholder, cursor, schema-failure, truncation, and gate paths.
