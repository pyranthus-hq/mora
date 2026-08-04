package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"
)

// Issue #243 — Gmail messages as derived evidence segments with stable
// sub-citations. This file owns the derived projection: schema, fail-closed
// derivation, and the rebuild/upsert plumbing that keeps gmail_segments +
// gmail_segments_fts + gmail_segment_diagnostics in parity with the vault.
// See gmail_segments_contract_test.go for the frozen contract this
// implements (§1 interface, §2 design questions).
//
// The projection is fully disposable, like every other derived index table:
// rebuildIndexWithPolicy DROPs and repopulates it inside the SAME transaction
// as memories/memories_fts, so `mora index rebuild` (or a deleted index.db)
// reproduces it byte-for-byte from the vault Markdown alone. Segments are
// derived ONLY from meta.messages + the "\n\n---\n\n"-joined body — never
// fabricated, never misattributed — and a memory that fails any fail-closed
// check gets ZERO segments plus a content-free diagnostic row, while the
// parent memory itself stays indexed normally.

// gmailBodySeparator is the exact per-message join boundary gmail.go's
// gmailThreadToItem writes ("\n\n---\n\n" between each "From: %s\n\n%s"
// block). Segment derivation splits the RAW (unstripped) memory text on this
// literal exactly once and uses that same ordered block slice for count,
// sender validation, and text. Keeping one canonical alignment path matters:
// stripFromLine trims whitespace after an empty first message, which can erase
// a real join boundary before a later literal separator restores the apparent
// count and shifts one sender's body under another message's evidence_ref.
const gmailBodySeparator = "\n\n---\n\n"

// Diagnostic reasons, in the exact priority order DQ2 (§2) pins: a fixture
// that satisfies more than one is reported under the FIRST match.
const (
	gmailSegDiagTruncated        = "truncated"
	gmailSegDiagCountMismatch    = "count_mismatch"
	gmailSegDiagOrderingMismatch = "ordering_mismatch"
	gmailSegDiagMalformedRef     = "malformed_ref"
	// gmailSegDiagDuplicateRef (P0 fix, reviewer-reproduced): two or more
	// messages in the SAME memory declaring the IDENTICAL MessageRef would
	// otherwise collide on gmail_segments' evidence_ref PRIMARY KEY — a real
	// SQL UNIQUE-constraint error that, unless caught here before any INSERT
	// runs, aborts the WHOLE rebuild transaction (not just this memory).
	// Checked LAST, after every ref is already confirmed individually
	// well-formed (malformed_ref) — "duplicate" is a property of the SET of
	// refs, not any one ref, so it gets its own distinct reason rather than
	// folding into malformed_ref.
	gmailSegDiagDuplicateRef = "duplicate_ref"
)

// gmailSegmentRow is one derived evidence segment — the in-memory shape
// gmail_segments persists. DQ1 (§2) pins the column set/order.
type gmailSegmentRow struct {
	EvidenceRef string
	MemoryID    string
	Sender      string
	Recipients  []string
	At          string
	BlockRefs   []string
	Text        string
}

// gmailSegmentDiagnostic is one gmail_segment_diagnostics row — counts/ids
// ONLY, never memory content (DQ2, §2).
type gmailSegmentDiagnostic struct {
	MemoryID  string
	Reason    string
	MetaCount int
	BodyCount int
}

