package mora

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	commitmentpkg "github.com/pyranthus-hq/mora/internal/commitment"
	"github.com/pyranthus-hq/mora/internal/commitmentclassify"
	meetingpkg "github.com/pyranthus-hq/mora/internal/meeting"
	"sort"
	"strings"
	"time"
)

// Direction is the shared obligation-direction vocabulary used by every
// product lane. A named type prevents task-ledger and evidence-derived loops
// from drifting into independently invented string values.
type Direction = commitmentpkg.Direction

const (
	commitDirectionUnknown   = commitmentpkg.DirectionUnknown
	commitOwedBySelf         = commitmentpkg.OwedBySelf
	commitOwedByCounterparty = commitmentpkg.OwedByCounterparty

	commitOpen       = commitmentpkg.Open
	commitClosed     = commitmentpkg.Closed
	commitSuperseded = commitmentpkg.Superseded

	commitDueNone         = commitmentpkg.DueNone
	commitDueRelative     = commitmentpkg.DueRelative
	commitDueExplicitDate = commitmentpkg.DueExplicitDate

	commitClosureNone = commitmentpkg.ClosureNone

	commitCitationOpener     = commitmentpkg.CitationOpener
	commitCitationClosure    = commitmentpkg.CitationClosure
	commitCitationSupporting = commitmentpkg.CitationSupporting
)

type Commitment = commitmentpkg.Record
type commitSpan = commitmentpkg.Span

type commitDue = commitmentpkg.Due

func commitDueValue(due commitDue) string { return commitmentpkg.DueValue(due) }

type commitmentMessageEvidence = commitmentpkg.GmailMessage
type CommitmentCitation = commitmentpkg.Citation

type commitmentPartyRole string

const (
	commitmentPartyUnknown      commitmentPartyRole = ""
	commitmentPartySelf         commitmentPartyRole = "self"
	commitmentPartyCounterparty commitmentPartyRole = "counterparty"
)

type commitmentEvidence struct {
	MemoryID         string
	MessageRef       string
	BlockRef         string
	Text             string
	OccurredAt       string
	Party            commitmentPartyRole
	Authored         bool
	Citation         BriefCitation
	Source           string
	CounterpartyKeys []string
}

type commitmentSnapshot struct {
	Generation  string
	Commitments []Commitment
}

// commitmentID is versioned and length-prefixed exactly like the scorer's
// evidence identity. Person identity is deliberately absent: graph alias merges
// can regroup a commitment without churning its durable anchor.
func commitmentID(messageRef, blockRef string, slot int) string {
	return commitmentpkg.ID(messageRef, blockRef, slot)
}

func atomEqual(a, b govAtom) bool { return commitmentpkg.EqualAtom(a, b) }

func atomPresent(a govAtom) bool {
	return strings.TrimSpace(a.Kind) != "" && strings.TrimSpace(a.Value) != ""
}

// reportedActorFor resolves attributed third-person speech only from stable source
// identities. A participant other than the current thread counterparty may own an
// obligation only when the report also names the user as its beneficiary; otherwise
// the work is between third parties and must be dropped.

func canonicalSelfAtom(cfg Config, preferred string) govAtom {
	return commitmentpkg.CanonicalSelf(selfEmails(cfg), preferred)
}

func commitmentCounterparty(m Memory, cfg Config) (govAtom, bool) {
	return commitmentpkg.Counterparty(m, selfEmails(cfg))
}

// participantNameIsSelf handles imported/transcoded iMessage records that list the
// user alongside the other chat participants. The live connector already stores
// only other-party handles. We exclude a listed participant only when every
// meaningful display-name token is independently present in a configured self
// mailbox local-part; a partial/common-name overlap is insufficient.

// imessageCommitmentMessages returns message-grain lifecycle evidence only when
// the whole metadata set passes the same fail-closed validation as the search
// projection. The second return value says that message_evidence was present.
// Present but malformed metadata must not fall back to transcript guesses.

