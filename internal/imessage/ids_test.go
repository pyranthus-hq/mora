package imessage

import (
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// TestIMessageStableID guards the two halves of the StableID/SafeFilename contract
// documented in ids.go (and CLAUDE.md): imessageStableID returns the chat GUID
// VERBATIM as "imessage_chat/<guid>", while the on-disk file name applies
// memory.SafeFilename, which maps only "/", ":" and " " to "_" — leaving the GUID's
// ";", "+", "-", "@" chars intact so any later findMemory lookup matches.
//
// The verbatim half overlaps with TestStableIDForm by design; the value here is the
// wantFile column, which exercises the SafeFilename mapping that the connector
// actually depends on (and which neither TestStableIDForm nor the original version of
// this test asserted). wantFile values are derived from the spec, not the impl.
func TestIMessageStableID(t *testing.T) {
	tests := []struct {
		name     string
		chatGUID string
		wantID   string // verbatim StableID
		wantFile string // SafeFilename form actually written to disk
	}{
		{
			name:     "phone number guid",
			chatGUID: "iMessage;-;+14155551234",
			wantID:   "imessage_chat/iMessage;-;+14155551234",
			wantFile: "imessage_chat_iMessage;-;+14155551234", // only the kind "/" is replaced
		},
		{
			name:     "email guid keeps @",
			chatGUID: "iMessage;-;user@example.com",
			wantID:   "imessage_chat/iMessage;-;user@example.com",
			wantFile: "imessage_chat_iMessage;-;user@example.com",
		},
		{
			name:     "group chat guid keeps +",
			chatGUID: "iMessage;+;chat123456789",
			wantID:   "imessage_chat/iMessage;+;chat123456789",
			wantFile: "imessage_chat_iMessage;+;chat123456789",
		},
		{
			name:     "space is sanitized in file name",
			chatGUID: "iMessage;-;John Doe",
			wantID:   "imessage_chat/iMessage;-;John Doe",
			wantFile: "imessage_chat_iMessage;-;John_Doe", // " " -> "_"
		},
		{
			name:     "colon is sanitized in file name",
			chatGUID: "iMessage;-;room:42",
			wantID:   "imessage_chat/iMessage;-;room:42",
			wantFile: "imessage_chat_iMessage;-;room_42", // ":" -> "_"
		},
		{
			name:     "embedded slash is sanitized in file name",
			chatGUID: "iMessage;-;a/b/c",
			wantID:   "imessage_chat/iMessage;-;a/b/c",
			wantFile: "imessage_chat_iMessage;-;a_b_c", // every "/" -> "_"
		},
		{
			name:     "empty guid",
			chatGUID: "",
			wantID:   "imessage_chat/",
			wantFile: "imessage_chat_",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := imessageStableID(tt.chatGUID)
			if id != tt.wantID {
				t.Errorf("imessageStableID(%q) = %q, want %q", tt.chatGUID, id, tt.wantID)
			}
			if file := memory.SafeFilename(id); file != tt.wantFile {
				t.Errorf("SafeFilename(%q) = %q, want %q", id, file, tt.wantFile)
			}
		})
	}
}
