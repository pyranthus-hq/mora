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

func cmdTeach(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora teach <identity|commitment|memory|undo|history|consent|examples>")
	}
	switch args[0] {
	case "identity":
		if len(args) == 1 {
			return errors.New("usage: mora teach identity <list|confirm|reject|undo>")
		}
		// `teach identity …` is an alias for `merge …` but publishes its own
		// schema names, so the namespace rides the context rather than forcing a
		// signature change on every command handler.
		return cmdMerge(withMergeSchemaNamespace(ctx, mergeSchemaTeachIdentity), args[1:], stdout, stderr)
	case "commitment":
		return teachCommitment(ctx, args[1:], stdout)
	case "memory":
		return teachMemory(ctx, args[1:], stdout)
	case "undo":
		return teachUndo(ctx, args[1:], stdout)
	case "history":
		return teachHistory(args[1:], stdout)
	case "consent":
		return teachConsent(args[1:], stdout)
	case "examples":
		return teachExamples(args[1:], stdout)
	default:
		return fmt.Errorf("unknown teach subcommand %q", args[0])
	}
}

func normalizeTeachDecision(raw string) string {
	return strings.ReplaceAll(strings.TrimSpace(raw), "-", "_")
}

func teachCommitment(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora teach commitment <not-a-commitment|wrong-person|wrong-direction|already-closed|duplicate|useful> --memory-id <id> [options] --yes")
	}
	decision := normalizeTeachDecision(args[0])
	fs := flag.NewFlagSet("teach commitment "+decision, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	memoryID := fs.String("memory-id", "", "opening memory id")
	commitmentID := fs.String("commitment-id", "", "commitment id (required when the memory has multiple commitments)")
	person := fs.String("person", "", "corrected counterparty email or handle")
	personKind := fs.String("person-kind", "", "address or handle (inferred when omitted)")
	direction := fs.String("direction", "", "owed_by_self or owed_by_counterparty")
	duplicateOf := fs.String("duplicate-of", "", "canonical commitment id")
	reason := fs.String("reason", "", "human review note")
	yes := fs.Bool("yes", false, "confirm the local decision")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !*yes {
		return errors.New("refusing to record a Teach decision without --yes")
	}
	if *memoryID == "" {
		return errors.New("--memory-id is required")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	target, err := resolveTeachCommitment(ctx, cfg, *memoryID, *commitmentID)
	if err != nil {
		return err
	}
	if decision == teachDuplicate {
		canonical := strings.TrimSpace(*duplicateOf)
		if canonical == target.ID {
			return errors.New("--duplicate-of cannot name the corrected commitment itself")
		}
		if _, err := resolveTeachCommitmentID(ctx, cfg, canonical); err != nil {
			return err
		}
	}
	entry := govEntry{
		Kind:         govKindTeachCommitment,
		Action:       govActionRecord,
		TargetID:     *memoryID,
		CommitmentID: target.ID,
		Decision:     decision,
		DuplicateOf:  strings.TrimSpace(*duplicateOf),
		Reason:       strings.TrimSpace(*reason),
	}
	if entry.Reason == "" {
		entry.Reason = "mora teach commitment " + strings.ReplaceAll(decision, "_", "-")
	}
	if *person != "" {
		kind := strings.TrimSpace(*personKind)
		if kind == "" {
			if strings.Contains(*person, "@") {
				kind = atomAddress
			} else {
				kind = atomHandle
			}
		}
		atom := govAtom{Kind: kind, Value: normalizeIdentity(kind, *person)}
		if kind == atomHandle {
			atom.Provider = "imessage"
		}
		entry.CorrectedAtom = &atom
	}
	entry.CorrectedDirection = Direction(strings.TrimSpace(*direction))
	if err := validateTeachCommitmentEntry(entry); err != nil {
		return err
	}
	stored, err := appendTeachAndRebuild(ctx, cfg, entry)
	if err != nil {
		return err
	}
	if *jsonOut {
		return emitReceipt(stdout, "mora.teach.commitment", 1, stored)
	}
	fmt.Fprintf(stdout, "recorded %s for commitment %s in %s (entry %s)\n",
		decision, displayCommitmentID(target), *memoryID, stored.ID)
	fmt.Fprintf(stdout, "reverse with `mora teach undo %s`\n", stored.ID)
	return nil
}

func resolveTeachCommitmentID(ctx context.Context, cfg Config, id string) (Commitment, error) {
	if id == "" {
		return Commitment{}, errors.New("duplicate requires --duplicate-of <commitment-id>")
	}
	snapshot, err := readCommitmentSnapshot(ctx, cfg)
	if err != nil {
		return Commitment{}, err
	}
	for _, c := range snapshot.Commitments {
		if c.ID == id {
			return c, nil
		}
	}
	return Commitment{}, fmt.Errorf("duplicate target commitment not found: %s", id)
}

func displayCommitmentID(c Commitment) string {
	if c.ID != "" {
		return c.ID
	}
	return "<legacy single commitment>"
}

func resolveTeachCommitment(ctx context.Context, cfg Config, memoryID, commitmentID string) (Commitment, error) {
	snapshot, err := readCommitmentSnapshot(ctx, cfg)
	if err != nil {
		return Commitment{}, err
	}
	var matches []Commitment
	for _, c := range snapshot.Commitments {
		if c.OpenedBy.MemoryID != memoryID {
			continue
		}
		if commitmentID != "" && c.ID != commitmentID {
			continue
		}
		matches = append(matches, c)
	}
	if len(matches) == 0 {
		return Commitment{}, fmt.Errorf("no current commitment matches memory %s", memoryID)
	}
	if len(matches) > 1 {
		return Commitment{}, fmt.Errorf("memory %s has %d commitments; pass --commitment-id", memoryID, len(matches))
	}
	return matches[0], nil
}

func appendTeachAndRebuild(ctx context.Context, cfg Config, entry govEntry) (govEntry, error) {
	op, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindRebuild, MemoryID: entry.TargetID})
	if err != nil {
		return govEntry{}, err
	}
	stored, err := appendGovernanceEntry(cfg, entry)
	if err != nil {
		_ = unmarkIndexDirty(cfg, op.OpID)
		return govEntry{}, err
	}
	if _, err := rebuildIndexWithPolicy(ctx, cfg, policyAllow); err != nil {
		return stored, err
	}
	return stored, nil
}

