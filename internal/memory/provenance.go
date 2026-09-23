package memory

import (
	"path/filepath"
	"strings"
)

// Provenance answers one question: who put this claim in the vault?
//
// It does not answer whether the claim is true, whether the user agreed with
// it, or whether anyone checked it. A connector record can be wrong. An agent
// note can be right. The point of the field is to stop a reader from treating
// an agent's own summary as if it were the user speaking.
const (
	// ProvenanceEvidence marks a record Mora copied from a connector with a
	// real outside sender or participant: a mail thread, a message, a calendar
	// entry.
	ProvenanceEvidence = "evidence"
	// ProvenanceDocument marks a file the filesystem connector copied from
	// disk. The file exists; who wrote it is unknown, and on this vault many
	// such files are an agent's own write-up, so a document is never called
	// evidence.
	ProvenanceDocument = "document"
	// ProvenanceAuthored marks a record an agent wrote. It is a lead to check,
	// not a fact to restate.
	ProvenanceAuthored = "authored"
)

// There is deliberately no kind for "the writer says the user confirmed this".
// Four attempts at reading that claim out of a free-text source label misread
// ordinary English in both directions: "user confirmation is pending" is not a
// claim, "user-confirmed no changes needed" is one, and a rule that gets either
// wrong invents user authority where none exists, which is the failure this
// field exists to prevent. The label is quoted to the reader instead, shortened
// but never reworded, so they can judge the writer's words for themselves.

// connectorIDPrefixes are the id prefixes the connectors stamp on the records
// they ingest (internal/mora/ingest.go and the per-connector mappers).
var connectorIDPrefixes = []string{
	"gmail_thread/",
	"imessage_chat/",
	"calendar_event/",
	"applecal_event/",
	"whatsapp_conversation/",
	// internal/githubissues/github.go overrides the stable id to this shape,
	// so "github_issue/" would never match a real record.
	"github:",
}

// agentNoteSources are the filesystem sources known to mirror an agent's own
// notes into the vault. Those files sit in the connector tree, but they are
// still an agent writing about the user, so they stay authored. Every other
// file the filesystem connector ingests is a document: it exists, but nothing
// here can tell a human's note from an agent's write-up, so it is never
// promoted to evidence.
var agentNoteSources = []string{
	"claude-memory",
	"claude-project-memory",
	"claude-remember",
	"codex-memory",
}

// DeriveProvenance reads the record and returns one of the three kinds. It is
// deterministic, uses only fields Mora itself sets during ingest or write, and
// never trusts a Provenance value already on the record.
func DeriveProvenance(m Memory) string {
	// The id is the one field no writer controls: Mora stamps mem_ on agent
	// memories and each connector stamps its own prefix. It is checked first
	// so no tag, label or injected frontmatter line can move a record across
	// that line in either direction.
	if hasConnectorID(m) {
		return ProvenanceEvidence
	}
	if isAgentMemory(m) {
		return ProvenanceAuthored
	}
	if isAgentNoteMirror(m) {
		return ProvenanceAuthored
	}
	if isFilesystemDocument(m) {
		return ProvenanceDocument
	}
	if isConnectorRecord(m) {
		return ProvenanceEvidence
	}
	return ProvenanceAuthored
}

// hasConnectorID reports whether a connector stamped the record's id.
func hasConnectorID(m Memory) bool {
	for _, prefix := range connectorIDPrefixes {
		if strings.HasPrefix(m.ID, prefix) {
			return true
		}
	}
	return false
}

// isAgentMemory reports whether the record is a memory an agent wrote through
// Mora: a mem_ id, or a file under the vault's memories tree. Such a record is
// never evidence, whatever provider a hand-edited or injected frontmatter line
// claims, because the memory file format writes tags unescaped and a tag can
// smuggle a provider line into the file.
func isAgentMemory(m Memory) bool {
	if strings.HasPrefix(m.ID, "mem_") {
		return true
	}
	slashed := leadingSlash(m.Path)
	return strings.Contains(slashed, "/memories/")
}

// leadingSlash normalises a path for tree matching so a relative path and an
// absolute one match the same folder names.
func leadingSlash(path string) string {
	return "/" + strings.TrimPrefix(filepath.ToSlash(path), "/")
}

// isFilesystemDocument reports whether the filesystem connector copied the
// record from disk. Those records carry no provider once hydrated from the
// index, so the path and the "filesystem" tag are the signals.
func isFilesystemDocument(m Memory) bool {
	if m.Provider == "filesystem" {
		return true
	}
	slashed := filepath.ToSlash(m.Path)
	if strings.Contains(slashed, "/sources/filesystem/") || strings.HasPrefix(slashed, "sources/filesystem/") {
		return true
	}
	for _, tag := range m.Tags {
		if strings.EqualFold(tag, "filesystem") {
			return true
		}
	}
	return false
}

// WithProvenance returns a copy of the record with the derived kind attached,
// overwriting anything that was there. Read surfaces call this; nothing writes
// the result back to disk.
func WithProvenance(m Memory) Memory {
	m.Provenance = DeriveProvenance(m)
	return m
}

// WithProvenanceAll is WithProvenance over a slice, in place.
func WithProvenanceAll(mems []Memory) []Memory {
	for i := range mems {
		mems[i].Provenance = DeriveProvenance(mems[i])
	}
	return mems
}

func isConnectorRecord(m Memory) bool {
	// A connector writes into the vault's sources tree; an agent memory lives
	// under memories. Filesystem records were already taken by
	// isFilesystemDocument, so whatever is left in the tree came from a
	// connector with an outside sender.
	if underVaultSources(m.Path) {
		return true
	}
	// A provider name means a connector produced the row even when the record
	// was hydrated from the index rather than read off disk.
	return m.Provider != "" && m.Provider != "filesystem"
}

// underVaultSources reports whether the path sits in the vault's sources tree.
// It matches the directory name rather than the vault root, because the
// derivation runs where the config is not in hand.
func underVaultSources(path string) bool {
	if path == "" {
		return false
	}
	slashed := filepath.ToSlash(path)
	return strings.Contains(slashed, "/sources/") || strings.HasPrefix(slashed, "sources/")
}

func isAgentNoteMirror(m Memory) bool {
	for _, tag := range m.Tags {
		for _, name := range agentNoteSources {
			if strings.EqualFold(tag, name) {
				return true
			}
		}
	}
	slashed := leadingSlash(m.Path)
	for _, name := range agentNoteSources {
		if strings.Contains(slashed, "/sources/filesystem/"+name+"/") {
			return true
		}
	}
	return false
}
