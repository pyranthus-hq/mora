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
func cmdForget(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) > 0 && args[0] == "list" {
		return forgetList(stdout)
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

	// Record the durable suppression FIRST, so a crash after removal can never
	// leave files removed but re-ingestable (the resurrection window).
	entry, err := appendGovernanceEntry(cfg, govEntry{
		Kind: govKindForget, Action: govActionSuppress, Atom: target,
		Reason: "mora forget " + label,
	})
	if err != nil {
		return err
	}
	removed := 0
	for _, m := range matches {
		// Mark-before-visible for the forget delete (A5 row 6): the file is about to
		// vanish, so a rebuild failure below must leave the index dirty and suppress
		// the removed id on reads, never keep serving forgotten content. The rebuild
		// retires each op (rule c: path gone).
		op, merr := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindDelete, Path: m.Path, MemoryID: m.ID})
		if merr != nil {
			return merr
		}
		if err := os.Remove(m.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = unmarkIndexDirty(cfg, op.OpID)
			return err
		}
		removed++
	}
	if _, err := rebuildIndexWithPolicy(ctx, cfg, policyAllow); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "forgot %s: removed %d %s; future syncs suppressed (entry %s)\n", label, removed, memWord(removed), entry.ID)
	fmt.Fprintln(stdout, "note: this stops Mora from holding and re-acquiring the content locally; it does NOT delete anything at Gmail/Apple. Reverse with `mora unforget "+entry.ID+"`.")
	return nil
}

// cmdUnforget reverses a forget: it revokes the suppression so future syncs may
// re-ingest the content again (subject to the connector's lookback window — an
// unforget is not a guaranteed restore of already-removed older content).
func cmdUnforget(ctx context.Context, args []string, stdout io.Writer) error {
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
	found, err := revokeGovernanceEntry(cfg, fs.Arg(0))
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no active governance entry %q", fs.Arg(0))
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
func memoriesMatching(cfg Config, target govAtom) ([]Memory, error) {
	probe := governance{Schema: governanceSchema, Entries: []govEntry{{
		Kind: govKindForget, Action: govActionSuppress, Atom: target,
	}}}
	paths, err := allMemoryFiles(cfg)
	if err != nil {
		return nil, err
	}
	var out []Memory
	for _, p := range paths {
		m, err := parseMemory(p)
		if err != nil {
			continue // unparseable files are skipped everywhere; don't act on them
		}
		if sup, _ := probe.decideSuppress(m.Provider, m.ID, m.Meta); sup {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// forgetList prints the active (non-revoked) suppressions.
func forgetList(stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		return err
	}
	active := g.activeSuppress()
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