// deriveGmailSegments computes the fail-closed evidence-segment projection
// for one memory. It returns (nil, nil) when m is not a Gmail memory, or is
// a legacy Gmail memory carrying no meta.messages key at all (the pre-#243
// shape) — neither case is a refusal, so neither gets a diagnostic row.
// Otherwise it returns EITHER a non-empty, well-formed row set OR exactly
// one diagnostic explaining the whole-memory refusal — never both, and
// never a partial/misattributed row set.
func deriveGmailSegments(m Memory) ([]gmailSegmentRow, *gmailSegmentDiagnostic) {
	if m.Provider == "imessage" || m.Type == "imessage" {
		return deriveIMessageSegments(m)
	}
	if !isGmailMemory(m) {
		return nil, nil
	}
	_, hasMessagesMeta := m.Meta["messages"]
	messages := gmailCommitmentMessages(m) // reuses commitment.go's meta.messages parser
	if len(messages) == 0 && !hasMessagesMeta {
		return nil, nil // legacy pre-#243 shape: key absent, no claim to fail closed on
	}

	// One canonical positional source for every alignment decision below. Never
	// count or derive text from gmailBodyParts: it strips and TrimSpace's the
	// first envelope before splitting, so an empty first message can erase a
	// real boundary and make later blocks shift while the apparent count still
	// matches meta.messages.
	rawBlocks := strings.Split(m.Text, gmailBodySeparator)
	bodyCount := len(rawBlocks)

	// DQ2 priority 1 — truncated, checked FIRST and independently of counts:
	// untrustworthy CONTENT can coincidentally still produce a matching count.
	if m.Truncated {
		return nil, &gmailSegmentDiagnostic{MemoryID: m.ID, Reason: gmailSegDiagTruncated, MetaCount: len(messages), BodyCount: bodyCount}
	}
	// DQ2 priority 2 — count mismatch (also covers a literal "---" line
	// inside one message's own body: the canonical raw split sees an extra
	// block).
	if len(messages) != bodyCount {
		return nil, &gmailSegmentDiagnostic{MemoryID: m.ID, Reason: gmailSegDiagCountMismatch, MetaCount: len(messages), BodyCount: bodyCount}
	}

	// DQ2 priority 3 — ordering mismatch: meta.messages[i].Sender must match
	// the sender address parsed from the RAW rendered block's OWN "From:"
	// header at that SAME position — a direct, positional, content-grounded
	// check, never a timestamp proxy (round 2 rejected that signal; see the
	// contract's DQ2 doc comment for why).
	for i, message := range messages {
		if i >= len(rawBlocks) {
			break // defensive; bodyCount==len(messages) is already guaranteed above
		}
		headerSender, ok := gmailSegBlockSender(rawBlocks[i])
		if !ok || !strings.EqualFold(strings.TrimSpace(message.Sender), headerSender) {
			return nil, &gmailSegmentDiagnostic{MemoryID: m.ID, Reason: gmailSegDiagOrderingMismatch, MetaCount: len(messages), BodyCount: bodyCount}
		}
	}

	// DQ3 — well-formed MessageRef: exact prefix m.ID+"#" with a non-empty
	// suffix. m.ID already equals "gmail_thread/"+threadID for every Gmail
	// memory, so this prefix check also enforces DQ3's thread-portion pin —
	// a ref naming a DIFFERENT thread can never share this prefix. A single
	// malformed ref refuses the WHOLE memory (never a partial derivation).
	prefix := m.ID + "#"
	for _, message := range messages {
		if !strings.HasPrefix(message.MessageRef, prefix) || message.MessageRef == prefix {
			return nil, &gmailSegmentDiagnostic{MemoryID: m.ID, Reason: gmailSegDiagMalformedRef, MetaCount: len(messages), BodyCount: bodyCount}
		}
	}

	// P0 fix (reviewer-reproduced) — duplicate MessageRef: checked LAST,
	// after every ref is already confirmed individually well-formed above.
	// Two messages declaring the SAME MessageRef would otherwise collide on
	// gmail_segments' evidence_ref PRIMARY KEY when writeGmailSegments
	// inserts them — a real SQL UNIQUE-constraint error that MUST be caught
	// here, before any row for this memory is ever inserted, so it fails
	// closed for ONLY this memory (zero segments + duplicate_ref
	// diagnostic) instead of aborting the whole rebuild/upsert transaction
	// and taking every OTHER memory's segments (and its own indexing) down
	// with it.
	seenRefs := make(map[string]bool, len(messages))
	for _, message := range messages {
		if seenRefs[message.MessageRef] {
			return nil, &gmailSegmentDiagnostic{MemoryID: m.ID, Reason: gmailSegDiagDuplicateRef, MetaCount: len(messages), BodyCount: bodyCount}
		}
		seenRefs[message.MessageRef] = true
	}

	// Well-formed: derive text from the SAME raw block whose count and sender
	// were validated above. stripFromLine is safe only after alignment is fixed
	// per block; applying it to the whole joined body before splitting is what
	// created the empty-first-message shift this invariant prevents.
	rows := make([]gmailSegmentRow, 0, len(messages))
	for i, message := range messages {
		text := strings.TrimSpace(stripFromLine(rawBlocks[i]))
		rows = append(rows, gmailSegmentRow{
			EvidenceRef: message.MessageRef,
			MemoryID:    m.ID,
			Sender:      message.Sender,
			Recipients:  gmailSegMergeRecipients(message.To, message.Cc),
			At:          message.At,
			BlockRefs:   append([]string(nil), message.BlockRefs...),
			Text:        text,
		})
	}
	return rows, nil
}

