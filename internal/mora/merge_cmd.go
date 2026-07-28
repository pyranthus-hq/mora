package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

// cmdMerge implements `mora merge` — the P13 one-tap confirm-queue for cross-channel
// (email<->phone) identity unification. Mora auto-merges only within a channel where
// the same-human claim is byte-provable (RULE 1/2). Across channels there is no such
// proof, so instead of guessing, it surfaces corroborated candidates here for a human
// to confirm or reject in one tap:
//
//	mora merge list                          show pending email<->phone candidates
//	mora merge confirm --handle H --email A   unify a phone handle with an email
//	mora merge reject  --handle H --email A   pin them apart (never re-proposed)
//	mora merge undo <ledger-id>               reverse a prior confirm/reject
//	  [--json]  (list) machine-readable candidates
//
// Every decision is recorded in the governance ledger keyed on the SOURCE-NATIVE
// atoms ({imessage,handle,+1…} and {,address,a@b.com}), never the post-merge person
// id — so it survives the next connector re-sync (the #52 trap).
func cmdMerge(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora merge <list|confirm|reject|undo> (see `mora merge list`)")
	}
	switch args[0] {
	case "list":
		return mergeList(ctx, args[1:], stdout)
	case "confirm":
		return mergeDecide(ctx, args[1:], stdout, mergeDecisionConfirm)
	case "reject":
		return mergeDecide(ctx, args[1:], stdout, mergeDecisionReject)
	case "undo":
		return mergeUndo(ctx, args[1:], stdout)
	default:
		return fmt.Errorf("unknown merge subcommand %q (want list|confirm|reject|undo)", args[0])
	}
}

// pendingMerge is one queue row for JSON/CLI rendering.
type pendingMerge struct {
	Name     string          `json:"name"`
	Handle   string          `json:"handle"`
	Email    string          `json:"email"`
	Echoed   []string        `json:"echoed_name_tokens"`
	Evidence []mergeEvidence `json:"corroborating_evidence"`
	Affected []string        `json:"affected_items"`
}

type mergeEvidence struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// mergeList prints the pending email<->phone candidates: proposed by
// emailPhoneCandidates (address-book name + address signature) minus every pair the
// user already decided (confirmed pairs are already merged; rejected pairs stay
// apart). Read-only.
func mergeList(ctx context.Context, args []string, stdout io.Writer) error {
	jsonOut := false
	for _, a := range args {
		switch a {
		case "--json", "-json":
			jsonOut = true
		default:
			return fmt.Errorf("unexpected argument %q to `mora merge list`", a)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	mems, err := loadGraphMemories(cfg)
	if err != nil {
		return err
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		return err
	}
	confirmed, decided := g.mergeDecisions()
	res := buildGraphResult(mems, confirmed)

	pending := make([]pendingMerge, 0, len(res.candidates))
	for _, c := range res.candidates {
		if decided[mergePairKey(c.PhoneID, c.EmailID)] {
			continue // already confirmed (merged) or rejected (pinned apart)
		}
		pending = append(pending, pendingMergeOf(c, mems))
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].Name != pending[j].Name {
			return pending[i].Name < pending[j].Name
		}
		if pending[i].Handle != pending[j].Handle {
			return pending[i].Handle < pending[j].Handle
		}
		return pending[i].Email < pending[j].Email
	})

	if jsonOut {
		return emit(stdout, pending, true)
	}
	if len(pending) == 0 {
		fmt.Fprintln(stdout, "no pending email<->phone merges")
		return nil
	}
	fmt.Fprintln(stdout, "Pending merges (email <-> phone) — a phone contact and an email that look like the same person:")
	for _, p := range pending {
		fmt.Fprintf(stdout, "  %s\n", p.Name)
		fmt.Fprintf(stdout, "    phone  %s  (imessage)\n", p.Handle)
		fmt.Fprintf(stdout, "    email  %s\n", p.Email)
		for _, evidence := range p.Evidence {
			fmt.Fprintf(stdout, "    evidence  %s: %s\n", evidence.Kind, evidence.Detail)
		}
		fmt.Fprintf(stdout, "    affected  %s\n", mergeAffectedLabel(p.Affected))
		fmt.Fprintf(stdout, "    confirm: mora merge confirm --handle %s --email %s --yes\n", p.Handle, p.Email)
	}
	fmt.Fprintln(stdout, "\n(Mora never merges these automatically — a wrong merge is worse than a gap. Reject with `mora merge reject`.)")
	return nil
}

