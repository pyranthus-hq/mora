package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// cmdForget implements `mora forget` — the durable, local, cross-connector
// deletion verb Mora was missing (#52). Unlike `mora delete` (which a connector
// sync silently resurrects), forget writes a suppression to the governance
// ledger AND removes the matching memories now, so the hourly sync can never
// re-create them.
//
//	mora forget --chat <stable-id>   forget one conversation/thread/event
//	mora forget --handle <handle>    forget a 1:1 iMessage counterpart
//	mora forget --email <address>    forget a 1:1 email counterpart
//	mora forget list                 show active suppressions
//	  [--dry-run]  preview exactly which memories would be removed
//	  [--yes]      required to actually remove (destructive)
//
// v1 is ATOM-LEVEL and precise: --handle/--email remove only SOLE-COUNTERPARTY
// (1:1) memories; a group thread the identity merely appears in is kept (its
// per-participant redaction is deferred to P16). Person-level fan-out across an
// identity's aliases needs the identity graph (P13) and is deliberately out of
// scope here, so no false-merge can over-reach.
func cmdForget(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "list" {
		fs := flag.NewFlagSet("forget list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "emit JSON")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return newMoraError(errCodeUsageUnknownFlag, "usage", parseErr, "%v", parseErr)
		}
		if fs.NArg() != 0 {
			return newMoraError(errCodeUsageUnknownValue, "usage", nil, "unexpected argument %q", fs.Arg(0))
		}
		return forgetList(stdout, *jsonOut)
	}
	fs := flag.NewFlagSet("forget", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	chat := fs.String("chat", "", "forget one conversation/thread/event by stable id")
	handle := fs.String("handle", "", "forget a 1:1 iMessage counterpart by handle")
	email := fs.String("email", "", "forget a 1:1 email counterpart by address")
	dryRun := fs.Bool("dry-run", false, "preview which memories would be removed")
	yes := fs.Bool("yes", false, "confirm removal (destructive)")
	// NOT flagsFirst: forget's selectors (--chat/--handle/--email) are value-taking,
	// which flagsFirst would corrupt. forget takes no positionals, so plain Parse works.
	if err := fs.Parse(args); err != nil {
		return err
	}

	target, label, err := forgetTarget(*chat, *handle, *email)
	if err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	matches, err := memoriesMatching(cfg, target)
	if err != nil {
		return err
	}

	if *dryRun {
		fmt.Fprintf(stdout, "forget %s would remove %d %s:\n", label, len(matches), memWord(len(matches)))
		for _, m := range matches {
			fmt.Fprintf(stdout, "  %s\n", m.ID)
		}
		fmt.Fprintln(stdout, "(dry run — nothing changed; re-run with --yes to apply)")
		return nil
	}
	if !*yes {
		return fmt.Errorf("refusing to forget without --yes (removes %d %s; use --dry-run to preview)", len(matches), memWord(len(matches)))
	}

	// Gap A (#115): take the SAME governance lease every connector write uses,
	// rescan while holding it, mark every matched id fail-closed, then append the
	// suppression before releasing. A writer can therefore land either before
	// the scan (and be included) or after the append (and be suppressed), never
	// in the old scan→append hole.
	release, err := acquireGovernanceLock(cfg, time.Now())
	if err != nil {
		return err
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		release()
		return err
	}
	matches, err = memoriesMatching(cfg, target)
	if err != nil {
		release()
		return err
	}
	if testHookForgetAfterScan != nil {
		testHookForgetAfterScan()
	}
	ops := make([]pendingOp, 0, len(matches))
	for _, m := range matches {
		op, merr := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindDelete, Path: m.Path, MemoryID: m.ID})
		if merr != nil {
			for _, prior := range ops {
				_ = unmarkIndexDirty(cfg, prior.OpID)
			}
			release()
			return merr
		}
		ops = append(ops, op)
	}
	entry, err := appendGovernanceEntryLocked(cfg, g, govEntry{
		Kind: govKindForget, Action: govActionSuppress, Atom: target,
		Reason: "mora forget " + label,
	})
	if err != nil {
		for _, op := range ops {
			_ = unmarkIndexDirty(cfg, op.OpID)
		}
		release()
		return err
	}
	release()

	removed := 0
	for i, m := range matches {
		if err := os.Remove(m.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			// This file remains present, so its pending-delete suppression would
			// make the current read path lie. Retire only this op; the durable
			// governance suppression remains and a later forget can retry cleanup.
			_ = unmarkIndexDirty(cfg, ops[i].OpID)
			return err
		}
		removed++
	}
	if _, err := rebuildIndexWithPolicy(ctx, cfg, policyAllow); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "forgot %s: removed %d %s; future syncs suppressed (entry %s)\n", label, removed, memWord(removed), entry.ID)
	fmt.Fprintln(stderr, "note: this stops Mora from holding and re-acquiring the content locally; it does NOT delete anything at Gmail/Apple. Reverse with `mora unforget "+entry.ID+"`.")
	return nil
}