func teachMemory(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora teach memory <correct|supersede|retract> --id <memory-id> [options] --yes")
	}
	decision := normalizeTeachDecision(args[0])
	if !teachMemoryDecisionValid(decision) {
		return fmt.Errorf("unknown memory decision %q", args[0])
	}
	fs := flag.NewFlagSet("teach memory "+decision, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	id := fs.String("id", "", "stable authored-memory id")
	title := fs.String("title", "", "replacement title")
	text := fs.String("text", "", "replacement text")
	scope := fs.String("scope", "", "replacement scope (defaults to original)")
	mtype := fs.String("type", "", "replacement type (defaults to original)")
	tags := fs.String("tags", "", "replacement tags")
	asOf := fs.String("as-of", "", "decision validity instant (RFC3339)")
	durability := fs.String("durability", "", "provisional, working, or standing")
	flip := fs.String("flip-conditions", "", "semicolon-separated conditions that reverse the decision")
	reviewBy := fs.String("review-by", "", "optional review deadline (RFC3339)")
	reason := fs.String("reason", "", "human review note")
	yes := fs.Bool("yes", false, "confirm the local decision")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !*yes {
		return errors.New("refusing to change current memory state without --yes")
	}
	if *id == "" {
		return errors.New("--id is required")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	original, err := findMemoryRaw(cfg, *id)
	if err != nil {
		return err
	}
	if original.Provider != "" {
		return errors.New("teach memory revisions apply only to authored memories; connector evidence is immutable")
	}
	governance, err := loadGovernance(cfg)
	if err != nil {
		return err
	}
	if !governance.memoryVisible(original.ID) {
		return errors.New("memory is historical, not current; undo the active revision before changing it")
	}
	entry := govEntry{
		Kind:     govKindTeachMemory,
		Action:   govActionRecord,
		TargetID: original.ID,
		Decision: decision,
		Reason:   strings.TrimSpace(*reason),
	}
	if entry.Reason == "" {
		entry.Reason = "mora teach memory " + decision
	}

	var replacement Memory
	var replacementOp pendingOp
	if decision != teachMemoryRetract {
		if *title == "" || *text == "" {
			return errors.New("correct and supersede require --title and --text")
		}
		replacement = Memory{
			Scope:     original.Scope,
			Type:      original.Type,
			Title:     *title,
			Text:      *text,
			Tags:      append([]string(nil), original.Tags...),
			Source:    original.Source,
			CreatedAt: nowRFC3339(),
		}
		if *scope != "" {
			replacement.Scope = *scope
		}
		if *mtype != "" {
			replacement.Type = *mtype
		}
		if *tags != "" {
			replacement.Tags = splitCSV(*tags)
		}
		if replacement.Type == "decision" {
			replacement.Decision = decisionValidityFromFlags(replacement.CreatedAt, *asOf, *durability, *flip, *reviewBy)
		}
		replacement, replacementOp, err = createMemory(ctx, cfg, replacement)
		if err != nil {
			return err
		}
		entry.ReplacementID = replacement.ID
	}

	stored, err := appendTeachAndRebuild(ctx, cfg, entry)
	if err != nil {
		// Roll back the newly-created file only if the governance append never
		// committed. Once stored.ID exists the revision is durable truth; a failed
		// rebuild must leave both file and dirty marker for recovery.
		if stored.ID == "" && replacement.Path != "" {
			_ = os.Remove(replacement.Path)
			_ = unmarkIndexDirty(cfg, replacementOp.OpID)
		}
		return err
	}
	if *jsonOut {
		return emitReceipt(stdout, "mora.teach.memory", 1, map[string]any{"decision": stored, "replacement": replacement})
	}
	if replacement.ID != "" {
		fmt.Fprintf(stdout, "%s %s with revision %s (entry %s)\n", decision, original.ID, replacement.ID, stored.ID)
	} else {
		fmt.Fprintf(stdout, "retracted %s from current-state reads (entry %s)\n", original.ID, stored.ID)
	}
	fmt.Fprintf(stdout, "reverse with `mora teach undo %s`\n", stored.ID)
	return nil
}

func decisionValidityFromFlags(createdAt, asOf, durability, flip, reviewBy string) *DecisionValidity {
	if asOf == "" {
		asOf = createdAt
	}
	if durability == "" {
		durability = decisionProvisional
	}
	return normalizeDecisionValidity(Memory{
		Type:      "decision",
		CreatedAt: createdAt,
		Decision: &DecisionValidity{
			AsOf:           asOf,
			Durability:     durability,
			FlipConditions: strings.Split(flip, ";"),
			ReviewBy:       reviewBy,
		},
	})
}

func teachUndo(ctx context.Context, args []string, stdout io.Writer) error {
	jsonOut := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
			continue
		}
		rest = append(rest, a)
	}
	args = rest
	if len(args) != 1 {
		return errors.New("teach undo requires one governance entry id")
	}
	if err := refuseDashLedPositional("teach undo", "entry id", args[0]); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		return err
	}
	var target *govEntry
	for i := range g.Entries {
		if g.Entries[i].ID == args[0] && !g.Entries[i].revoked() {
			target = &g.Entries[i]
			break
		}
	}
	if target == nil || (target.Kind != govKindTeachCommitment && target.Kind != govKindTeachMemory) {
		return fmt.Errorf("no active Teach decision %q", args[0])
	}
	if target.ReplacementID != "" {
		for _, e := range g.Entries {
			if !e.revoked() && e.Kind == govKindTeachMemory && e.TargetID == target.ReplacementID {
				return fmt.Errorf("cannot undo %s while later revision %s is active; undo newest first", target.ID, e.ID)
			}
		}
	}
	op, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindRebuild, MemoryID: target.TargetID})
	if err != nil {
		return err
	}
	found, err := revokeGovernanceEntry(cfg, args[0])
	if err != nil {
		_ = unmarkIndexDirty(cfg, op.OpID)
		return err
	}
	if !found {
		_ = unmarkIndexDirty(cfg, op.OpID)
		return fmt.Errorf("no active Teach decision %q", args[0])
	}
	if _, err := rebuildIndexWithPolicy(ctx, cfg, policyAllow); err != nil {
		return err
	}
	if jsonOut {
		return emitReceipt(stdout, "mora.teach.undo", 1, teachUndoReceipt{EntryID: args[0], Revoked: true})
	}
	fmt.Fprintf(stdout, "undid Teach decision %s\n", args[0])
	return nil
}

