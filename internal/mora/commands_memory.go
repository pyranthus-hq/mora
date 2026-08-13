package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

func cmdWrite(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("write", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scope := fs.String("scope", "global", "scope")
	mtype := fs.String("type", "insight", "memory type")
	title := fs.String("title", "", "title")
	text := fs.String("text", "", "text")
	tags := fs.String("tags", "", "comma-separated tags")
	source := fs.String("source", "manual", "source")
	asOf := fs.String("as-of", "", "decision validity instant (RFC3339)")
	durability := fs.String("durability", "", "decision durability: provisional|working|standing")
	flip := fs.String("flip-conditions", "", "semicolon-separated conditions that reverse a decision")
	reviewBy := fs.String("review-by", "", "optional decision review deadline (RFC3339)")
	jsonOut := fs.Bool("json", false, "json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *text == "" && fs.NArg() > 0 {
		*text = strings.Join(fs.Args(), " ")
	}
	if *title == "" || *text == "" {
		return errors.New("--title and --text are required")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	m := Memory{Scope: *scope, Type: *mtype, Title: *title, Tags: genericutil.SplitCSV(*tags), Source: *source, CreatedAt: time.Now().Format(time.RFC3339), Text: *text}
	if m.Type == "decision" {
		m.Decision = decisionValidityFromFlags(m.CreatedAt, *asOf, *durability, *flip, *reviewBy)
	} else if *asOf != "" || *durability != "" || *flip != "" || *reviewBy != "" {
		return errors.New("decision validity flags require --type decision")
	}
	// Create-exclusive publish: a colliding newID can never clobber an existing
	// memory (os.Link fails EEXIST → re-mint), so a same-instant concurrent writer
	// never silently loses its write. createMemory sets m.ID and m.Path.
	m, _, err = createMemory(ctx, cfg, m)
	if err != nil {
		return err
	}
	m = decorateDecision(m, time.Now())
	// The vault write already succeeded (vault is truth; the index is a derived
	// cache). Reflect just this one memory into the index (O(1)) instead of a full
	// vault rebuild, so concurrent agent writers don't serialize whole-vault
	// rebuilds. A BLOCKED index update — vault looks empty or unfamiliar — must NOT
	// fail the write: failing here would lose nothing on disk but would report the
	// save as failed, inviting a retry that mints a duplicate memory. Mirror the MCP
	// write_memory degraded-success path: warn loudly, still emit the saved
	// memory, and exit 0. Any OTHER index error is a genuine failure → surface it.
	if err := indexUpsert(ctx, cfg, m); err != nil {
		// The op REMAINS on failure — the vault has the memory, the index does not,
		// so the index reads dirty until a rebuild covers it (A2). Do not retire it.
		if errors.Is(err, errRebuildBlocked) {
			fmt.Fprintf(stdout, "warning: memory saved but the search index was not updated (vault looks empty or unfamiliar); run `mora index rebuild --force` after checking vault_dir\n")
			return emit(stdout, m, *jsonOut)
		}
		return err
	}
	// Keep the marker until the elected full reconciliation has committed: indexUpsert
	// makes FTS immediately visible, but only rebuildIndex can make graph, vectors,
	// and commitments byte-identical to the vault.
	if err := reconcileAuthoredWrites(ctx, cfg); err != nil {
		fmt.Fprintf(stdout, "warning: memory saved and text search updated, but full index reconciliation is pending: %v\n", err)
	}
	return emit(stdout, m, *jsonOut)
}
func cmdRead(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "json")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("read requires memory id")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	m, err := findMemory(cfg, fs.Arg(0))
	if err != nil {
		// Read-only fallback: ids from subscribed share corpora are searchable,
		// so they must be readable too. Delete paths never take this fallback.
		if sm, ok := findSharedMemory(cfg, fs.Arg(0)); ok {
			if !*jsonOut {
				printHealthBannerLine(stdout, cfg, time.Now())
			}
			return emit(stdout, sm, *jsonOut)
		}
		return err
	}
	if !*jsonOut {
		printHealthBannerLine(stdout, cfg, time.Now())
	}
	return emit(stdout, m, *jsonOut)
}
func cmdList(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scope := fs.String("scope", "", "scope")
	limit := fs.Int("limit", 20, "limit")
	jsonOut := fs.Bool("json", false, "json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	items, err := listMemories(cfg, *scope, *limit)
	if err != nil {
		return err
	}
	if !*jsonOut {
		printHealthBannerLine(stdout, cfg, time.Now())
	}
	return emit(stdout, items, *jsonOut)
}
func cmdSearch(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) >= 1 && genericutil.IsHelpFlag(args[0]) {
		fmt.Fprintln(stdout, "usage: mora search <query> [--scope S] [--limit N] [--json]")
		return nil
	}
	scope, limit, jsonOut, queryArgs, err := parseSearchArgs(args)
	if err != nil {
		return err
	}
	if len(queryArgs) < 1 {
		return errors.New("search requires query")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	items, err := defaultSearch(ctx, cfg, strings.Join(queryArgs, " "), scope, limit)
	if err != nil {
		return err
	}
	if !jsonOut {
		printHealthBannerLine(stdout, cfg, time.Now())
	}
	return emit(stdout, items, jsonOut)
}
func cmdDelete(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "yes")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("delete requires memory id")
	}
	if !*yes {
		return errors.New("refusing to delete without --yes")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	m, err := findMemory(cfg, fs.Arg(0))
	if err != nil {
		return err
	}
	// Mark the delete BEFORE removing the file (A5 row 4): a rebuild that fails
	// after the file is gone would otherwise keep SERVING the deleted content
	// (Finding 4 — a data-safety P0). While the op is pending its memory_id is
	// suppressed on every read path (B4), so fail-closed here is an actual
	// guarantee, not just a banner.
	op, merr := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindDelete, Path: m.Path, MemoryID: m.ID})
	if merr != nil {
		return merr
	}
	if err := os.Remove(m.Path); err != nil {
		_ = unmarkIndexDirty(cfg, op.OpID) // the file never went away
		return err
	}
	if _, err := rebuildIndexWithPolicy(ctx, cfg, policyAllow); err != nil {
		// The op REMAINS -> the index reads dirty AND B4 suppresses the deleted id.
		return fmt.Errorf("memory %s deleted, but the search index could not be updated: %w — run `mora index rebuild`", m.ID, err)
	}
	fmt.Fprintf(stdout, "deleted %s\n", m.ID)
	return nil
}
func cmdContext(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scope := fs.String("scope", "", "scope")
	query := fs.String("query", "", "query")
	budget := fs.Int("budget", 0, "token budget (default: profile default)")
	jsonOut := fs.Bool("json", false, "json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	tokenBudget, charBudget := resolveContextBudgetTokens(cfg, *budget)
	var items []Memory
	if *query != "" {
		items, err = hybridSearch(ctx, cfg, *query, *scope, 10)
	} else {
		items, err = listMemories(cfg, *scope, 10)
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		// Receipts are budgeted FIRST and the blob gets the remainder (#200).
		// The old order let the blob spend the whole budget, so items[] came
		// back empty on every vault big enough to fill it — and the receipts'
		// own JSON pushed `used` past `budget`. Reserving them up front keeps
		// the structured lane populated and used ≤ budget by construction.
		receipts := contextReceipts(items, charBudget)
		text := buildContext(cfg, items, charBudget-jsonLen(receipts), *query != "")
		used := estimateTokensUsed(len(text) + jsonLen(receipts))
		return emit(stdout, map[string]any{
			"context":     text,
			"items":       receipts,
			"budget_unit": budgetUnitTokens,
			"budget":      tokenBudget,
			"used":        used,
		}, true)
	}
	printHealthBannerLine(stdout, cfg, time.Now())
	fmt.Fprint(stdout, buildContext(cfg, items, charBudget, *query != ""))
	return nil
}

// cmdThink implements `mora think "<query>" [--scope s] [--limit n] [--json]`:
// the I3 synthesis envelope (hybrid evidence + deterministic gap analysis + a
// synthesis prompt the calling agent's model runs). The deterministic floor is
// fully useful with no model attached.
func cmdThink(ctx context.Context, args []string, stdout io.Writer) error {
	jsonOut := false
	scope := ""
	limit := 8
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--scope":
			if i+1 < len(args) {
				i++
				scope = args[i]
			}
		case "--limit":
			if i+1 < len(args) {
				i++
				if n, perr := strconv.Atoi(args[i]); perr == nil && n > 0 {
					limit = n
				}
			}
		default:
			positional = append(positional, args[i])
		}
	}
	query := strings.TrimSpace(strings.Join(positional, " "))
	if query == "" {
		return errors.New(`usage: mora think "<question>" [--scope s] [--limit n] [--json]`)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	res, err := buildThink(ctx, cfg, query, scope, limit, time.Now())
	if err != nil {
		return err
	}
	logUsage(cfg, usageEvent{Tool: "think", Query: query, Scope: scope, Results: len(res.Evidence)})
	if jsonOut {
		return emit(stdout, res, true)
	}
	printHealthBannerLine(stdout, cfg, time.Now())
	printThink(stdout, res)
	return nil
}
func printThink(w io.Writer, res ThinkResult) {
	fmt.Fprintf(w, "Q: %s\n\nEvidence (%d):\n", res.Query, len(res.Evidence))
	for _, e := range res.Evidence {
		fmt.Fprintf(w, "  [%s] %s — %s\n", e.StableID, e.Title, e.Snippet)
	}
	if res.Gaps.empty() {
		fmt.Fprintln(w, "\nGaps: none detected.")
	} else {
		fmt.Fprintln(w, "\nWhat the vault does NOT know:")
		for _, s := range res.Gaps.Stale {
			fmt.Fprintf(w, "  · %s\n", s)
		}
		for _, s := range res.Gaps.FreshnessUnknown {
			fmt.Fprintf(w, "  · %s\n", s)
		}
		for _, s := range res.Gaps.SparseEvidence {
			fmt.Fprintf(w, "  · %s\n", s)
		}
		for _, s := range res.Gaps.SourceCoverage {
			fmt.Fprintf(w, "  · %s\n", s)
		}
		for _, s := range res.Gaps.TemporalState {
			fmt.Fprintf(w, "  · %s\n", s)
		}
		for _, s := range res.Gaps.ThinCoverage {
			fmt.Fprintf(w, "  · %s\n", s)
		}
		for _, s := range res.Gaps.CoverageHoles {
			fmt.Fprintf(w, "  · %s\n", s)
		}
		for _, s := range res.Gaps.RetrievalCaveats {
			fmt.Fprintf(w, "  · %s\n", s)
		}
	}
	fmt.Fprintln(w, "\n(Pass this evidence + gaps to your agent, or run `mora think … --json` for the synthesis prompt.)")
}