// testHookForgetAfterScan fires while cmdForget holds the governance lease after
// its removal scan and before its suppression append. Tests start a real competing
// connector write here and prove it cannot materialize after the forget commits.
var testHookForgetAfterScan func()

// cmdUnforget reverses a forget: it revokes the suppression so future syncs may
// re-ingest the content again (subject to the connector's lookback window — an
// unforget is not a guaranteed restore of already-removed older content).
func cmdUnforget(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("unforget", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "confirm")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("unforget requires a governance entry id (see `mora forget list`)")
	}
	if !*yes {
		return errors.New("refusing to unforget without --yes")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// A5 row 7: the governance ledger is an INDEX input (writeGraph loads it, so a
	// revoked suppression changes the graph the next rebuild derives). Mark the index
	// dirty BEFORE mutating the ledger and rebuild AFTER — a crash between the two
	// leaves the pending rebuild op, so the index reads dirty and never false-clean.
	// The committed rebuild retires the op (A3 rule a: marked_at <= listing_started_at).
	op, merr := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindRebuild})
	if merr != nil {
		return merr
	}
	found, err := revokeGovernanceEntry(cfg, fs.Arg(0))
	if err != nil {
		_ = unmarkIndexDirty(cfg, op.OpID) // the ledger never changed
		return err
	}
	if !found {
		_ = unmarkIndexDirty(cfg, op.OpID) // nothing revoked; no index change owed
		return fmt.Errorf("no active governance entry %q", fs.Arg(0))
	}
	if _, err := rebuildIndexWithPolicy(ctx, cfg, policyAllow); err != nil {
		// The op REMAINS -> the index reads dirty until a rebuild covers the change.
		return fmt.Errorf("unforgot %s, but the search index could not be updated: %w — run `mora index rebuild`", fs.Arg(0), err)
	}
	fmt.Fprintf(stdout, "unforgot %s; future syncs may re-ingest this content (within the connector's lookback window)\n", fs.Arg(0))
	return nil
}

// forgetTarget builds the stable-atom key from exactly one selector flag. A
// selector is trimmed FIRST so a whitespace-only value ("   ") counts as unset
// rather than passing the "exactly one" gate and minting an inert junk
// suppression that reports false success.
func forgetTarget(chat, handle, email string) (govAtom, string, error) {
	chat, handle, email = strings.TrimSpace(chat), strings.TrimSpace(handle), strings.TrimSpace(email)
	set := 0
	for _, s := range []string{chat, handle, email} {
		if s != "" {
			set++
		}
	}
	if set == 0 {
		return govAtom{}, "", errors.New("forget requires one of --chat <id>, --handle <handle>, --email <address> (or `mora forget list`)")
	}
	if set > 1 {
		return govAtom{}, "", errors.New("forget takes exactly one of --chat, --handle, --email")
	}
	switch {
	case chat != "":
		// Provider "" (wildcard): a stable id is already provider-namespaced
		// (gmail_thread/…, imessage_chat/…), so the user needn't name the provider.
		return govAtom{Kind: atomStableID, Value: chat}, "chat " + chat, nil
	case handle != "":
		return govAtom{Provider: "imessage", Kind: atomHandle, Value: normalizeIdentity(atomHandle, handle)}, "handle " + handle, nil
	default:
		return govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, email)}, "email " + email, nil
	}
}