type imessageEvidenceMeta struct {
	EvidenceRef string `json:"evidence_ref"`
	At          string `json:"at"`
	FromMe      *bool  `json:"from_me"`
	Sender      string `json:"sender"`
	BlockStart  int    `json:"block_start"`
	BlockEnd    int    `json:"block_end"`
}

// deriveIMessageSegments rebuilds message-grain evidence exclusively from the
// vault's metadata plus exact rendered-body byte boundaries. Legacy memories
// remain parent-searchable and get an explicit coverage diagnostic.
func deriveIMessageSegments(m Memory) ([]gmailSegmentRow, *gmailSegmentDiagnostic) {
	raw, ok := m.Meta["message_evidence"]
	if !ok {
		return nil, &gmailSegmentDiagnostic{MemoryID: m.ID, Reason: "message_evidence_unavailable"}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, &gmailSegmentDiagnostic{MemoryID: m.ID, Reason: "message_evidence_malformed"}
	}
	var entries []imessageEvidenceMeta
	if json.Unmarshal(b, &entries) != nil || len(entries) == 0 {
		return nil, &gmailSegmentDiagnostic{MemoryID: m.ID, Reason: "message_evidence_malformed"}
	}
	prefix := m.ID + "#"
	seen := map[string]bool{}
	lastEnd := 0
	rows := make([]gmailSegmentRow, 0, len(entries))
	for _, e := range entries {
		if !strings.HasPrefix(e.EvidenceRef, prefix) || e.EvidenceRef == prefix || seen[e.EvidenceRef] ||
			e.FromMe == nil || strings.TrimSpace(e.Sender) == "" || !validIMessageEvidenceTime(e.At) ||
			e.BlockStart < lastEnd || e.BlockStart < 0 || e.BlockEnd <= e.BlockStart || e.BlockEnd > len(m.Text) {
			return nil, &gmailSegmentDiagnostic{MemoryID: m.ID, Reason: "message_evidence_malformed", MetaCount: len(entries)}
		}
		seen[e.EvidenceRef] = true
		lastEnd = e.BlockEnd
		rows = append(rows, gmailSegmentRow{
			EvidenceRef: e.EvidenceRef, MemoryID: m.ID, Sender: e.Sender, At: e.At,
			BlockRefs: []string{fmt.Sprintf("bytes:%d-%d", e.BlockStart, e.BlockEnd), fmt.Sprintf("from_me:%t", *e.FromMe)},
			Text:      m.Text[e.BlockStart:e.BlockEnd],
		})
	}
	return rows, nil
}

func validIMessageEvidenceTime(value string) bool {
	_, err := time.Parse(time.RFC3339, value)
	return err == nil
}

// imessageDirection recovers the explicit message direction carried by an
// iMessage segment. Gmail block refs never use this marker, so their existing
// search/read receipts stay byte-identical through omitempty.
func imessageDirection(blockRefs []string) string {
	for _, ref := range blockRefs {
		switch ref {
		case "from_me:true":
			return "outgoing"
		case "from_me:false":
			return "incoming"
		}
	}
	return ""
}