// trustedIMessageAuthoredBody removes exactly one rendered sender prefix. The
// explicit direction and sender metadata must agree with the visible block.

// gmailAuthoredBlockRef binds a sender-authored prefix to the first ordered block
// ref. The Gmail renderer preserves block order. When later footer, quoted, or
// forwarded blocks exist, senderAuthoredBody removes them before classification;
// therefore the first ref remains the only evidence-derived ref we can assign.
// A message with no authored prefix stays ID-less.

// gmailFulfilledQuotedRequest recognizes the contract's one exceptional quoted
// opening: an authored delivery followed by the earlier request it fulfills. The
// two ordered block refs are required so the opening and closure remain grounded;
// a quote without authored fulfillment returns no obligation.

// acceptanceRestatesRequest implements the contract rule that accepting an
// existing request does not create extra work. It is intentionally narrower than
// general dedup: same artifact, later message, same typed parties/direction/due,
// a direct-request opener, and either strong object overlap or an explicit
// anaphoric acceptance with corroborating object overlap.

func classifyCommitments(m Memory, cfg Config) []Commitment {
	return commitmentclassify.Classify(m, commitmentclassify.Options{SelfEmails: selfEmails(cfg), ServiceOnly: memoryIsServiceOnly(m)})
}

func commitmentEvidenceFromMemories(mems []Memory, cfg Config) []commitmentEvidence {
	eligible := make([]Memory, 0, len(mems))
	for _, m := range mems {
		if m.DeletedAt == "" && !meetingpkg.IsMeetingNotification(m) && !memoryIsServiceOnly(m) {
			eligible = append(eligible, m)
		}
	}
	projected := commitmentpkg.EvidenceFromMemories(eligible, selfEmails(cfg))
	out := make([]commitmentEvidence, len(projected))
	for i, e := range projected {
		out[i] = commitmentEvidence{MemoryID: e.MemoryID, MessageRef: e.MessageRef, BlockRef: e.BlockRef, Text: e.Text, OccurredAt: e.OccurredAt, Party: commitmentPartyRole(e.Party), Authored: e.Authored, Citation: e.Citation, Source: e.Source, CounterpartyKeys: e.CounterpartyKeys}
	}
	return out
}

func commitmentEvidenceLess(a, b Commitment) bool { return commitmentpkg.EvidenceLess(a, b) }
func containsStringFold(values []string, want string) bool {
	return commitmentpkg.ContainsStringFold(values, want)
}
func commitmentLifecycleEvidence(e commitmentEvidence) commitmentpkg.Evidence {
	stableEvidenceRef := ""
	if strings.HasPrefix(e.MemoryID, "imessage_chat/") && strings.HasPrefix(e.MessageRef, e.MemoryID+"#") {
		stableEvidenceRef = e.MessageRef
	}
	return commitmentpkg.Evidence{MemoryID: e.MemoryID, MessageRef: e.MessageRef, Text: e.Text, OccurredAt: e.OccurredAt, Party: commitmentpkg.Party(e.Party), Authored: e.Authored, CounterpartyKeys: e.CounterpartyKeys, Citation: e.Citation, CitationEvidenceRef: stableEvidenceRef}
}
func applyCommitmentLifecycle(commitments []Commitment, evidence []commitmentEvidence) []Commitment {
	ev := make([]commitmentpkg.Evidence, len(evidence))
	for i, e := range evidence {
		ev[i] = commitmentLifecycleEvidence(e)
	}
	return commitmentpkg.ApplyLifecycle(commitments, ev)
}

func deduplicateCommitments(commitments []Commitment) []Commitment {
	return commitmentpkg.Deduplicate(commitments)
}