// memoriesMatching scans the vault for the connector memories a suppression of
// `target` would remove — computed with the SAME decision the write chokepoint
// uses, so the removal set is exactly what will be suppressed on re-sync (never
// more). Authored notes carry no connector identity and never match.
//
// Pre-#115 attachment memories lack the parent-provenance Meta stamped by the
// current writer. Their stable id is nevertheless att_<hash(parent-id:path)> and
// their Source retains path, so the scan can reconstruct that one-way relation
// from a matching parent (or directly from a stable-id target). This migration
// path removes already-written legacy children immediately after upgrade rather
// than waiting for a re-ingest to rewrite them with current Meta.
func memoriesMatching(cfg Config, target govAtom) ([]Memory, error) {
	probe := governance{Schema: governanceSchema, Entries: []govEntry{{
		Kind: govKindForget, Action: govActionSuppress, Atom: target,
	}}}
	paths, err := allMemoryFiles(cfg)
	if err != nil {
		return nil, err
	}
	var parsed []Memory
	for _, p := range paths {
		m, err := parseMemory(p)
		if err != nil {
			continue // unparseable files are skipped everywhere; don't act on them
		}
		parsed = append(parsed, m)
	}

	matched := map[string]bool{}
	var matchingParents []Memory
	for _, m := range parsed {
		if sup, _ := probe.decideSuppress(m.Provider, m.ID, m.Meta); sup {
			matched[m.ID] = true
			if !strings.HasPrefix(m.ID, "att_") {
				matchingParents = append(matchingParents, m)
			}
		}
	}
	for _, attachment := range parsed {
		if matched[attachment.ID] || !strings.HasPrefix(attachment.ID, "att_") ||
			attachment.Source == "" {
			continue
		}
		if target.Kind == atomStableID &&
			attachment.ID == "att_"+memory.ContentHash(target.Value+":"+attachment.Source) {
			matched[attachment.ID] = true
			continue
		}
		for _, parent := range matchingParents {
			if attachment.ID == "att_"+memory.ContentHash(parent.ID+":"+attachment.Source) {
				matched[attachment.ID] = true
				break
			}
		}
	}

	out := make([]Memory, 0, len(matched))
	for _, m := range parsed {
		if matched[m.ID] {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// forgetList prints the active (non-revoked) suppressions.
type forgetListPayload struct {
	Entries []govEntry `json:"entries"`
}

func forgetList(stdout io.Writer, jsonOut bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		return err
	}
	active := g.activeSuppress()
	if jsonOut {
		entries := make([]govEntry, 0, len(active))
		entries = append(entries, active...)
		sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
		return emitReceipt(stdout, "mora.forget.list", 1, forgetListPayload{Entries: entries})
	}
	if len(active) == 0 {
		fmt.Fprintln(stdout, "no active suppressions")
		return nil
	}
	for _, e := range active {
		fmt.Fprintf(stdout, "%s  %s  %s=%s", e.ID, e.Kind, e.Atom.Kind, e.Atom.Value)
		if e.Atom.Provider != "" {
			fmt.Fprintf(stdout, " (%s)", e.Atom.Provider)
		}
		fmt.Fprintln(stdout)
	}
	return nil
}

func memWord(n int) string {
	if n == 1 {
		return "memory"
	}
	return "memories"
}
