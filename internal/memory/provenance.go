package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
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
	"eywa_fact/",
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

// TentativeTag marks an agent write that saved without owner review. A tag
// travels through the vault file and the index with no schema change, and
// write_memory has no tags argument, so an agent cannot set or clear it.
const TentativeTag = "mora-tentative"

// TrustTentative is the Trust value of a record that carries TentativeTag.
const TrustTentative = "tentative"

// IsTentative reports whether nobody has confirmed the record.
func IsTentative(m Memory) bool {
	for _, tag := range m.Tags {
		if tag == TentativeTag {
			return true
		}
	}
	return false
}

// OwnerConfirmedTagPrefix starts the tag the keep action of `mora review` adds
// in the owner's own terminal. The rest of the tag is OwnerConfirmedDigest of
// the text the owner saw, so a copied tag or a later edit leaves a tag that
// counts for nothing. Commands that take tags refuse this prefix.
const OwnerConfirmedTagPrefix = "mora-owner-confirmed-"

// TrustOwnerConfirmed is the Trust value of a record IsOwnerConfirmed accepts.
const TrustOwnerConfirmed = "owner-confirmed"

// OwnerConfirmedFields lists, in order, every field of a note that a reader
// acts on or that its writer chose: what `mora review` shows the owner and
// what OwnerConfirmedDigest binds. Optional fields appear only when set; each
// flip condition is its own entry. The text is trimmed because the memory file
// codec trims it. created_at is bound because disposition.Project uses it for
// correction eligibility and precedence. Left out: the id and path, tags (no
// MCP tool can set them, and commands refuse the confirmed prefix), the
// connector fields an agent memory never has, and the read-time projections
// (decision status, disposition, corrections).
func OwnerConfirmedFields(m Memory) [][2]string {
	f := [][2]string{{"scope", m.Scope}, {"type", m.Type}, {"title", m.Title}}
	if m.Source != "" {
		f = append(f, [2]string{"source", m.Source})
	}
	if m.CreatedAt != "" {
		f = append(f, [2]string{"created at", m.CreatedAt})
	}
	if d := m.Decision; d != nil {
		f = append(f, [2]string{"decision as of", d.AsOf}, [2]string{"decision durability", d.Durability})
		for _, c := range d.FlipConditions {
			f = append(f, [2]string{"decision flip condition", c})
		}
		if d.ReviewBy != "" {
			f = append(f, [2]string{"decision review by", d.ReviewBy})
		}
		f = append(f, [2]string{"decision complete", strconv.FormatBool(d.Complete)})
	}
	// The same canonical JSON line the memory file stores.
	if meta, _ := CanonicalMeta(m.Meta); meta != "" {
		f = append(f, [2]string{"meta", meta})
	}
	return append(f, [2]string{"text", strings.TrimSpace(m.Text)})
}

// OwnerConfirmedDigest is the hex SHA-256 of OwnerConfirmedFields. Each label
// and value is length-prefixed, so no two field lists share an input.
func OwnerConfirmedDigest(m Memory) string {
	h := sha256.New()
	for _, f := range OwnerConfirmedFields(m) {
		fmt.Fprintf(h, "%d:%s%d:%s,", len(f[0]), f[0], len(f[1]), f[1])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// IsOwnerConfirmed reports whether the owner kept this exact record: an agent
// memory in the owner's own vault (not a subscribed share row), not tentative,
// with a confirmed tag that matches its current content. A stale or copied tag
// is not an error; it just does not count. deriveTrust checks tentative first,
// but the tentative check here stays: the context adoption line and the keep's
// own postcondition call this directly.
func IsOwnerConfirmed(m Memory) bool {
	if m.Owner != "" || IsTentative(m) || !strings.HasPrefix(m.ID, "mem_") || DeriveProvenance(m) != ProvenanceAuthored {
		return false
	}
	for _, tag := range m.Tags {
		// Hash only rows that carry the prefix; most rows on a read surface do not.
		if strings.HasPrefix(tag, OwnerConfirmedTagPrefix) && tag == OwnerConfirmedTagPrefix+OwnerConfirmedDigest(m) {
			return true
		}
	}
	return false
}

// WithoutOwnerConfirmedTags returns the record without owner-confirmed tags.
// Read paths call it on a row built from index columns when the live file
// could not be parsed: the tag matches the old indexed content, so the row
// must not read as owner-confirmed.
func WithoutOwnerConfirmedTags(m Memory) Memory {
	kept := make([]string, 0, len(m.Tags))
	for _, t := range m.Tags {
		if !strings.HasPrefix(t, OwnerConfirmedTagPrefix) {
			kept = append(kept, t)
		}
	}
	m.Tags = kept
	return m
}

func deriveTrust(m Memory) string {
	if IsTentative(m) {
		return TrustTentative
	}
	if IsOwnerConfirmed(m) {
		return TrustOwnerConfirmed
	}
	return ""
}

// WithProvenance returns a copy of the record with the derived kind attached,
// overwriting anything that was there. Read surfaces call this; nothing writes
// the result back to disk.
func WithProvenance(m Memory) Memory {
	m.Provenance = DeriveProvenance(m)
	m.Trust = deriveTrust(m)
	return m
}

// WithProvenanceAll is WithProvenance over a slice, in place.
func WithProvenanceAll(mems []Memory) []Memory {
	for i := range mems {
		mems[i].Provenance = DeriveProvenance(mems[i])
		mems[i].Trust = deriveTrust(mems[i])
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