// mergeDecide records a confirm/reject decision for an email<->phone pair and rebuilds
// the graph so a confirm takes effect immediately. Non-destructive and reversible
// (`mora merge undo <id>`): a confirm only unifies; nothing is deleted.
func mergeDecide(ctx context.Context, args []string, stdout io.Writer, decision string) error {
	fs := flag.NewFlagSet("merge "+decision, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	handle := fs.String("handle", "", "iMessage phone handle (e.g. +14155550123)")
	email := fs.String("email", "", "email address (e.g. person@example.com)")
	yes := fs.Bool("yes", false, "confirm after reviewing evidence and affected items")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *handle == "" || *email == "" {
		return fmt.Errorf("merge %s requires --handle <phone> and --email <address>", decision)
	}
	phoneAtom := govAtom{Provider: "imessage", Kind: atomHandle, Value: normalizeIdentity(atomHandle, *handle)}
	// Provider "" (cross-provider wildcard): an email address is the same identity
	// whether it arrived over gmail or calendar — mirrors `mora forget --email`.
	emailAtom := govAtom{Provider: "", Kind: atomAddress, Value: normalizeIdentity(atomAddress, *email)}
	if phoneAtom.Value == "" || emailAtom.Value == "" {
		return errors.New("both --handle and --email must be non-empty identities")
	}
	if atomPersonID(phoneAtom) == atomPersonID(emailAtom) {
		return errors.New("--handle and --email resolve to the same identity; nothing to merge")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if decision == mergeDecisionConfirm {
		// A confirm is the high-impact path. Always show the exact proposal
		// evidence and affected memories before any ledger mutation, even when the
		// caller supplied --yes directly.
		mems, lerr := loadGraphMemories(cfg)
		if lerr != nil {
			return lerr
		}
		pending, perr := pendingMergeForPair(cfg, mems, phoneAtom, emailAtom)
		if perr != nil {
			return perr
		}
		fmt.Fprintf(stdout, "Review before confirming %s <-> %s:\n", pending.Handle, pending.Email)
		for _, evidence := range pending.Evidence {
			fmt.Fprintf(stdout, "  %s: %s\n", evidence.Kind, evidence.Detail)
		}
		fmt.Fprintf(stdout, "  affected items: %s\n", mergeAffectedLabel(pending.Affected))
		if !*yes {
			return errors.New("review required; rerun the same command with --yes")
		}
	}
	// A5 row 8: mark the index dirty BEFORE appending to the governance ledger (an
	// index input via writeGraph), not after. The append at :151 landed before the
	// rebuild's own A4 mark, so a crash in that window left the ledger changed while
	// the index read fresh — a false-clean. Marking first closes it: the pending
	// rebuild op survives the crash and the committed rebuild below retires it
	// (A3 rule a).
	op, merr := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindRebuild})
	if merr != nil {
		return merr
	}
	entry, err := appendGovernanceEntry(cfg, govEntry{
		Kind:     govKindMergeConfirm,
		Action:   govActionRecord,
		Atom:     phoneAtom,
		Atom2:    &emailAtom,
		Decision: decision,
		Reason:   fmt.Sprintf("mora merge %s --handle %s --email %s", decision, *handle, *email),
	})
	if err != nil {
		_ = unmarkIndexDirty(cfg, op.OpID) // the ledger never changed
		return err
	}
	if _, err := rebuildIndexWithPolicy(ctx, cfg, policyAllow); err != nil {
		return err
	}
	if decision == mergeDecisionConfirm {
		fmt.Fprintf(stdout, "confirmed: %s and %s are the same person (entry %s)\n", *handle, *email, entry.ID)
	} else {
		fmt.Fprintf(stdout, "rejected: %s and %s kept as separate people (entry %s)\n", *handle, *email, entry.ID)
	}
	fmt.Fprintln(stdout, "reverse with `mora merge undo "+entry.ID+"`")
	return nil
}