// gmailSegBlockSender parses the sender address off ONE raw block's own
// leading "From: <header>" line (net/mail address parsing, mirroring
// gmail.go's own header handling via addrSet.addHeader). Returns ok=false
// when the block carries no such line at all.
func gmailSegBlockSender(rawBlock string) (string, bool) {
	if !strings.HasPrefix(rawBlock, "From: ") {
		return "", false
	}
	line := rawBlock
	if i := strings.IndexByte(rawBlock, '\n'); i >= 0 {
		line = rawBlock[:i]
	}
	header := strings.TrimSpace(strings.TrimPrefix(line, "From:"))
	if addr, err := mail.ParseAddress(header); err == nil {
		return strings.ToLower(strings.TrimSpace(addr.Address)), true
	}
	// Defensive fallback for a header net/mail cannot parse at all — a
	// best-effort lowercase compare rather than dropping the signal.
	header = strings.ToLower(header)
	return header, header != ""
}

// gmailSegMergeRecipients implements DQ-recipients (§2): the deterministic,
// sorted, case-insensitive-deduped, lowercased UNION of To and Cc — never a
// plain concatenation and never independently-sorted-then-appended.
func gmailSegMergeRecipients(to, cc []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{to, cc} {
		for _, addr := range list {
			norm := strings.ToLower(strings.TrimSpace(addr))
			if norm == "" || seen[norm] {
				continue
			}
			seen[norm] = true
			out = append(out, norm)
		}
	}
	sort.Strings(out)
	return out
}

// gmailSegSchemaStmts are the CREATE statements for the three derived
// tables — DQ1/DQ2 (§2)'s frozen shapes. Appended into rebuildIndexWithPolicy's
// same stmts slice as every other table, so they're created inside the SAME
// transaction as memories/memories_fts (frozen interface #1: disposable,
// same-transaction projection).
var gmailSegSchemaStmts = []string{
	`CREATE TABLE IF NOT EXISTS gmail_segments (
		evidence_ref TEXT PRIMARY KEY,
		memory_id TEXT,
		sender TEXT,
		recipients TEXT,
		at TEXT,
		block_refs TEXT,
		text TEXT
	)`,
	`CREATE VIRTUAL TABLE IF NOT EXISTS gmail_segments_fts USING fts5(evidence_ref UNINDEXED, text)`,
	`CREATE TABLE IF NOT EXISTS gmail_segment_diagnostics (
		memory_id TEXT PRIMARY KEY,
		reason TEXT,
		meta_count INTEGER,
		body_count INTEGER
	)`,
}

// gmailSegDeleteStmts clear the three tables — paired with gmailSegSchemaStmts
// in rebuildIndexWithPolicy's blanket DELETE-then-reinsert, exactly like
// memories/memories_fts.
var gmailSegDeleteStmts = []string{
	`DELETE FROM gmail_segments`,
	`DELETE FROM gmail_segments_fts`,
	`DELETE FROM gmail_segment_diagnostics`,
}

// gmailSegStmts bundles the three prepared statements writeGmailSegments
// needs, prepared once per rebuild/upsert and reused across every memory.
type gmailSegStmts struct {
	seg  *sql.Stmt
	fts  *sql.Stmt
	diag *sql.Stmt
}

