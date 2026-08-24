// Package segments owns the disposable message-grain evidence projection.
package segments

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/urgency"
	"net/mail"
	"sort"
	"strings"
	"time"
)

const BodySeparator = "\n\n---\n\n"
const (
	DiagTruncated        = "truncated"
	DiagCountMismatch    = "count_mismatch"
	DiagOrderingMismatch = "ordering_mismatch"
	DiagMalformedRef     = "malformed_ref"
	DiagDuplicateRef     = "duplicate_ref"
)

type Row struct {
	EvidenceRef, MemoryID, Sender string
	Recipients                    []string
	At                            string
	BlockRefs                     []string
	Text                          string
}
type Diagnostic struct {
	MemoryID, Reason     string
	MetaCount, BodyCount int
}
type message struct {
	MessageRef string   `json:"message_ref"`
	Sender     string   `json:"sender,omitempty"`
	To         []string `json:"to,omitempty"`
	Cc         []string `json:"cc,omitempty"`
	At         string   `json:"at,omitempty"`
	BlockRefs  []string `json:"block_refs,omitempty"`
}

func gmailMessages(m memory.Memory) []message {
	b, err := json.Marshal(m.Meta["messages"])
	if err != nil {
		return nil
	}
	var out []message
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}
func isGmail(m memory.Memory) bool {
	return strings.EqualFold(m.Provider, "gmail") || strings.Contains(strings.ToLower(m.ProviderID), "gmail")
}
func Derive(m memory.Memory) ([]Row, *Diagnostic) {
	if m.Provider == "imessage" || m.Type == "imessage" {
		return deriveIMessage(m)
	}
	if !isGmail(m) {
		return nil, nil
	}
	_, has := m.Meta["messages"]
	messages := gmailMessages(m)
	if len(messages) == 0 && !has {
		return nil, nil
	}
	blocks := strings.Split(m.Text, BodySeparator)
	bodyCount := len(blocks)
	diag := func(reason string) *Diagnostic {
		return &Diagnostic{MemoryID: m.ID, Reason: reason, MetaCount: len(messages), BodyCount: bodyCount}
	}
	if m.Truncated {
		return nil, diag(DiagTruncated)
	}
	if len(messages) != bodyCount {
		return nil, diag(DiagCountMismatch)
	}
	for i, msg := range messages {
		if i >= len(blocks) {
			break
		}
		sender, ok := BlockSender(blocks[i])
		if !ok || !strings.EqualFold(strings.TrimSpace(msg.Sender), sender) {
			return nil, diag(DiagOrderingMismatch)
		}
	}
	prefix := m.ID + "#"
	for _, msg := range messages {
		if !strings.HasPrefix(msg.MessageRef, prefix) || msg.MessageRef == prefix {
			return nil, diag(DiagMalformedRef)
		}
	}
	seen := map[string]bool{}
	for _, msg := range messages {
		if seen[msg.MessageRef] {
			return nil, diag(DiagDuplicateRef)
		}
		seen[msg.MessageRef] = true
	}
	rows := make([]Row, 0, len(messages))
	for i, msg := range messages {
		rows = append(rows, Row{EvidenceRef: msg.MessageRef, MemoryID: m.ID, Sender: msg.Sender, Recipients: MergeRecipients(msg.To, msg.Cc), At: msg.At, BlockRefs: append([]string(nil), msg.BlockRefs...), Text: strings.TrimSpace(urgency.StripFromLine(blocks[i]))})
	}
	return rows, nil
}

type imessageEvidence struct {
	EvidenceRef string `json:"evidence_ref"`
	At          string `json:"at"`
	FromMe      *bool  `json:"from_me"`
	Sender      string `json:"sender"`
	BlockStart  int    `json:"block_start"`
	BlockEnd    int    `json:"block_end"`
}