func materializeCommitments(mems []Memory, cfg Config, now time.Time) []Commitment {
	// Decisions with incomplete or expired validity remain queryable as
	// needs_review, but cannot open an obligation or supply lifecycle evidence
	// that closes one. Filter both paths together so a stale decision cannot act
	// as current law through either side of the lifecycle projection.
	authority := make([]Memory, 0, len(mems))
	for _, m := range mems {
		if memoryMayGovernCommitments(m, now) {
			authority = append(authority, m)
		}
	}
	var commitments []Commitment
	for _, m := range authority {
		commitments = append(commitments, classifyCommitments(m, cfg)...)
	}
	commitments = applyCommitmentLifecycle(commitments, commitmentEvidenceFromMemories(authority, cfg))
	commitments = deduplicateCommitments(commitments)
	if commitmentSnapshotUncertain(cfg, now) {
		for i := range commitments {
			commitments[i].StateUncertain = true
			if commitments[i].Gap == "" {
				commitments[i].Gap = "One or more required sources are stale or unavailable; lifecycle state is uncertain."
			}
		}
	}
	sort.SliceStable(commitments, func(i, j int) bool {
		if commitments[i].OpenedBy.MemoryID != commitments[j].OpenedBy.MemoryID {
			return commitments[i].OpenedBy.MemoryID < commitments[j].OpenedBy.MemoryID
		}
		return commitmentEvidenceLess(commitments[i], commitments[j])
	})
	return commitments
}

func commitmentGenerationOf(manifestLines []string, cfg Config, now time.Time) string {
	lines := append([]string(nil), manifestLines...)
	health, _ := json.Marshal(sourceHealthAll(cfg, now))
	lines = append(lines,
		"commitment_snapshot_at  "+now.UTC().Format(time.RFC3339Nano),
		"commitment_source_health  "+string(health),
	)
	return manifestDigestOf(lines)
}

func commitmentSnapshotUncertain(cfg Config, now time.Time) bool {
	for _, health := range sourceHealthAll(cfg, now) {
		if health.State != healthFresh {
			return true
		}
	}
	return false
}

func meetingCommitmentFor(candidates []Commitment, attendee govAtom, aliases []string, excerpt string) (Commitment, bool) {
	samePerson := func(atom govAtom) bool {
		if atomEqual(atom, attendee) {
			return true
		}
		for _, alias := range aliases {
			if strings.EqualFold(normalizeIdentity(atom.Kind, atom.Value), normalizeIdentity(atom.Kind, alias)) {
				return true
			}
		}
		return false
	}
	var matched []Commitment
	for _, commitment := range candidates {
		if !samePerson(commitment.Counterparty) {
			continue
		}
		if atomEqual(commitment.Owner, commitment.Counterparty) {
			commitment.Owner = attendee
		}
		commitment.Counterparty = attendee
		if excerpt != "" && strings.EqualFold(oneLine(commitment.Summary), oneLine(excerpt)) {
			return commitment, true
		}
		matched = append(matched, commitment)
	}
	if excerpt == "" && len(matched) == 1 {
		return matched[0], true
	}
	// Multiple independently anchored openings in one artifact are not collapsed
	// into a single line by guesswork. A later PR can render them as separate
	// commitments; this one refuses ambiguous attribution.
	return Commitment{}, false
}

func attachCommitment(line *CitedBriefLine, commitment Commitment) {
	line.Direction = commitment.Direction
	line.Owner = commitment.Owner
	line.Counterparty = commitment.Counterparty
	line.CounterpartyLabel = commitment.CounterpartyLabel
	line.CommitmentID = commitment.ID
	line.DueAt = commitDueValue(commitment.Due)
	line.Lifecycle = commitment.State
	line.ClosureRef = commitment.ClosureRef
	line.DuplicateOf = commitment.DuplicateOf
	line.StateUncertain = commitment.StateUncertain
	line.CommitmentCitations = append([]CommitmentCitation(nil), commitment.Citations...)
}