// teachUndoReceipt is the machine form of a revoked Teach decision.
type teachUndoReceipt struct {
	EntryID string `json:"entry_id"`
	Revoked bool   `json:"revoked"`
}

// teachHistoryReceipt carries the decision log under a named key with a
// never-null array, so an agent can iterate it without a nil check.
type teachHistoryReceipt struct {
	Entries []govEntry `json:"entries"`
}

func teachHistory(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("teach history", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	memoryID := fs.String("memory-id", "", "filter by target or replacement memory id")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		return err
	}
	teaching := g.teachingEntries()
	connectedMemoryIDs := map[string]bool{}
	if *memoryID != "" {
		connectedMemoryIDs[*memoryID] = true
		for changed := true; changed; {
			changed = false
			for _, e := range teaching {
				if e.Kind != govKindTeachMemory ||
					(!connectedMemoryIDs[e.TargetID] && !connectedMemoryIDs[e.ReplacementID]) {
					continue
				}
				for _, id := range []string{e.TargetID, e.ReplacementID} {
					if id != "" && !connectedMemoryIDs[id] {
						connectedMemoryIDs[id] = true
						changed = true
					}
				}
			}
		}
	}
	entries := make([]govEntry, 0, len(teaching))
	for _, e := range teaching {
		if *memoryID != "" {
			switch e.Kind {
			case govKindTeachMemory:
				if !connectedMemoryIDs[e.TargetID] && !connectedMemoryIDs[e.ReplacementID] {
					continue
				}
			default:
				if e.TargetID != *memoryID {
					continue
				}
			}
		}
		entries = append(entries, e)
	}
	if *jsonOut {
		return emitReceipt(stdout, "mora.teach.history", 1, teachHistoryReceipt{Entries: entries})
	}
	for _, e := range entries {
		status := "active"
		if e.revoked() {
			status = "undone"
		}
		fmt.Fprintf(stdout, "%s  %s  %s  target=%s  %s\n", e.ID, status, e.Decision, e.TargetID, e.CreatedAt)
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "no Teach history")
	}
	return nil
}