// mergeUndo revokes a prior merge_confirm entry and rebuilds so the graph reverts.
func mergeUndo(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("merge undo requires one governance entry id (see `mora merge list`)")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// A5 row 8: mark before mutating the ledger (see mergeDecide) — a crash between
	// the revoke and the rebuild must leave the index dirty, never fresh-with-a-
	// changed-ledger. The committed rebuild retires the op (A3 rule a).
	op, merr := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindRebuild})
	if merr != nil {
		return merr
	}
	found, err := revokeGovernanceEntry(cfg, args[0])
	if err != nil {
		_ = unmarkIndexDirty(cfg, op.OpID)
		return err
	}
	if !found {
		_ = unmarkIndexDirty(cfg, op.OpID)
		return fmt.Errorf("no active governance entry %q", args[0])
	}
	if _, err := rebuildIndexWithPolicy(ctx, cfg, policyAllow); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "undid merge decision %s\n", args[0])
	return nil
}

// loadGraphMemories loads ALL vault memories (including connector tombstones, exactly
// as the index rebuild's graph pass does) so candidate detection sees the same person
// aggregates buildGraphResult builds. Unparseable files are skipped, never acted on.
func loadGraphMemories(cfg Config) ([]Memory, error) {
	paths, err := allMemoryFiles(cfg)
	if err != nil {
		return nil, err
	}
	out := make([]Memory, 0, len(paths))
	governance, err := loadGovernance(cfg)
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		m, err := parseMemory(p)
		if err != nil {
			continue
		}
		if governance.memoryVisible(m.ID) {
			out = append(out, m)
		}
	}
	return out, nil
}

func pendingMergeOf(c mergeCandidate, mems []Memory) pendingMerge {
	handle := personIdentity(c.PhoneID)
	email := personIdentity(c.EmailID)
	affected := mergeAffectedItems(mems, handle, email)
	return pendingMerge{
		Name:     c.Name,
		Handle:   handle,
		Email:    email,
		Echoed:   append([]string(nil), c.Echoed...),
		Affected: affected,
		Evidence: []mergeEvidence{
			{Kind: "address_book_name", Detail: fmt.Sprintf("%s is the trusted name for %s", c.Name, handle)},
			{Kind: "email_name", Detail: fmt.Sprintf("%s self-presents as %s", email, c.Name)},
			{Kind: "address_signature", Detail: fmt.Sprintf("email local-part corroborates token(s): %s", strings.Join(c.Echoed, ", "))},
		},
	}
}

func mergeAffectedItems(mems []Memory, handle, email string) []string {
	seen := map[string]bool{}
	for _, m := range mems {
		for _, atom := range counterpartyAtoms(m.Provider, m.Meta) {
			if (atom.Kind == atomHandle && atom.Value == normalizeIdentity(atomHandle, handle)) ||
				(atom.Kind == atomAddress && atom.Value == normalizeIdentity(atomAddress, email)) {
				seen[m.ID] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func mergeAffectedLabel(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

func pendingMergeForPair(cfg Config, mems []Memory, phoneAtom, emailAtom govAtom) (pendingMerge, error) {
	g, err := loadGovernance(cfg)
	if err != nil {
		return pendingMerge{}, err
	}
	confirmed, decided := g.mergeDecisions()
	res := buildGraphResult(mems, confirmed)
	key := mergePairKey(atomPersonID(phoneAtom), atomPersonID(emailAtom))
	if decided[key] {
		return pendingMerge{}, errors.New("this identity pair already has an active decision")
	}
	for _, candidate := range res.candidates {
		if mergePairKey(candidate.PhoneID, candidate.EmailID) == key {
			return pendingMergeOf(candidate, mems), nil
		}
	}
	// Manual escape hatch: the human may know a pair the precision-first proposer
	// refused. There is no claimed corroboration; say so and still enumerate impact.
	return pendingMerge{
		Handle:   phoneAtom.Value,
		Email:    emailAtom.Value,
		Affected: mergeAffectedItems(mems, phoneAtom.Value, emailAtom.Value),
		Evidence: []mergeEvidence{{Kind: "manual_override", Detail: "no active corroborated proposal; this confirmation relies on the human review"}},
	}, nil
}