func writeCommitments(ctx context.Context, tx *sql.Tx, generation string, mems []Memory, cfg Config, now time.Time) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO commitments (generation, row_key, commitment_id, memory_id, payload)
		 VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	governance, err := loadGovernance(cfg)
	if err != nil {
		return err
	}
	commitments := applyTeachCommitments(materializeCommitments(mems, cfg, now), governance, cfg)
	for i, commitment := range commitments {
		payload, err := json.Marshal(commitment)
		if err != nil {
			return err
		}
		rowKey := commitment.ID
		if rowKey == "" {
			sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", commitment.OpenedBy.MemoryID, i, commitment.Direction, commitment.Summary)))
			rowKey = "legacy:" + hex.EncodeToString(sum[:])
		}
		if _, err := stmt.ExecContext(ctx, generation, rowKey, nullStr(commitment.ID), commitment.OpenedBy.MemoryID, string(payload)); err != nil {
			return err
		}
	}
	return nil
}

func readCommitmentSnapshot(ctx context.Context, cfg Config) (commitmentSnapshot, error) {
	db, err := ensureIndexDB(ctx, cfg)
	if err != nil {
		return commitmentSnapshot{}, err
	}
	defer db.Close()
	var generation string
	if err := db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key='commitments_generation'`).Scan(&generation); err != nil {
		return commitmentSnapshot{}, fmt.Errorf("read commitment generation: %w", err)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT memory_id, payload FROM commitments WHERE generation=? ORDER BY memory_id, row_key`, generation)
	if err != nil {
		return commitmentSnapshot{}, err
	}
	defer rows.Close()
	out := commitmentSnapshot{Generation: generation, Commitments: []Commitment{}}
	for rows.Next() {
		var memoryID, payload string
		if err := rows.Scan(&memoryID, &payload); err != nil {
			return commitmentSnapshot{}, err
		}
		var commitment Commitment
		if err := json.Unmarshal([]byte(payload), &commitment); err != nil {
			return commitmentSnapshot{}, fmt.Errorf("decode commitment %s: %w", memoryID, err)
		}
		out.Commitments = append(out.Commitments, commitment)
	}
	return out, rows.Err()
}

func readCommitmentInventory(ctx context.Context, cfg Config, at time.Time) (map[string][]Commitment, error) {
	inventory, _, err := readCommitmentInventoryWithMemories(ctx, cfg, at)
	return inventory, err
}

// readCommitmentInventoryWithMemories returns the current typed inventory and
// the opening memories it validated in the same vault pass. Callers that need
// both must use this helper instead of resolving each memory id separately,
// which would rescan the whole vault once per commitment-bearing memory.
func readCommitmentInventoryWithMemories(ctx context.Context, cfg Config, at time.Time) (map[string][]Commitment, map[string]Memory, error) {
	snapshot, err := readCommitmentSnapshot(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	governance, err := loadGovernance(cfg)
	if err != nil {
		return nil, nil, err
	}
	wanted := make(map[string]bool, len(snapshot.Commitments))
	for _, commitment := range snapshot.Commitments {
		wanted[commitment.OpenedBy.MemoryID] = true
	}
	memories := make(map[string]Memory, len(wanted))
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return nil, nil, err
	}
	for _, path := range files {
		m, perr := parseMemory(path)
		if perr == nil && wanted[m.ID] {
			memories[m.ID] = m
		}
	}
	out := map[string][]Commitment{}
	for _, commitment := range snapshot.Commitments {
		// The vault is truth and the index is derived. Refuse a stale row when its
		// evidence is no longer current or no longer readable. Re-evaluating
		// decision validity at the caller's surface clock also prevents a
		// commitment built before review_by from remaining authoritative after it
		// expires without another rebuild.
		m, ok := memories[commitment.OpenedBy.MemoryID]
		if !ok || !governance.memoryVisible(m.ID) ||
			!memoryMayGovernCommitments(m, at) {
			continue
		}
		out[commitment.OpenedBy.MemoryID] = append(out[commitment.OpenedBy.MemoryID], commitment)
	}
	return out, memories, nil
}