// prepareGmailSegStmts prepares the three INSERT statements on tx.
func prepareGmailSegStmts(ctx context.Context, tx *sql.Tx) (*gmailSegStmts, error) {
	seg, err := tx.PrepareContext(ctx, `INSERT INTO gmail_segments VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	fts, err := tx.PrepareContext(ctx, `INSERT INTO gmail_segments_fts (evidence_ref, text) VALUES (?, ?)`)
	if err != nil {
		return nil, err
	}
	diag, err := tx.PrepareContext(ctx, `INSERT INTO gmail_segment_diagnostics VALUES (?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	return &gmailSegStmts{seg: seg, fts: fts, diag: diag}, nil
}

func (s *gmailSegStmts) Close() {
	if s == nil {
		return
	}
	_ = s.seg.Close()
	_ = s.fts.Close()
	_ = s.diag.Close()
}

// writeGmailSegments derives and persists m's evidence-segment projection
// (or its fail-closed diagnostic) on tx. Called once per LIVE (non-
// tombstoned) memory from both the full rebuild (index.go) and the
// incremental upsert (index_upsert.go), so the two paths stay in parity —
// the frozen contract carries no explicit pin for the upsert path, so this
// mirrors the rebuild's own fail-closed rules exactly rather than leaving it
// undefined.
func writeGmailSegments(ctx context.Context, stmts *gmailSegStmts, m Memory) error {
	rows, diag := deriveGmailSegments(m)
	for _, r := range rows {
		recipientsJSON, err := json.Marshal(r.Recipients)
		if err != nil {
			return err
		}
		blockRefsJSON, err := json.Marshal(r.BlockRefs)
		if err != nil {
			return err
		}
		if _, err := stmts.seg.ExecContext(ctx, r.EvidenceRef, r.MemoryID, r.Sender, string(recipientsJSON), r.At, string(blockRefsJSON), r.Text); err != nil {
			return err
		}
		if _, err := stmts.fts.ExecContext(ctx, r.EvidenceRef, r.Text); err != nil {
			return err
		}
	}
	if diag != nil {
		if _, err := stmts.diag.ExecContext(ctx, diag.MemoryID, diag.Reason, diag.MetaCount, diag.BodyCount); err != nil {
			return err
		}
	}
	return nil
}

// clearGmailSegmentsFor deletes every gmail_segments/_fts/diagnostics row for
// one memory id — the incremental-upsert counterpart of the full rebuild's
// blanket DELETE, run before writeGmailSegments re-derives (or the memory is
// a tombstone and gets none). Small per-memory row counts make a two-step
// select-then-delete simpler and safer than a subquery against the FTS5
// virtual table.
func clearGmailSegmentsFor(ctx context.Context, tx *sql.Tx, memoryID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT evidence_ref FROM gmail_segments WHERE memory_id=?`, memoryID)
	if err != nil {
		return err
	}
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, ref := range refs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM gmail_segments_fts WHERE evidence_ref=?`, ref); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gmail_segments WHERE memory_id=?`, memoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM gmail_segment_diagnostics WHERE memory_id=?`, memoryID); err != nil {
		return err
	}
	return nil
}

// gmailSegmentByRef looks up ONE derived segment by (memoryID, evidenceRef).
// A zero-row result covers all three read_memory fail-closed cases uniformly
// (§1.4 / DQ6): an unknown ref, a ref belonging to another memory, and a ref
// against a memory whose own segments failed closed at rebuild time (which
// leaves zero rows under that memory_id for ANY evidence_ref) — the caller
// turns ok=false into one explicit rejection, never a silent fallback.
func gmailSegmentByRef(ctx context.Context, cfg Config, memoryID, evidenceRef string) (gmailSegmentRow, bool, error) {
	db, err := openIndexRO(ctx, cfg)
	if err != nil {
		return gmailSegmentRow{}, false, err
	}
	defer db.Close()
	var r gmailSegmentRow
	var recipientsJSON, blockRefsJSON string
	err = db.QueryRowContext(ctx,
		`SELECT evidence_ref, memory_id, sender, recipients, at, block_refs, text
		 FROM gmail_segments WHERE memory_id = ? AND evidence_ref = ?`, memoryID, evidenceRef,
	).Scan(&r.EvidenceRef, &r.MemoryID, &r.Sender, &recipientsJSON, &r.At, &blockRefsJSON, &r.Text)
	if err == sql.ErrNoRows {
		return gmailSegmentRow{}, false, nil
	}
	if err != nil {
		return gmailSegmentRow{}, false, err
	}
	_ = json.Unmarshal([]byte(recipientsJSON), &r.Recipients)
	_ = json.Unmarshal([]byte(blockRefsJSON), &r.BlockRefs)
	return r, true, nil
}
