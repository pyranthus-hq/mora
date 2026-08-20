package memory

// Memory is Mora's shared memory record across persistence, indexing, retrieval, and API surfaces.
type Memory struct {
	ID          string   `json:"id"`
	Scope       string   `json:"scope"`
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Tags        []string `json:"tags"`
	Source      string   `json:"source"`
	CreatedAt   string   `json:"created_at"`
	Path        string   `json:"path"`
	Text        string   `json:"text,omitempty"`
	Score       float64  `json:"score,omitempty"`
	Provider    string   `json:"provider,omitempty"`
	Account     string   `json:"account,omitempty"` // multi-account label; composes the "provider:account" instance key
	ProviderID  string   `json:"provider_id,omitempty"`
	ContentHash string   `json:"content_hash,omitempty"`
	LastSynced  string   `json:"last_synced,omitempty"`
	Truncated   bool     `json:"truncated,omitempty"`
	DeletedAt   string   `json:"deleted_at,omitempty"`
	// EventStart, SourceCreatedAt, and IndexedAt split the three distinct instants
	// `created_at` conflated on a browse row (#218): when the thing happens, when
	// the source object was created at its provider, and when Mora wrote the
	// memory into the vault. They are DERIVED at read time by decorateBrowseRecency
	// (`recency.go`), never persisted (renderMemory writes no such frontmatter),
	// and populated only on the MCP `list_memory` rows — omitempty therefore keeps
	// every other payload byte-identical, which the MCP budget gate depends on. An
	// absent field means Mora cannot derive that instant honestly, never that it
	// substituted another one.
	EventStart      string `json:"event_start,omitempty"`
	SourceCreatedAt string `json:"source_created_at,omitempty"`
	IndexedAt       string `json:"indexed_at,omitempty"`
	// Decision carries the validity contract for a decision memory. It is
	// persisted in Markdown frontmatter and remains visible on local read
	// surfaces. Legacy decisions are represented as incomplete/provisional
	// rather than silently treated as current law.
	Decision *DecisionValidity `json:"decision,omitempty"`
	// DecisionStatus is derived at read time. It is not persisted: an expired
	// review_by becomes needs_review as the clock advances without a vault write.
	DecisionStatus string `json:"decision_status,omitempty"`
	// Owner attributes a result from a SHARED corpus (`mora share subscribe`)
	// with the subscriber-chosen subscription name. Never persisted to disk and
	// always empty for the user's own memories — omitempty keeps local-only
	// payloads byte-identical (the MCP budget gate depends on that).
	Owner string `json:"owner,omitempty"`
	// Meta is structured identity/frontmatter (participants, from/to, occurred_at),
	// persisted as one canonical JSON line (`meta: {...}`). Powers the entity graph;
	// the graph compiler reads it deterministically (no NER).
	Meta map[string]any `json:"meta,omitempty"`
	// Corroborating holds compact refs to other memories the vault believes
	// describe the SAME real-world event as this one (issue #237). Populated
	// at result-assembly time by the shared retrieval primitives
	// (clusterAndTruncate, cluster.go) — search_memory, think, context_memory,
	// and CLI `mora context` all propagate it from there to their own output
	// shapes (see think.go's ThinkEvidence.Corroborating, mcp.go's top-level
	// context_memory "corroborating", and entities.go's
	// contextItemJSON.Corroborating). Never persisted and never set on
	// read_memory/list_memory — omitempty keeps every other read surface
	// byte-identical.
	Corroborating []CorroboratingRef `json:"corroborating,omitempty"`
	// LaterRelatedEvidence is a derived retrieval hint, never persisted. It
	// points from an older result to the newest deeper-pool record with a
	// strongly matching title in the same scope. It deliberately says related,
	// not superseded: only Teach governance may assert an actual supersession.
	LaterRelatedEvidence *LaterRelatedEvidence `json:"later_related_evidence,omitempty"`
	// Evidence is the compact Gmail evidence-segment receipt (issue #243,
	// DQ5 §2): the STRONGEST query-matching derived segment's identity +
	// snippet, attached at search_memory result-assembly time
	// (gmail_segments_search.go's attachGmailSegmentEvidence) when the row's
	// underlying parent has at least one query-matching segment. Never
	// persisted and never set on read_memory/list_memory — omitempty keeps
	// every other read surface and every non-participating memory's search
	// row byte-identical (frozen interface #5).
	Evidence *GmailSegmentEvidence `json:"evidence,omitempty"`
}

// CorroboratingRef is a compact citation for a corroborating record.
type CorroboratingRef struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
}

// LaterRelatedEvidence is an honest pointer to a newer strongly-related record.
type LaterRelatedEvidence struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Source    string `json:"source"`
	IndexedAt string `json:"indexed_at"`
}

// GmailSegmentEvidence is the strongest query-matching derived Gmail segment receipt.
type GmailSegmentEvidence struct {
	EvidenceRef string `json:"evidence_ref"`
	Sender      string `json:"sender"`
	At          string `json:"at"`
	Direction   string `json:"direction,omitempty"`
	Snippet     string `json:"snippet"`
}

// Source is a persisted connector registration and its provider-specific read scope.
type Source struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Scope     string   `json:"scope"`
	Path      string   `json:"path,omitempty"`
	Label     string   `json:"label,omitempty"`
	Calendar  string   `json:"calendar,omitempty"`
	FolderID  string   `json:"folder_id,omitempty"`
	SinceDays int      `json:"since_days,omitempty"` // 0 => default per type
	LabelIDs  []string `json:"label_ids,omitempty"`
	DocsOnly  bool     `json:"docs_only,omitempty"` // filesystem: docs/metadata only
	Account   string   `json:"account,omitempty"`   // google: account label for multi-mailbox (empty = the default/legacy account)
	Email     string   `json:"email,omitempty"`     // google: the signed-in address, stamped at connect — the same-account re-auth guard reads it
	Enabled   *bool    `json:"enabled,omitempty"`   // nil => legacy source, grandfather to true (D-12); *false => opt-in disabled (D-11)
	CreatedAt string   `json:"created_at"`

	// DenyContacts / DenyConversations scope iMessage ingest (IMSG-06/D-07/D-08).
	// Persisted on the imessage source row in sources.json (no new config file),
	// matching Phase 1's no-new-file precedent. Empty = include everyone.
	DenyContacts      []string `json:"deny_contacts,omitempty"`
	DenyConversations []string `json:"deny_conversations,omitempty"`
	Repositories      []string `json:"repositories,omitempty"` // github: explicit owner/repo allowlist
}

// IsEnabled reports explicit connector enablement; nil preserves legacy disabled semantics.
func (s Source) IsEnabled() bool { return s.Enabled != nil && *s.Enabled }
