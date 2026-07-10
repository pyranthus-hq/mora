package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

// cmdBriefCorrect implements `mora brief correct`, the P16 click-to-correct action
// for cited meeting-brief lines. It records a stable-atom keyed decision in the
// governance confirm queue:
//   - --confirm keeps/pins this source memory line to the attendee atom.
//   - --unlink rejects this source↔attendee link (destructive; requires --yes).
//
// The key is ALWAYS source-native atoms (stable_id + handle/address), never a
// canonical person id, so the correction persists across connector re-sync.
func cmdBriefCorrect(ctx context.Context, args []string, stdout io.Writer) error {
	_ = ctx
	fs := flag.NewFlagSet("brief correct", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	memoryID := fs.String("memory-id", "", "cited source memory id")
	attendee := fs.String("attendee", "", "attendee identity atom (email or iMessage handle)")
	confirm := fs.Bool("confirm", false, "confirm this source↔attendee attribution")
	unlink := fs.Bool("unlink", false, "unlink this source↔attendee attribution (destructive)")
	yes := fs.Bool("yes", false, "confirm destructive unlink")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if *memoryID == "" || *attendee == "" {
		return errors.New("brief correct requires --memory-id <id> and --attendee <identity>")
	}
	if (*confirm && *unlink) || (!*confirm && !*unlink) {
		return errors.New("brief correct requires exactly one of --confirm or --unlink")
	}
	if *unlink && !*yes {
		return errors.New("refusing to unlink without --yes (destructive operation)")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	m, err := findMemory(cfg, *memoryID)
	if err != nil {
		return err
	}
	if m.DeletedAt != "" {
		return fmt.Errorf("memory %q is deleted", *memoryID)
	}
	attendeeAtom, err := attendeeAtomForIdentity(*attendee)
	if err != nil {
		return err
	}
	stableAtom := itemAtom(m.Provider, m.ID)
	decision := mergeDecisionConfirm
	if *unlink {
		decision = mergeDecisionReject
	}
	entry, err := appendGovernanceEntry(cfg, govEntry{
		Kind:     govKindRedact,
		Action:   govActionRecord,
		Atom:     stableAtom,
		Atom2:    &attendeeAtom,
		Decision: decision,
		Reason:   fmt.Sprintf("mora brief correct --memory-id %s --attendee %s --%s", m.ID, attendeeAtom.Value, map[bool]string{true: "unlink", false: "confirm"}[*unlink]),
	})
	if err != nil {
		return err
	}
	if decision == mergeDecisionConfirm {
		fmt.Fprintf(stdout, "confirmed citation link: %s ↔ %s (entry %s)\n", stableAtom.Value, attendeeAtom.Value, entry.ID)
		fmt.Fprintln(stdout, "this line attribution is pinned and will persist across re-sync")
		return nil
	}
	fmt.Fprintf(stdout, "unlinked citation: %s ↔ %s (entry %s)\n", stableAtom.Value, attendeeAtom.Value, entry.ID)
	fmt.Fprintln(stdout, "future briefs will suppress this line for that attendee (persists across re-sync)")
	return nil
}