func deriveIMessage(m memory.Memory) ([]Row, *Diagnostic) {
	raw, ok := m.Meta["message_evidence"]
	if !ok {
		return nil, &Diagnostic{MemoryID: m.ID, Reason: "message_evidence_unavailable"}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, &Diagnostic{MemoryID: m.ID, Reason: "message_evidence_malformed"}
	}
	var entries []imessageEvidence
	if json.Unmarshal(b, &entries) != nil || len(entries) == 0 {
		return nil, &Diagnostic{MemoryID: m.ID, Reason: "message_evidence_malformed"}
	}
	prefix := m.ID + "#"
	seen := map[string]bool{}
	lastEnd := 0
	rows := make([]Row, 0, len(entries))
	for _, e := range entries {
		if !strings.HasPrefix(e.EvidenceRef, prefix) || e.EvidenceRef == prefix || seen[e.EvidenceRef] || e.FromMe == nil || strings.TrimSpace(e.Sender) == "" || !validTime(e.At) || e.BlockStart < lastEnd || e.BlockStart < 0 || e.BlockEnd <= e.BlockStart || e.BlockEnd > len(m.Text) {
			return nil, &Diagnostic{MemoryID: m.ID, Reason: "message_evidence_malformed", MetaCount: len(entries)}
		}
		seen[e.EvidenceRef] = true
		lastEnd = e.BlockEnd
		audience := "direct"
		if group, _ := m.Meta["is_group"].(bool); group {
			audience = "group"
		}
		rows = append(rows, Row{EvidenceRef: e.EvidenceRef, MemoryID: m.ID, Sender: e.Sender, At: e.At, BlockRefs: []string{fmt.Sprintf("bytes:%d-%d", e.BlockStart, e.BlockEnd), fmt.Sprintf("from_me:%t", *e.FromMe), "audience:" + audience}, Text: m.Text[e.BlockStart:e.BlockEnd]})
	}
	return rows, nil
}
func validTime(v string) bool { _, err := time.Parse(time.RFC3339, v); return err == nil }
func Direction(blockRefs []string) string {
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

func Audience(blockRefs []string) string {
	for _, ref := range blockRefs {
		if ref == "audience:group" {
			return "group"
		}
		if ref == "audience:direct" {
			return "direct"
		}
	}
	return ""
}
func BlockSender(raw string) (string, bool) {
	if !strings.HasPrefix(raw, "From: ") {
		return "", false
	}
	line := raw
	if i := strings.IndexByte(raw, '\n'); i >= 0 {
		line = raw[:i]
	}
	header := strings.TrimSpace(strings.TrimPrefix(line, "From:"))
	if addr, err := mail.ParseAddress(header); err == nil {
		return strings.ToLower(strings.TrimSpace(addr.Address)), true
	}
	header = strings.ToLower(header)
	return header, header != ""
}
func MergeRecipients(to, cc []string) []string {
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

var SchemaStatements = []string{
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
var DeleteStatements = []string{
	`DELETE FROM gmail_segments`,
	`DELETE FROM gmail_segments_fts`,
	`DELETE FROM gmail_segment_diagnostics`,
}

type Statements struct{ seg, fts, diag *sql.Stmt }

func Prepare(ctx context.Context, tx *sql.Tx) (*Statements, error) {
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
	return &Statements{seg: seg, fts: fts, diag: diag}, nil
}
func (s *Statements) Close() {
	if s == nil {
		return
	}
	_ = s.seg.Close()
	_ = s.fts.Close()
	_ = s.diag.Close()
}
func (s *Statements) Write(ctx context.Context, m memory.Memory) error {
	rows, diag := Derive(m)
	for _, r := range rows {
		recipients, err := json.Marshal(r.Recipients)
		if err != nil {
			return err
		}
		refs, err := json.Marshal(r.BlockRefs)
		if err != nil {
			return err
		}
		if _, err = s.seg.ExecContext(ctx, r.EvidenceRef, r.MemoryID, r.Sender, string(recipients), r.At, string(refs), r.Text); err != nil {
			return err
		}
		if _, err = s.fts.ExecContext(ctx, r.EvidenceRef, r.Text); err != nil {
			return err
		}
	}
	if diag != nil {
		if _, err := s.diag.ExecContext(ctx, diag.MemoryID, diag.Reason, diag.MetaCount, diag.BodyCount); err != nil {
			return err
		}
	}
	return nil
}
func Clear(ctx context.Context, tx *sql.Tx, memoryID string) error {
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
func Lookup(ctx context.Context, db *sql.DB, memoryID, evidenceRef string) (Row, bool, error) {
	var r Row
	var recipients, refs string
	err := db.QueryRowContext(ctx, `SELECT evidence_ref, memory_id, sender, recipients, at, block_refs, text FROM gmail_segments WHERE memory_id = ? AND evidence_ref = ?`, memoryID, evidenceRef).Scan(&r.EvidenceRef, &r.MemoryID, &r.Sender, &recipients, &r.At, &refs, &r.Text)
	if err == sql.ErrNoRows {
		return Row{}, false, nil
	}
	if err != nil {
		return Row{}, false, err
	}
	_ = json.Unmarshal([]byte(recipients), &r.Recipients)
	_ = json.Unmarshal([]byte(refs), &r.BlockRefs)
	return r, true, nil
}