func teachConsent(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		return err
	}
	switch args[0] {
	case "status":
		return emitReceipt(stdout, "mora.teach.consent.status", 1, map[string]any{
			"evaluation_examples": g.evalConsentEnabled(),
			"privacy":             "structural fields only; raw memory text and identities are never exported",
		})
	case "enable", "disable":
		jsonOut := false
		rest := make([]string, 0, len(args))
		for _, a := range args[1:] {
			if a == "--json" {
				jsonOut = true
				continue
			}
			rest = append(rest, a)
		}
		if len(rest) != 1 || rest[0] != "--yes" {
			return errors.New("consent change requires --yes")
		}
		entry, err := appendGovernanceEntry(cfg, govEntry{
			Kind:     govKindEvalConsent,
			Action:   govActionRecord,
			Decision: args[0],
			Reason:   "explicit local consent for privacy-minimized evaluation examples",
		})
		if err != nil {
			return err
		}
		if jsonOut {
			return emitReceipt(stdout, "mora.teach.consent."+args[0], 1, teachConsentReceipt{
				Decision: args[0], Enabled: args[0] == "enable", EntryID: entry.ID,
			})
		}
		fmt.Fprintf(stdout, "evaluation-example consent %sd (entry %s)\n", args[0], entry.ID)
		return nil
	default:
		return fmt.Errorf("unknown consent action %q", args[0])
	}
}

type teachExample struct {
	Ref      string `json:"ref"`
	Kind     string `json:"kind"`
	Decision string `json:"decision"`
	Undone   bool   `json:"undone"`
}

func teachExamples(args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] != "--json" {
		return errors.New("usage: mora teach examples --json")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		return err
	}
	if !g.evalConsentEnabled() {
		// A typed refusal: an agent detects "consent required" from the code
		// instead of parsing this sentence. Mora observed the closed gate in its
		// own governance ledger, so the code states a fact, not an inference.
		return newCodedError(errCodeConsentRequired, nil,
			"evaluation examples are disabled; enable explicitly with `mora teach consent enable --yes`")
	}
	var entries []govEntry
	for _, e := range g.teachingEntries() {
		if e.Kind != govKindTeachCommitment && e.Kind != govKindTeachMemory {
			continue
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreatedAt != entries[j].CreatedAt {
			return entries[i].CreatedAt < entries[j].CreatedAt
		}
		return entries[i].ID < entries[j].ID
	})
	out := make([]teachExample, 0, len(entries))
	for i, e := range entries {
		out = append(out, teachExample{
			Ref:      fmt.Sprintf("example-%04d", i+1),
			Kind:     e.Kind,
			Decision: e.Decision,
			Undone:   e.revoked(),
		})
	}
	// Plan 01-07: the bare array moves under `examples`.
	return emitReceipt(stdout, "mora.teach.examples", 1, teachExamplesPayload{Examples: out})
}

// teachExamplesPayload carries the privacy-minimized examples under a named key.
type teachExamplesPayload struct {
	Examples []teachExample `json:"examples"`
}

// teachConsentReceipt is the machine form of a consent change.
type teachConsentReceipt struct {
	Decision string `json:"decision"`
	Enabled  bool   `json:"enabled"`
	EntryID  string `json:"entry_id"`
}
