package imessage

import "github.com/pyranthus-hq/mora/internal/memory"

// imessageStableID derives the conversation StableID from the chat GUID — provider
// identity only, never content (so a re-synced/edited conversation overwrites the
// same file instead of duplicating). Form: "imessage_chat/<guid>".
//
// GOTCHA (CLAUDE.md StableID-vs-SafeFilename): memory.SafeFilename maps "/"→"_",
// ":"→"_", " "→"_", so the on-disk file for imessage_chat/iMessage;-;+1415... is
// imessage_chat_iMessage;-;+1415....md. Any ID lookup (findMemory) must therefore
// match the SafeFilename form. Real chat GUIDs additionally contain ";", "+", "-",
// "@" (e.g. "iMessage;-;+14155551234", "iMessage;+;chat123", "...;you@email.com")
// — none of those are path separators, so they are filesystem-safe and the existing
// Replacer ("/" ":" " ") is sufficient; the only truly unsafe char "/" is covered.
func imessageStableID(chatGUID string) string {
	return memory.StableID(KindIMessageChat, chatGUID)
}
