package mora

import (
	"archive/zip"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/applecal"
	"github.com/pyranthus-hq/mora/internal/githubissues"
	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/imessage"
	ingestpkg "github.com/pyranthus-hq/mora/internal/ingest"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func syncStatusFileThreshold(name string) time.Duration { return ingestpkg.StatusFileThreshold(name) }
func syncStatusFileState(st *memory.SyncStatus, threshold time.Duration, now time.Time) string {
	return ingestpkg.StatusFileState(st, threshold, now)
}
func persistSyncStatus(out io.Writer, path string, st *memory.SyncStatus, ingErr error) error {
	// A failure raised INSIDE memory.Ingest is typed HERE, not at ingestSource.
	// memory.Ingest stamps its own LastAttemptAt, which correctly trips
	// stampSyncAttemptFailure's inner-path guard (health.go) — that guard exists
	// so the outer stamp can never clobber a checkpoint or counter the inner path
	// already persisted, and two tests pin it. The outer stamp therefore never
	// runs for this family, and without this line the whole dropped-item and
	// fetch-failure family would persist prose with no code (or, worse, keep a
	// code left by an earlier and different failure). Every memory.Ingest call
	// site routes through this function, so this is the complete boundary for it.
	if ingErr != nil {
		st.ErrorCode = connectorErrorCode(ingErr)
	}
	saveErr, result := ingestpkg.PersistStatus(path, st, ingErr)
	if saveErr != nil && out != nil {
		warnf(out, "could not persist sync status (%s): %v", path, saveErr)
	}
	return result
}
func googleStatusPath(cfg Config, name string) string {
	return ingestpkg.StatusPath(cfg, "google", name)
}
func imessageStatusPath(cfg Config, name string) string {
	return ingestpkg.StatusPath(cfg, "imessage", name)
}
func appleCalStatusPath(cfg Config, name string) string {
	return ingestpkg.StatusPath(cfg, "applecal", name)
}
func sourceFreshness(cfg Config) map[string]string { return ingestpkg.SourceFreshness(cfg) }

func ingestOperationSourceKey(s Source) string { return ingestpkg.OperationSourceKey(s) }
func windowForSource(s Source, k google.ItemKind) google.FetchWindow {
	return ingestpkg.GoogleWindow(s, k, time.Now())
}
func googleTokenPathFor(cfg Config, account string) string {
	return ingestpkg.GoogleTokenPath(cfg, account)
}
func iMessageLookbackDays(s Source) int             { return ingestpkg.IMessageLookbackDays(s) }
func windowForIMessage(s Source) memory.FetchWindow { return ingestpkg.IMessageWindow(s, time.Now()) }
func windowForAppleCal(s Source, now time.Time) memory.FetchWindow {
	return ingestpkg.AppleCalendarWindow(s, now)
}
func defaultFilesystemSourceName(path string) string {
	return ingestpkg.DefaultFilesystemSourceName(path)
}
func curatedAllowedExt(ext string) bool    { return ingestpkg.CuratedAllowedExt(ext) }
func curatedExtractExt(ext string) bool    { return ingestpkg.CuratedExtractExt(ext) }
func curatedMetadataFile(name string) bool { return ingestpkg.CuratedMetadataFile(name) }

func backfillEnabledGoogle(ctx context.Context, cfg Config, stdout io.Writer) (int, error) {
	return runEnabledSourceBackfill(ctx, cfg, stdout, func(s Source) bool {
		return s.Type == "gmail" || s.Type == "calendar"
	}, func(s Source, e error) {
		if isGoogleAuthError(e) {
			// CROSS-PHASE TOUCH (UI-SPEC §C): name the real cause + fix for the
			// 7-day Testing-mode refresh-token trap instead of a bare resumable warn.
			warnf(stdout, "%s sync incomplete: Google sign-in expired — run `mora connect google` to sign in again.", s.Name)
			fmt.Fprintln(stdout, "(If this keeps happening every ~7 days, your Google app is in \"Testing\" mode; switch it to \"Production\" for durable access.)")
		} else {
			warnf(stdout, "%s sync incomplete (resumable): %v", s.Name, e)
		}
	})
}

// backfillEnabledFilesystem re-walks every enabled filesystem source, then
// rebuilds the derived index once so the refreshed files are immediately
// searchable. A failure in one source does not starve the remaining sources or
// skip the rebuild, but it is still returned at the end: a partial snapshot must
// never be reported as wholly fresh.
func backfillEnabledFilesystem(ctx context.Context, cfg Config, stdout io.Writer) (int, error) {
	return runEnabledSourceBackfill(ctx, cfg, stdout, func(s Source) bool { return s.Type == "filesystem" }, func(s Source, err error) {
		warnf(stdout, "%s sync incomplete: %v", s.Name, err)
	})
}

func backfillEnabledGitHub(ctx context.Context, cfg Config, stdout io.Writer) (int, error) {
	return runEnabledSourceBackfill(ctx, cfg, stdout, func(s Source) bool { return s.Type == "github" }, func(s Source, err error) {
		warnf(stdout, "%s sync incomplete (prior evidence preserved): %v", s.Name, err)
	})
}

func runEnabledSourceBackfill(ctx context.Context, cfg Config, stdout io.Writer, selected func(Source) bool, report func(Source, error)) (int, error) {
	sources, err := loadSources(cfg)
	if err != nil {
		return 0, fmt.Errorf("load sources: %w", err)
	}
	var plans []sourceRunPlan
	for _, s := range sources {
		if !s.IsEnabled() || !selected(s) {
			continue
		}
		plans = append(plans, sourceRunPlan{Key: healthInstanceKeyForSource(s), Source: s})
	}
	output := io.Writer(&synchronizedWriter{w: stdout})
	outcomes := runSourcePlan(ctx, plans, sourceRunOptions{Run: func(runCtx context.Context, plan sourceRunPlan) sourceRunOutcome {
		result, runErr := ingestSourceFn(runCtx, cfg, plan.Source, output)
		return sourceRunOutcome{Key: plan.Key, Items: result.Materialized, Examined: result.Examined, Materialized: result.Materialized, Failed: result.Failed, Missing: result.Missing, Stages: result.Stages, Err: runErr}
	}})
	for i, outcome := range outcomes {
		if outcome.Err != nil && !outcome.Cancelled && report != nil {
			report(plans[i].Source, outcome.Err)
		}
	}
	_, rebuildErr := rebuildIndex(ctx, cfg)
	aggregate, aggregateErr := aggregateSourceRuns(plans, outcomes, nil, rebuildErr)
	if ctx.Err() != nil {
		return sourceOutcomesMaterialized(outcomes), ctx.Err()
	}
	if aggregateErr != nil && rebuildErr == nil && aggregate.FailedSources > 0 {
		aggregateErr = fmt.Errorf("%d source(s) failed to sync; data may be stale (run `mora sync status`): %w", aggregate.FailedSources, aggregateErr)
	}
	return sourceOutcomesMaterialized(outcomes), aggregateErr
}

func cmdIngest(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	if len(args) == 0 || args[0] != "run" {
		return newCodedError(errCodeUsageMissingArgument, nil, "usage: mora ingest run --source <name>|--all")
	}
	fs := flag.NewFlagSet("ingest run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sourceName := fs.String("source", "", "source")
	all := fs.Bool("all", false, "all")
	jsonOut := fs.Bool("json", false, "emit the ingest receipt as JSON")
	if perr := fs.Parse(args[1:]); perr != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", perr, "%v", perr)
	}
	// Per-source progress and resumable-failure warnings are diagnostics, not
	// results: under --json they move to stderr so stdout carries exactly one
	// JSON document.
	progress := progressWriter(stdout, stderr, *jsonOut)
	progress = &synchronizedWriter{w: progress}
	// One of the two selectors is required: a bare `ingest run` used to walk the
	// loop with sourceName=="", match nothing, and print a successful-looking
	// "ingested 0 item(s)".
	if !*all && *sourceName == "" {
		return newCodedError(errCodeUsageMissingArgument, nil, "usage: mora ingest run --source <name>|--all")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// ingest-hourly producer chokepoint (HEALTH-11): only the scheduled `--all` run
	// is the producer; a targeted `--source` run is an interactive one-off.
	if *all {
		defer stampChokepoint(cfg, stdout, args, "ingest-hourly", producerClock(), &err)
	}
	sources, err := loadSources(cfg)
	if err != nil {
		return err
	}
	count := 0
	named := false
	var namedErr error
	var plans []sourceRunPlan
	for _, s := range sources {
		// Enabled gate (D-07): a named disabled source ERRORS before the skip so
		// the user is never silently no-op'd; `--all` silently skips disabled.
		// The gate wraps the ingestSource CALLER, never ingestSource itself.
		if !*all {
			if s.Name != *sourceName {
				continue
			}
			named = true
			if !s.IsEnabled() {
				return fmt.Errorf("%s is disabled — run `mora connectors enable %s` first", s.Name, s.Type)
			}
		} else if !s.IsEnabled() {
			continue // --all silently skips disabled (D-07)
		}
		key := healthInstanceKeyForSource(s)
		plans = append(plans, sourceRunPlan{Key: key, Source: s})
	}
	// A named source that matched NOTHING is a typo, not a successful empty run:
	// error (exit 1) before the rebuild — no ingest happened, so there is nothing
	// to make searchable.
	if !*all && !named {
		return fmt.Errorf("no source named %q — run `mora sources list` to see configured sources", *sourceName)
	}
	outcomes := runSourcePlan(ctx, plans, sourceRunOptions{Run: func(runCtx context.Context, plan sourceRunPlan) sourceRunOutcome {
		result, runErr := ingestSourceFn(runCtx, cfg, plan.Source, progress)
		outcome := sourceRunOutcome{
			Key: plan.Key, Items: result.Materialized, Examined: result.Examined,
			Materialized: result.Materialized, Failed: result.Failed, Missing: result.Missing, Stages: result.Stages, Err: runErr,
		}
		if path := syncStatusPathFor(cfg, plan.Source); path != "" {
			if status, statusErr := memory.LoadStatus(path); statusErr == nil && status != nil {
				outcome.LastSuccessAt = status.LastSuccessAt
				outcome.LastAttemptAt = status.LastAttemptAt
				health := sourceHealthFor(cfg, plan.Source, plan.Key, briefClock())
				outcome.Stale = health.State != healthFresh
			}
		}
		return outcome
	}})
	for _, outcome := range outcomes {
		count += outcome.Materialized
		if outcome.Err != nil && !outcome.Cancelled && !outcome.TimedOut {
			if !*all {
				namedErr = outcome.Err
			} else {
				warnf(progress, "%s sync incomplete (resumable): %v", outcome.Key, outcome.Err)
			}
		}
	}
	var rebuildErr error
	if sourceOutcomesMaterialized(outcomes) > 0 && ctx.Err() == nil {
		_, rebuildErr = rebuildIngestIndexFn(ctx, cfg)
	}
	aggregate, aggregateErr := aggregateSourceRuns(plans, outcomes, nil, rebuildErr)
	// CON-02: every --json path that got as far as writing memories owes stdout
	// exactly one JSON document saying what landed. A partial named-source
	// failure and a failed rebuild both leave items in the vault, so returning
	// their error without the receipt handed an agent exit 1 and an empty
	// stdout — the one outcome it cannot act on. --all already emitted before
	// its aggregate error; these two paths did not.
	//
	// Errors raised BEFORE any ingest work (unknown source, disabled source,
	// missing selector) still emit nothing: a usage error is not a result.
	//
	// Human output is unchanged — the prose line still prints only on success.
	emitRunReceipt := func() error {
		if !*jsonOut {
			return nil
		}
		return emitReceipt(stdout, "mora.ingest.run", 1, ingestRunReceipt{
			Source: *sourceName, All: *all, Items: count, sourceRunAggregate: aggregate,
		})
	}
	if rebuildErr != nil {
		if eerr := emitRunReceipt(); eerr != nil {
			return eerr
		}
		if namedErr != nil {
			return fmt.Errorf("%w (and the index rebuild failed: %v — run `mora index rebuild`)", namedErr, rebuildErr)
		}
		return rebuildErr
	}
	if eerr := emitRunReceipt(); eerr != nil {
		return eerr
	}
	if namedErr != nil {
		return namedErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !*jsonOut {
		fmt.Fprintf(stdout, "ingested %d item(s)\n", count)
	}
	if aggregateErr != nil {
		return aggregateErr
	}
	return nil
}

// ingestRunReceipt reports what one ingest run did. Phase 2 (ISO-02) adds the
// per-source outcome breakdown here; additions are minor, removals need a bump.
type ingestRunReceipt struct {
	Source string `json:"source,omitempty"`
	All    bool   `json:"all"`
	Items  int    `json:"items"`
	sourceRunAggregate
}

var rebuildIngestIndexFn = rebuildIndex

// progressWriter routes a command's running commentary away from stdout when
// the caller asked for JSON, so a machine payload is never interleaved with
// progress prose.
func progressWriter(stdout, stderr io.Writer, jsonOut bool) io.Writer {
	if jsonOut {
		return stderr
	}
	return stdout
}

// connectReceipt is the machine form of a completed connect verb.
type connectReceipt struct {
	Source    string `json:"source"`
	Connected bool   `json:"connected"`
}

func cmdConnect(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	// github and imessage never defined --json; strip it here, route their setup
	// prose to stderr, and answer with a receipt. connect filesystem already owns
	// its own --json branch and its receipt, so it is left alone.
	if len(args) >= 1 && (args[0] == "github" || args[0] == "imessage") {
		source := args[0]
		jsonOut := false
		rest := make([]string, 0, len(args))
		for _, a := range args[1:] {
			if a == "--json" {
				jsonOut = true
				continue
			}
			rest = append(rest, a)
		}
		out := stdout
		if jsonOut {
			out = stderr
		}
		var err error
		if source == "github" {
			err = connectGitHub(ctx, rest, out)
		} else {
			err = connectIMessage(ctx, rest, out)
		}
		if err != nil {
			return err
		}
		if jsonOut {
			return emitReceipt(stdout, "mora.connect."+source, 1, connectReceipt{Source: source, Connected: true})
		}
		return nil
	}
	if len(args) >= 1 && args[0] == "filesystem" {
		return connectFilesystem(ctx, args[1:], stdout, stderr)
	}
	if len(args) < 1 || args[0] != "google" {
		return errors.New("usage: mora connect google [--since-days N] [--account <label>] | mora connect github [--repo owner/repo] | mora connect imessage [--since-days N] | mora connect filesystem <path> [--name <name>]")
	}
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sinceDays := fs.Int("since-days", 0, "gmail backfill window in days (default 90)")
	account := fs.String("account", "", `label for an ADDITIONAL Google account (e.g. "work"); empty = the default account`)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *account != "" && !isValidAccountLabel(*account) {
		return fmt.Errorf("invalid account label %q (use lowercase letters, digits, hyphens)", *account)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	oc, err := google.ResolveOAuthConfig(google.Scopes)
	if err != nil {
		return err
	}
	// Phase 1: authenticate (loopback consent).
	printGoogleAuthPreamble(stdout)
	tok, err := google.StartLoopbackAuth(ctx, oc, stdout)
	if err != nil {
		return err
	}
	// Phase 2: persist + validate. The token lands in the per-account file so a
	// second mailbox (personal vs business) never clobbers the first's refresh
	// token.
	if err := google.SaveToken(googleTokenPathFor(cfg, *account), tok); err != nil {
		return err
	}
	fetcher, err := google.NewLiveFetcher(ctx, oc, tok)
	if err != nil {
		return err
	}
	email, err := fetcher.AuthedEmail()
	if err != nil {
		return fmt.Errorf("validation call failed: %w", err)
	}
	okf(stdout, "Signed in as %s — verified read-only access to your Gmail and Calendar.", email)

	// Same-account guard: this mailbox may already be connected under another
	// label — proceeding would double-ingest every thread under distinct
	// @account StableIDs. Exit gracefully, and remove the just-written
	// duplicate token so a stray refresh token doesn't linger.
	if existing, found := googleAccountForEmail(loadSourcesOrEmpty(cfg), email); found && existing != *account {
		label := existing
		if label == "" {
			label = "the default account"
		} else {
			label = "account " + strconv.Quote(label)
		}
		_ = os.Remove(googleTokenPathFor(cfg, *account))
		fmt.Fprintf(stdout, "%s is already connected as %s — nothing to do.\n", email, label)
		fmt.Fprintln(stdout, "To refresh that account's data, run `mora sync google`.")
		return nil
	}

	// Phase 3: register default sources if absent, enable them, then backfill.
	// `connect google` is the deliberate enable+backfill convenience (D-06):
	// ensureGoogleSources creates gmail/calendar DISABLED (D-11), so connect must
	// flip the Enabled bit BEFORE the backfill loop, then reload so the loop sees
	// the enabled rows. The backfill loop itself stays UNGATED — it is the named,
	// consented convenience path, NOT a silent backfill (REG-03/Pitfall 3).
	if err := ensureGoogleSources(cfg, *account); err != nil {
		return err
	}
	gmailName, calName := googleSourceNames(*account)
	if err := setSourceEnabledByName(cfg, gmailName, true); err != nil {
		return err
	}
	if err := setSourceEnabledByName(cfg, calName, true); err != nil {
		return err
	}
	if err := setSourceEmailByAccount(cfg, *account, email); err != nil {
		return err
	}
	// Persist an explicit window override so this and future `sync google` runs
	// reuse it (0 keeps the 90-day default in windowForSource).
	if *sinceDays > 0 {
		if err := setSourceSinceDaysByName(cfg, gmailName, *sinceDays); err != nil {
			return err
		}
	}
	sources, _ := loadSources(cfg)
	total := 0
	now := time.Now()
	for _, s := range sources {
		if s.Type != "gmail" && s.Type != "calendar" {
			continue // ungated convenience path (D-06) — no IsEnabled() skip here
		}
		if s.Account != *account {
			continue // backfill only the account just connected — other mailboxes keep their own cadence
		}
		// Skip-if-fresh: a re-auth minutes after a clean backfill should not
		// re-pull the whole window (the full re-pull is the honest-snapshot
		// design; `mora sync google` remains the explicit force path).
		if sourceFreshlySynced(cfg, s, connectFreshWindow, now) {
			fmt.Fprintf(stdout, "  %s: synced recently — skipping re-pull (`mora sync google` to force)\n", s.Name)
			continue
		}
		n, e := ingestSource(cfg, s, stdout)
		total += n
		if e != nil {
			warnf(stdout, "%s sync incomplete (resumable): %v", s.Name, e)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return err
	}
	okf(stdout, "Enabled gmail + calendar and backfilled %d item(s).", total)
	renderSetupState(cfg, stdout)
	return nil
}
func cmdSync(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) >= 1 && genericutil.IsHelpFlag(args[0]) {
		fmt.Fprintln(stdout, "usage: mora sync <status|google|github|filesystem|imessage|applecalendar|git>")
		fmt.Fprintln(stdout, "  status    show per-source freshness (no fetch)")
		fmt.Fprintln(stdout, "  google    re-run the Gmail + Calendar backfill")
		fmt.Fprintln(stdout, "  github    re-run the read-only GitHub issue sync")
		fmt.Fprintln(stdout, "  filesystem re-index enabled filesystem sources")
		fmt.Fprintln(stdout, "  imessage  re-run the iMessage backfill")
		fmt.Fprintln(stdout, "  applecalendar re-run the local Apple Calendar backfill")
		fmt.Fprintln(stdout, "  git       back up the vault to a private git remote (off-device)")
		fmt.Fprintln(stdout, "            --init [--remote URL | --github [--name repo]] [-m msg]")
		return nil
	}
	if len(args) == 0 {
		return newCodedError(errCodeUsageMissingArgument, nil,
			"usage: mora sync <status|google|github|filesystem|imessage|applecalendar|git>")
	}
	// Strip the relay receipt token BEFORE any flag parsing. --mora-app-receipt
	// is a private argument of the signed-app relay protocol, not part of a
	// per-verb flag surface, so the --json flag sets below would reject it as an
	// unknown flag — which both hid the "only valid" guard and broke the relay
	// child (`mora sync imessage --mora-app-receipt <token>`).
	receiptToken, cleanedArgs, err := protectedSyncReceiptArg(args)
	if err != nil {
		return err
	}
	args = cleanedArgs
	if receiptToken != "" && (len(args) == 0 || !protectedSyncSource(args[0])) {
		return fmt.Errorf("%s is only valid for imessage or applecalendar sync", protectedSyncReceiptFlag)
	}
	if len(args) == 0 {
		return newCodedError(errCodeUsageMissingArgument, nil,
			"usage: mora sync <status|google|github|filesystem|imessage|applecalendar|git>")
	}
	var statusJSON bool
	if args[0] == "status" {
		fs := flag.NewFlagSet("sync status", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
		}
		if fs.NArg() != 0 {
			return newMoraError(errCodeUsageUnknownValue, "usage", nil, "unexpected argument %q", fs.Arg(0))
		}
		statusJSON = *jsonOut
	}
	// The per-source re-sync verbs share one flag surface. --json replaces the
	// "synced N item(s)" line with a receipt and moves walk progress to stderr,
	// so stdout carries exactly one JSON document. Routing below stays an
	// explicit per-source chain so a typo can never fall through to a networked
	// backfill.
	sourceJSON := false
	switch args[0] {
	case "filesystem", "imessage", "applecalendar", "github", "google":
		fs := flag.NewFlagSet("sync "+args[0], flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "emit the sync receipt as JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
		}
		if fs.NArg() != 0 {
			return newMoraError(errCodeUsageUnknownValue, "usage", nil, "unexpected argument %q", fs.Arg(0))
		}
		sourceJSON = *jsonOut
	}
	sourceProgress := progressWriter(stdout, stderr, sourceJSON)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// Route protected local-store reads through the signed application when this
	// invocation came from another host. The launched child carries a receipt token
	// and runs this same command directly, preventing relay recursion.
	if receiptToken == "" && len(args) > 0 && protectedSyncSource(args[0]) {
		if relayReceipt, rerr := relayProtectedSync(ctx, cfg, args[0]); rerr == nil {
			if sourceJSON {
				return emitSyncSourceResult(stdout, args[0], true, relayReceipt.Items, "")
			}
			fmt.Fprintf(stdout, "synced %s via Mora.app\n", args[0])
			return nil
		} else if rerr != errProtectedSyncDirect {
			// A child sync can fail after materializing some items. Match the direct
			// path's contract: publish the deterministic receipt before returning the
			// failure, while launch/protocol failures without a child receipt remain
			// plain command errors.
			if sourceJSON && relayReceipt.Source != "" {
				if emitErr := emitSyncSourceResult(stdout, args[0], true, relayReceipt.Items, ""); emitErr != nil {
					return emitErr
				}
			}
			return rerr
		}
	}
	// `mora sync git` — one-way, push-only, fail-loud off-device backup to a
	// private git remote (opt-in; the vault otherwise never leaves the device).
	if args[0] == "git" {
		gerr := syncGit(ctx, cfg, args[1:], stdout, realExec)
		// git-daily producer chokepoint (HEALTH-11).
		stampChokepoint(cfg, stdout, args, "git-daily", producerClock(), &gerr)
		return gerr
	}
	if args[0] == "status" {
		dir := filepath.Join(cfg.StateDir, "sync")
		entries, _ := os.ReadDir(dir)
		if statusJSON {
			return emitReceipt(stdout, "mora.sync.status", 1, syncStatusReceipt{Sources: syncStatusReceiptSources(entries, dir, time.Now())})
		}
		if len(entries) == 0 {
			fmt.Fprintln(stdout, "no sources synced yet")
			return nil
		}
		now := time.Now()
		// C3 ▸R2: this used to read st.LastSynced against a flat 48h threshold,
		// ignoring ErrorCount entirely — a DIVERGENT, parallel staleness verdict
		// that could disagree with the health banner (a fresh sync-status line
		// sitting above an unhealthy index, or a source that errored every hour
		// still reading merely "old"). Route through the SAME success-only
		// watermark + per-type threshold + failed/never precedence as
		// sourceHealthFor (health.go), so the two can never again disagree.
		if banner := healthBanner(cfg, now); banner != "" {
			fmt.Fprintln(stdout, banner)
		}
		sty := newStyler(stdout, false)
		for _, e := range entries {
			st, err := memory.LoadStatus(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			state := syncStatusFileState(st, syncStatusFileThreshold(e.Name()), now)
			stale := ""
			if state != healthFresh {
				stale = " " + sty.bad("("+strings.ToUpper(state)+")")
			}
			errs := fmt.Sprintf("%d errors", st.ErrorCount)
			if st.ErrorCount > 0 {
				errs = sty.bad(errs)
			}
			fmt.Fprintf(stdout, "%s: %d items, %s, %s%s\n",
				sty.accent(st.Source), st.ItemCount, errs, sty.dim("last_synced "+st.LastSynced), stale)
		}
		return nil
	}
	// `mora sync filesystem` — re-walk only the enabled filesystem sources.
	// Routing is explicit so a typo can never fall through to a networked Google
	// backfill, and the helper performs one final index rebuild after the walks.
	if args[0] == "filesystem" {
		total, err := backfillEnabledFilesystem(ctx, cfg, sourceProgress)
		if rerr := emitSyncSourceResult(stdout, "filesystem", sourceJSON, total, "synced %d item(s)\n"); rerr != nil {
			return rerr
		}
		return err
	}
	// `mora sync imessage` — re-run the gated iMessage backfill (shared seam).
	if args[0] == "imessage" {
		total, syncErr := backfillEnabledIMessage(ctx, cfg, sourceProgress)
		rerr := emitSyncSourceResult(stdout, "imessage", sourceJSON, total, "synced %d item(s)\n")
		// The relay receipt is written even when the stdout emit failed: the
		// launching host is waiting on it, and a broken pipe must not strand it.
		if receiptToken != "" {
			r := protectedSyncReceipt{Token: receiptToken, Source: args[0], Items: total, CompletedAt: protectedSyncNow().UTC().Format(time.RFC3339)}
			if syncErr != nil {
				r.Error = syncErr.Error()
			}
			if err := writeProtectedSyncReceipt(cfg, r); err != nil {
				return err
			}
		}
		if rerr != nil {
			return rerr
		}
		return syncErr
	}
	// `mora sync applecalendar` — targeted retry for the local Calendar store.
	// Keep this separate from Google Calendar: the provider, FDA gate, status
	// receipt, and recovery action are all independent (#266).
	if args[0] == "applecalendar" {
		total, syncErr := backfillEnabledAppleCalendar(ctx, cfg, sourceProgress)
		rerr := emitSyncSourceResult(stdout, "applecalendar", sourceJSON, total, "synced %d item(s)\n")
		// Same rule as imessage above: the relay receipt outlives a stdout failure.
		if receiptToken != "" {
			r := protectedSyncReceipt{Token: receiptToken, Source: args[0], Items: total, CompletedAt: protectedSyncNow().UTC().Format(time.RFC3339)}
			if syncErr != nil {
				r.Error = syncErr.Error()
			}
			if err := writeProtectedSyncReceipt(cfg, r); err != nil {
				return err
			}
		}
		if rerr != nil {
			return rerr
		}
		return syncErr
	}
	if args[0] == "github" {
		total, err := backfillEnabledGitHub(ctx, cfg, sourceProgress)
		if rerr := emitSyncSourceResult(stdout, "github", sourceJSON, total, "synced %d issue(s)\n"); rerr != nil {
			return rerr
		}
		return err
	}
	if args[0] == "google" {
		total, err := backfillEnabledGoogle(ctx, cfg, sourceProgress)
		if rerr := emitSyncSourceResult(stdout, "google", sourceJSON, total, "synced %d item(s)\n"); rerr != nil {
			return rerr
		}
		return err
	}
	return fmt.Errorf("unknown sync source %q (usage: mora sync <status|google|github|filesystem|imessage|applecalendar|git>)", args[0])
}

// syncSourceReceipt is the per-source re-sync outcome. Phase 2 (ISO-02) adds
// the typed failure fields here; additions are minor, removals need a bump.
type syncSourceReceipt struct {
	Source string `json:"source"`
	Items  int    `json:"items"`
}

// emitSyncSourceResult writes the outcome of one per-source re-sync: a receipt
// under --json, the shipped human line otherwise. It runs on the error path too,
// because a partial sync still reports what it managed to pull.
func emitSyncSourceResult(stdout io.Writer, source string, jsonOut bool, total int, humanFormat string) error {
	if jsonOut {
		return emitReceipt(stdout, "mora.sync."+source, 1, syncSourceReceipt{Source: source, Items: total})
	}
	_, err := fmt.Fprintf(stdout, humanFormat, total)
	return err
}

// syncStatusReceipt is additive: Phase 2 (ISO-02) and Phase 7 (OBS-01) may add
// fields in a minor release; removal or retyping requires a schema_version bump.
type syncStatusReceipt struct {
	Sources []syncStatusReceiptSource `json:"sources"`
}

type syncStatusReceiptSource struct {
	Source        string `json:"source"`
	State         string `json:"state"`
	LastSuccessAt string `json:"last_success_at"`
	LastAttemptAt string `json:"last_attempt_at"`
	ItemCount     int    `json:"item_count"`
	ErrorCount    int    `json:"error_count"`
	LastError     string `json:"last_error,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
}

func syncStatusReceiptSources(entries []os.DirEntry, dir string, now time.Time) []syncStatusReceiptSource {
	sources := make([]syncStatusReceiptSource, 0, len(entries))
	for _, entry := range entries {
		st, err := memory.LoadStatus(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		sources = append(sources, syncStatusReceiptSource{
			Source:        st.Source,
			State:         syncStatusFileState(st, syncStatusFileThreshold(entry.Name()), now),
			LastSuccessAt: st.LastSuccessAt,
			LastAttemptAt: st.LastAttemptAt,
			ItemCount:     st.ItemCount,
			ErrorCount:    st.ErrorCount,
			LastError:     st.LastError,
			// The typed companion CON-07 needs. A record persisted before the
			// taxonomy shipped has no code on disk and is NOT rewritten; it reads
			// as connector.unclassified here instead.
			ErrorCode: syncErrorCodeForState(
				syncStatusFileState(st, syncStatusFileThreshold(entry.Name()), now),
				st.ErrorCode, st.LastError),
		})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Source < sources[j].Source })
	return sources
}

// syncStatusFileThreshold infers the freshness threshold for a raw sync/
// status file from its filename prefix (mirrors syncStatusPathFor's own
// naming: "google-"/"applecal-"/"imessage-"/"filesystem-"). `mora sync status`
// walks the raw directory rather than sources.json (a file can outlive its
// source being disabled/removed), so it cannot resolve a full Source struct —
// but the filename prefix is enough to pick the SAME threshold family
// sourceHealthThreshold does, which is all classification needs.

// syncStatusFileState classifies one raw sync/ status file with the EXACT same
// worst-first precedence as sourceHealthFor (never > failed > stale > fresh),
// so `mora sync status`'s per-line verdict can never again disagree with the
// health banner (C3 ▸R2: the old flat-48h/LastSynced check ignored ErrorCount
// and used a single threshold for every connector type).

// cmdReingest re-fetches enabled sources and rewrites memories with the latest
// structured metadata (the Meta-in-content-hash change means a normal sync already
// rewrites within the window; --full extends the lookback to all-time so the
// rewrite reaches memories older than the default window), then rebuilds the graph.
// reingestReceipt reports what one reingest run did. Phase 2 (ISO-02) may add
// the per-source breakdown; additions are minor, removals need a bump.
type reingestReceipt struct {
	Full          bool `json:"full"`
	Items         int  `json:"items"`
	FailedSources int  `json:"failed_sources"`
	sourceRunAggregate
}

func cmdReingest(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	full := false
	jsonOut := false
	retryFailed := false
	for _, a := range args {
		switch a {
		case "--full":
			full = true
		case "--json":
			jsonOut = true
		case "--failed":
			retryFailed = true
		case "-h", "--help":
			fmt.Fprintln(stdout, "usage: mora reingest [--full]")
			fmt.Fprintln(stdout, "  re-fetch enabled sources, rewrite memories with the latest")
			fmt.Fprintln(stdout, "  structured identity metadata, and rebuild the entity graph.")
			fmt.Fprintln(stdout, "  --full  extend the lookback to all-time (backfill older memories)")
			fmt.Fprintln(stdout, "  --failed  retry only sources whose last failure is retryable")
			return nil
		default:
			return fmt.Errorf("unknown flag %q (usage: mora reingest [--full])", a)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// Per-source progress is a diagnostic, not the result: under --json it moves
	// to stderr so stdout carries exactly one document (CON-06).
	progress := progressWriter(stdout, stderr, jsonOut)
	progress = &synchronizedWriter{w: progress}
	sources, loadErr := loadSources(cfg)
	if loadErr != nil {
		return loadErr
	}
	var plans []sourceRunPlan
	for _, s := range sources {
		if !s.IsEnabled() {
			continue
		}
		if retryFailed {
			status, statusErr := memory.LoadStatus(syncStatusPathFor(cfg, s))
			if statusErr != nil {
				return statusErr
			}
			code := syncErrorCodeOrUnclassified(status.ErrorCode, status.LastError)
			if status.LastError == "" || !retryableForErrorCode(code) {
				continue
			}
		}
		if full {
			switch s.Type {
			case "gmail":
				s.SinceDays = reingestFullDays // all-time lookback (copy; not persisted)
			case "imessage":
				s.SinceDays = -1 // all-time (windowForIMessage)
			}
		}
		plans = append(plans, sourceRunPlan{Key: healthInstanceKeyForSource(s), Source: s})
	}
	outcomes := runSourcePlan(ctx, plans, sourceRunOptions{Run: func(runCtx context.Context, plan sourceRunPlan) sourceRunOutcome {
		result, runErr := ingestSourceFn(runCtx, cfg, plan.Source, progress)
		return sourceRunOutcome{Key: plan.Key, Items: result.Materialized, Examined: result.Examined, Materialized: result.Materialized, Failed: result.Failed, Missing: result.Missing, Stages: result.Stages, Err: runErr}
	}})
	total := sourceOutcomesMaterialized(outcomes)
	for _, outcome := range outcomes {
		if outcome.Err != nil && !outcome.Cancelled {
			warnf(progress, "%s reingest incomplete (resumable): %v", outcome.Key, outcome.Err)
		}
	}
	_, rebuildErr := rebuildIndex(ctx, cfg)
	aggregate, aggregateErr := aggregateSourceRuns(plans, outcomes, nil, rebuildErr)
	if ctx.Err() != nil {
		aggregateErr = ctx.Err()
	}
	if jsonOut {
		// The receipt ships BEFORE the aggregate failure return, so a partial
		// reingest never hands a caller exit 1 with an empty stdout — the same
		// rule `ingest run` follows.
		if eerr := emitReceipt(stdout, "mora.reingest", 1, reingestReceipt{
			Full: full, Items: total, FailedSources: aggregate.FailedSources, sourceRunAggregate: aggregate,
		}); eerr != nil {
			return eerr
		}
		return aggregateErr
	}
	suffix := ""
	if full {
		suffix = " (full lookback)"
	}
	fmt.Fprintf(stdout, "reingested %d item(s)%s\n", total, suffix)
	return aggregateErr
}

// ingestSource is the single dispatch chokepoint every caller (backfillEnabledGoogle,
// cmdIngest, cmdReingest, applySetupSelection, ...) routes through, so it is also
// the one place that closes the pre-Ingest stamping gap (▸CX, health.go): an
// error here — including one from BEFORE the type-specific path ever reaches
// memory.Ingest (OAuth/token/fetcher/DB-open failures) — always stamps
// LastAttemptAt/LastError so doctor can show WHY a source is failing, not only
// that it has gone quiet. attemptStart is captured BEFORE dispatch runs so
// stampSyncAttemptFailure can tell "the inner path stamped this attempt" from
// "a previous attempt failed" by timing, not by comparing error text (a
// repeated identical failure must still advance LastAttemptAt every time).
// testHookIngestActivityStarted fires after the durable activity+journal marks
// exist and before provider dispatch can publish. Tests use it as an ordering and
// concurrency failpoint; production leaves it nil.
var testHookIngestActivityStarted func(Config, Source, operationHandle)

type sourceIngestResult struct {
	Examined     int
	Materialized int
	Failed       int
	Missing      int
	Stages       memory.IngestStages
}

func sourceIngestResultFromMemory(result memory.IngestResult) sourceIngestResult {
	return sourceIngestResult{
		Examined: result.Examined, Materialized: result.Materialized,
		Failed: result.Failed, Missing: result.Missing,
		Stages: result.Stages,
	}
}

func ingestSource(cfg Config, s Source, out io.Writer) (int, error) {
	result, err := ingestSourceDetailed(context.Background(), cfg, s, out)
	return result.Materialized, err
}

func ingestSourceDetailed(ctx context.Context, cfg Config, s Source, out io.Writer) (result sourceIngestResult, err error) {
	// attemptStart is captured before ANY step that can fail, so every failure at
	// this chokepoint — activity bookkeeping included — stamps the same attempt.
	attemptStart := time.Now()
	// The activity receipt and matching journal header are both durable before
	// dispatch can publish a vault file. Even a zero-item run gets a header, so
	// its success can be closed only by the covering committed rebuild.
	h, err := beginOperation(cfg, operationKindIngest, "starting", operationClock())
	if err != nil {
		// The connector failure boundary (CON-07). Every source failure passes
		// through here exactly once, so this is where an untyped one acquires a
		// published code. The message is preserved byte-for-byte and the cause
		// stays reachable through errors.Is/errors.As — this adds a label; it does
		// not change what failed or what the user reads.
		err = classifyConnectorError(fmt.Errorf("starting ingest activity: %w", err))
		stampSyncAttemptFailure(cfg, s, err, attemptStart, out)
		return result, err
	}
	cfg.SetOperationRunID(h.RunID)
	finishFailed := func(code string, cause error) error {
		errorCount := result.Failed
		if errorCount == 0 {
			errorCount = 1
		}
		finishErr := finishOperation(cfg, h, operationFailed, "failed", operationCounts{
			Items: result.Materialized, Errors: errorCount, Examined: result.Examined,
			Materialized: result.Materialized, Missing: result.Missing,
		}, code, operationClock())
		return errors.Join(cause, finishErr)
	}
	sourceKey := ingestOperationSourceKey(s)
	if err := ingestpkg.EnsureJournalHeader(cfg, sourceKey, ingestPublishSeams()); err != nil {
		return result, finishFailed("journal_start_failed", err)
	}
	// Release only this source's lease. Concurrent in-process ingests share a PID;
	// releasing every lease here would abandon the still-running sibling.
	defer ingestpkg.ReleaseLeaseOwnedHere(cfg, sourceKey, ingestLeaseSeams())

	progress := startOperationProgress(cfg, h, "ingesting")
	if err := progress.Update("ingesting", operationCounts{}); err != nil {
		_ = progress.Stop()
		return result, finishFailed("heartbeat_failed", err)
	}
	if testHookIngestActivityStarted != nil {
		testHookIngestActivityStarted(cfg, s, h)
	}
	result, dispatchErr := ingestSourceDispatchFn(ctx, cfg, s, out)
	if dispatchErr == nil {
		dispatchErr = progress.Update("awaiting_rebuild", operationCounts{
			Items: result.Materialized, Errors: result.Failed, Examined: result.Examined,
			Materialized: result.Materialized, Missing: result.Missing,
		})
	}
	if dispatchErr != nil {
		// CON-07: the untyped failure acquires its published connector code here,
		// before the operation bookkeeping joins in, so the typed cause stays
		// reachable through errors.As for the sync-status stamp below.
		dispatchErr = classifyConnectorError(dispatchErr)
		progressErr := progress.Stop()
		err = errors.Join(dispatchErr, progressErr)
		err = finishFailed("ingest_failed", err)
		stampSyncAttemptFailure(cfg, s, err, attemptStart, out)
		return result, err
	}
	// Success deliberately remains running/awaiting_rebuild, and its bounded
	// heartbeat stays live while sibling sources finish. The committed rebuild
	// that retires this run's journal stops it and owns the terminal transition.
	return result, nil
}

// classifyConnectorError attaches a CON-07 discrimination to a connector
// failure. A failure already typed at its own boundary keeps that code — the
// site that RAISED it knows more than this function can infer.
//
// Every rule below is STRUCTURAL: a decoder type, a sentinel, an interface. None
// matches on error prose. That restraint is deliberate. The live Doctor
// misdiagnosis Phase 3 (DOC-03) exists to kill was produced by exactly that
// habit — inferring a permission state from an error string — and a typed
// taxonomy that re-encodes a guess is worse than none, because it makes the
// guess look like a fact. An unrecognized failure is honestly unclassified.
func classifyConnectorError(err error) error {
	if err == nil {
		return nil
	}
	var typed moraError
	if errors.As(err, &typed) && classForErrorCode(typed.Code) == errClassConnector {
		return err
	}
	return newCodedError(connectorCodeForCause(err), err, "%v", err)
}

// connectorErrorCode returns the published code a connector failure resolves to,
// applying the SAME classification the ingestSource boundary applies. It exists
// so a failure raised inside memory.Ingest can be typed at the moment it is
// persisted: internal/memory writes prose and never claims a code, because the
// taxonomy lives here and that package must not import this one.
func connectorErrorCode(err error) string {
	return connectorErrorCodeFor(classifyConnectorError(err))
}

// connectorCodeForCause is classifyConnectorError's structural rule table, split
// out so a test can drive every branch with a synthetic cause.
func connectorCodeForCause(err error) string {
	// A payload the connector returned but Mora could not decode.
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errCodeConnectorMalformed
	}
	// The connector's process, binary, endpoint, or backing file is not there.
	var execErr *exec.Error
	var exitErr *exec.ExitError
	var netErr net.Error
	if errors.Is(err, fs.ErrNotExist) || errors.As(err, &execErr) || errors.As(err, &exitErr) ||
		errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) {
		return errCodeConnectorUnavailable
	}
	return errCodeConnectorUnclassified
}

var ingestSourceDispatchFn = ingestSourceDispatchDetailed

func ingestSourceDispatchDetailed(ctx context.Context, cfg Config, s Source, out io.Writer) (sourceIngestResult, error) {
	switch s.Type {
	case "filesystem":
		return ingestFilesystemDetailed(ctx, cfg, s, out)
	case "gmail":
		return ingestGoogleDetailed(ctx, cfg, s, google.KindGmailThread, out)
	case "calendar":
		return ingestGoogleDetailed(ctx, cfg, s, google.KindCalEvent, out)
	case "imessage":
		return ingestIMessageDetailed(ctx, cfg, s, out)
	case "applecalendar":
		return ingestAppleCalDetailed(ctx, cfg, s, out)
	case "github":
		return ingestGitHubDetailed(ctx, cfg, s, out)
	case "gdrive":
		return sourceIngestResult{}, nil // deferred to week 2
	default:
		return sourceIngestResult{}, fmt.Errorf("unknown source type %q", s.Type)
	}
}

// testHookMappedWriteCritical, when non-nil (tests only), fires inside
// writeMappedMemory AFTER the fresh suppression check and the content-hash skip,
// BEFORE atomicWrite — i.e. while the governance lease is held. It is the
// connector-path twin of testHookInWriteCritical: it proves the lease SPANS the
// check-and-write, closing the connector TOCTOU (#113 gap #1). Nil in production.
var testHookMappedWriteCritical func()

func writeMappedMemory(cfg Config, mm memory.MappedMemory) error {
	// Governance chokepoint (#52/#53): consult the vault-resident ledger before
	// persisting ANY connector memory. A suppressed stable-atom (forgotten chat,
	// forgotten 1:1 person, pruned item) is silently skipped so the hourly,
	// agent-less sync can never resurrect it. This is the single place re-ingest
	// is blocked; it covers all connector call sites without per-site edits.
	//
	// The suppression check AND the atomicWrite run under a SINGLE held governance
	// lease (governanceWriteLease) — the same lease `mora forget` takes to append
	// its suppression. That makes check→write atomic w.r.t. that append and closes
	// the connector-path TOCTOU (#113 gap #1): a fresh once-per-item load alone
	// closed the STALE-SNAPSHOT variant but NOT the check-to-write window — a
	// forget committing between an unlocked check and the atomicWrite was still
	// missed and resurrected the atom. Holding the lease across both (mirroring the
	// filesystem writeUnlessForgotten fix) is the durable close. A corrupt ledger
	// returns an error (fail-closed) rather than resurrecting.
	g, release, err := governanceWriteLease(cfg)
	if err != nil {
		return err
	}
	defer release()
	if sup, _ := g.suppresses(mm); sup {
		return nil
	}
	// Derive the canonical record/path and skip decision inside the held lease so
	// the whole check→skip→write remains one critical section.
	out := ingestpkg.MappedTargetPath(cfg, mm)
	var existing *Memory
	if parsed, err := parseMemory(out); err == nil {
		existing = &parsed
	}
	m, skip := ingestpkg.PrepareMapped(mm, existing)
	if skip {
		return nil
	}
	body, err := renderMemory(m)
	if err != nil {
		return err
	}
	if testHookMappedWriteCritical != nil {
		testHookMappedWriteCritical() // test seam: assert the lease is held ACROSS the write (#113).
	}
	// A3 rule d: mark-before-visible for connector writes. The durable journal
	// header must land BEFORE the file is published, so a SIGKILL after the publish
	// still reads the index dirty and the next rebuild recovers the memory. Only an
	// ACTUAL publish is journaled — the content-hash-unchanged / suppressed early
	// returns above wrote no new file and reach neither line.
	sourceKey := ingestpkg.SourceKey(mm.Provider, mm.Account)
	if jerr := ingestpkg.EnsureJournalHeader(cfg, sourceKey, ingestPublishSeams()); jerr != nil {
		return jerr
	}
	if err := atomicio.Write(out, body, 0o644); err != nil {
		return err
	}
	if testHookPostConnectorPublish != nil {
		// Test seam (matrix 34a): a SIGKILL in the publish->journal-line window. The
		// file is on disk; the best-effort path line has NOT appended. The durable
		// header (written BEFORE the publish) is what still keeps the index dirty here,
		// so removing it is a false-clean. Nil in production.
		testHookPostConnectorPublish()
	}
	ingestpkg.RecordPublishedPath(cfg, sourceKey, out, ingestPublishSeams())
	return nil
}

// testHookPostConnectorPublish fires inside writeMappedMemory AFTER the vault publish
// but BEFORE the best-effort journal path line, so a test can crash in exactly the
// window the durable header protects (matrix 34a). Nil in production.
var testHookPostConnectorPublish func()

func ingestGoogle(cfg Config, s Source, kind google.ItemKind, out io.Writer) (int, error) {
	result, err := ingestGoogleDetailed(context.Background(), cfg, s, kind, out)
	return result.Materialized, err
}

func ingestGoogleDetailed(ctx context.Context, cfg Config, s Source, kind google.ItemKind, out io.Writer) (sourceIngestResult, error) {
	oc, err := google.ResolveOAuthConfig(google.Scopes)
	if err != nil {
		return sourceIngestResult{}, err
	}
	tok, err := google.LoadToken(googleTokenPathFor(cfg, s.Account))
	if err != nil {
		connectCmd := "mora connect google"
		if s.Account != "" {
			connectCmd += " --account " + s.Account
		}
		// connector.unauthorized is honest here and nowhere else in this file: the
		// credential itself could not be loaded, which Mora DIRECTLY observed. It is
		// deliberately not derived from isGoogleAuthError's prose markers ("token",
		// "expired", "oauth"), which are a good enough hint for a human sentence but
		// would be an inference if they typed a published code.
		return sourceIngestResult{}, newCodedError(errCodeConnectorUnauthorized, err,
			"not connected to google (run `%s`): %v", connectCmd, err)
	}
	fetcher, err := google.NewLiveFetcher(ctx, oc, tok)
	if err != nil {
		return sourceIngestResult{}, err
	}
	statusPath := googleStatusPath(cfg, s.Name)
	st, _ := memory.LoadStatus(statusPath)
	st.Source = s.Name
	win := windowForSource(s, kind)

	// Progress output: a thread-level backfill makes one API call per item and
	// prints nothing, so a large pull looks frozen for minutes (#ux). Emit a
	// start line and a periodic running count by wrapping the Write callback.
	noun := "items"
	switch kind {
	case google.KindGmailThread:
		noun = "threads"
	case google.KindCalEvent:
		noun = "events"
	}
	prog := newProgress(out, s.Name, noun)
	write := func(mm memory.MappedMemory) error {
		// Account-scoped provenance (the sourceInstanceKey multi-account seam):
		// a labeled account stamps Account so the instance key composes
		// "gmail:work" — separate watermark, digest section, and three-state per
		// mailbox instead of collapsing into the default account's; and tags
		// StableID ("…@work") because a meeting shared across both mailboxes
		// carries the SAME iCal UID — untagged, the second account's copy would
		// collide with (and clobber) the first's.
		if s.Account != "" {
			mm.Account = s.Account
			mm.StableID = mm.StableID + "@" + s.Account
		}
		if err := writeMappedMemory(cfg, mm); err != nil {
			return err
		}
		prog.tick()
		return nil
	}

	res, ingErr := memory.Ingest(memory.IngestParams{
		Context: ctx, Fetcher: fetcher, Kind: kind, Window: win, Scope: s.Scope, BodyBudget: 16 * 1024,
		Status: st, Write: write, Checkpoint: func(status *memory.SyncStatus) error { return memory.SaveStatus(statusPath, status) },
	})
	prog.done()
	return sourceIngestResultFromMemory(res), persistSyncStatus(out, statusPath, res.Status, ingErr)
}

// persistSyncStatus writes a sync path's final SyncStatus to disk — the single
// boundary every sync path (google, GitHub, imessage, applecal, filesystem) routes
// through. A failed save is warned AND folded into the returned error instead
// of swallowed: the status file drives the digest's three-state health, so
// losing it silently turns a real outcome into permanent "unavailable"/stale
// readings. ingErr (the sync's own error) stays primary; the save error is
// returned only when the sync itself succeeded.

// googleTokenPathFor maps an account label to its token file. The unlabeled
// default keeps the legacy tokens/google.json (existing installs untouched);
// a labeled account (a second mailbox, e.g. personal vs business) gets its own
// tokens/google-<label>.json so two Google identities never clobber each
// other's refresh tokens.
func googleTokenPath(cfg Config) string { return googleTokenPathFor(cfg, "") }

// imessageStatusPath mirrors googleStatusPath so `mora sync status` reads the
// honest-snapshot freshness for iMessage generically (no special-casing).

// chatDBPath is the default local iMessage database location. macOS-only; the
// caller gates on runtime.GOOS before reading it.
func chatDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Messages", "chat.db")
}

// addressBookRoot is the AddressBook Sources/ root used for handle→name resolution.
func addressBookRoot() string {
	home, _ := os.UserHomeDir()
	return imessage.DefaultAddressBookRoot(home)
}

// windowForIMessage builds the lookback window: a lean 90-day default (D-06),
// overridable per source via SinceDays (0 ⇒ 90; a negative SinceDays means all-time,
// matching the Gmail since-days ergonomic). Zero Since = all-time at the SQL bound.
const defaultIMessageLookbackDays = ingestpkg.DefaultIMessageLookbackDays

// iMessageLookbackDays reports the configured effective lookback. A negative
// override means all available history; zero retains the documented default.

// iMessageFetcher is the injectable open+close seam for chat.db (Packet G2 /
// HEALTH-08). Production uses imessage.NewLiveFetcher; tests inject a denial or
// a zero-row Fetcher so FDA loss is drivable on Linux and Windows CI (macOS is
// not in CI, and the real GOOS gate would otherwise make the denial test a
// permanent false green).
type iMessageFetcher interface {
	memory.Fetcher
	Close() error
}

// newIMessageFetcher opens chat.db for ingestIMessage. Package var so
// TestFDALossNeverStampsSuccess can inject an open denial without chmod.
var newIMessageFetcher = func(path string, deny imessage.DenyList) (iMessageFetcher, error) {
	return imessage.NewLiveFetcher(path, deny)
}

// ingestIMessage reads the local chat.db read-only and writes one memory per
// conversation (IMSG-01/03). It is macOS-gated (a non-darwin host prints an honest
// note and returns 0, never a false error), resolves contact names via the
// AddressBook, honors the source's deny-list (IMSG-06), and surfaces resumable
// errors. Rendering/truncation is the connector's inverted-truncation mapper, routed
// through the shared resumable Ingest loop via the Map hook — the writeMappedMemory
// boundary is reused, never reimplemented.
func ingestIMessage(cfg Config, s Source, out io.Writer) (int, error) {
	result, err := ingestIMessageDetailed(context.Background(), cfg, s, out)
	return result.Materialized, err
}

func ingestIMessageDetailed(ctx context.Context, cfg Config, s Source, out io.Writer) (sourceIngestResult, error) {
	if runtimeGOOS() != "darwin" {
		if out != nil {
			fmt.Fprintf(out, "note: iMessage ingest only runs on macOS; this machine is %s.\n", runtimeGOOS())
		}
		return sourceIngestResult{}, nil
	}

	path := chatDBPath()
	deny := imessage.DenyList{Contacts: s.DenyContacts, Conversations: s.DenyConversations}
	fetcher, err := newIMessageFetcher(path, deny)
	if err != nil {
		// A present-but-unreadable chat.db is the FDA-denied case — point the user at
		// the doctor guidance rather than dumping a raw sqlite error.
		//
		// PHASE 3 HANDOFF (DOC-03). The prose above INFERS Full Disk Access from a
		// failed open; Mora never observed a permission refusal. This plan does not
		// remove that claim — it supplies the typed alternative so DOC-03 has
		// something to switch to. The code is connector.unavailable, NOT
		// connector.unauthorized, precisely because the permission state is
		// unverified: a machine reading this receipt learns "could not open", which
		// is true, rather than "denied", which is a guess. When DOC-03 lands a real
		// probe, an observed refusal becomes connector.unauthorized here and the
		// English can finally say what it means.
		return sourceIngestResult{}, newCodedError(errCodeConnectorUnavailable, err,
			"cannot read your Messages database (Full Disk Access not granted?) — run `mora doctor`: %v", err)
	}
	defer fetcher.Close()

	// Handle→name resolution; an unreadable AddressBook degrades to raw handles (D-09).
	resolver, _ := imessage.NewResolver(addressBookRoot())

	statusPath := imessageStatusPath(cfg, s.Name)
	st, _ := memory.LoadStatus(statusPath)
	st.Source = s.Name
	win := windowForIMessage(s)

	if out != nil {
		days := iMessageLookbackDays(s)
		if days < 0 {
			fmt.Fprintf(out, "  %s: lookback set to all available history (since-days < 0)\n", s.Name)
		} else {
			fmt.Fprintf(out, "  %s: ingesting the last %d days; use --since-days to change this window.\n", s.Name, days)
		}
	}
	prog := newProgress(out, s.Name, "conversations")
	write := func(mm memory.MappedMemory) error {
		if err := writeMappedMemory(cfg, mm); err != nil {
			return err
		}
		if _, err := writeAttachmentMemories(cfg, mm); err != nil {
			return err
		}
		prog.tick()
		return nil
	}

	res, ingErr := memory.Ingest(memory.IngestParams{
		Context: ctx, Fetcher: fetcher, Kind: imessage.KindIMessageChat, Window: win, Scope: s.Scope,
		BodyBudget: 16 * 1024, Status: st, Write: write,
		Checkpoint: func(status *memory.SyncStatus) error { return memory.SaveStatus(statusPath, status) },
		Map:        imessage.MapConversationFn(resolver),
	})
	prog.done()
	ingErr = persistSyncStatus(out, statusPath, res.Status, ingErr)
	if ingErr != nil {
		if out != nil {
			warnf(out, "imessage sync incomplete: %v", ingErr)
		}
		return sourceIngestResultFromMemory(res), ingErr
	}
	return sourceIngestResultFromMemory(res), nil
}

// appleCalStatusPath mirrors imessageStatusPath for the Apple Calendar store.

// appleCalDBPath probes the modern group-container store first, then the
// legacy ~/Library/Calendars location; returns the modern default when neither
// exists yet (the open error then names the real path the user must grant).
func appleCalDBPath() string {
	home, _ := os.UserHomeDir()
	modern := applecal.DefaultDBPath(home)
	if _, err := os.Stat(modern); err == nil {
		return modern
	}
	if legacy := applecal.LegacyDBPath(home); genericutil.FileExists(legacy) {
		return legacy
	}
	return modern
}

// windowForAppleCal builds the event window: SinceDays back (default 90;
// negative = all-time past) and a FIXED 180-day forward bound — Apple Calendar
// stores subscribed-holiday events YEARS out, and an unbounded Until would
// flood the vault (and the digest's upcoming-events section) with them.

// ingestAppleCal reads the local Apple Calendar store read-only and writes one
// memory per event. macOS-gated like iMessage (same Full Disk Access story —
// chat.db's TCC lesson applies verbatim: launchd grants are per-binary), and
// routed through the shared resumable Ingest loop + writeMappedMemory boundary.
func ingestAppleCal(cfg Config, s Source, out io.Writer) (int, error) {
	result, err := ingestAppleCalDetailed(context.Background(), cfg, s, out)
	return result.Materialized, err
}

func ingestAppleCalDetailed(ctx context.Context, cfg Config, s Source, out io.Writer) (sourceIngestResult, error) {
	if runtime.GOOS != "darwin" {
		if out != nil {
			fmt.Fprintf(out, "note: Apple Calendar ingest only runs on macOS; this machine is %s.\n", runtime.GOOS)
		}
		return sourceIngestResult{}, nil
	}
	fetcher, err := applecal.NewLiveFetcher(appleCalDBPath())
	if err != nil {
		// Same PHASE 3 HANDOFF (DOC-03) as ingestIMessage: the Full Disk Access
		// sentence is an inference this plan preserves verbatim, and the typed code
		// beside it reports only what Mora actually observed — the open failed.
		return sourceIngestResult{}, newCodedError(errCodeConnectorUnavailable, err,
			"cannot read your Calendar database (Full Disk Access not granted?) — run `mora doctor`: %v", err)
	}
	defer fetcher.Close()

	statusPath := appleCalStatusPath(cfg, s.Name)
	st, _ := memory.LoadStatus(statusPath)
	st.Source = s.Name
	prog := newProgress(out, s.Name, "events")
	write := func(mm memory.MappedMemory) error {
		if err := writeMappedMemory(cfg, mm); err != nil {
			return err
		}
		prog.tick()
		return nil
	}
	res, ingErr := memory.Ingest(memory.IngestParams{
		Context: ctx, Fetcher: fetcher, Kind: applecal.KindAppleCalEvent, Window: windowForAppleCal(s, time.Now()),
		Scope: s.Scope, BodyBudget: 16 * 1024, Status: st, Write: write,
		Checkpoint: func(status *memory.SyncStatus) error { return memory.SaveStatus(statusPath, status) },
	})
	prog.done()
	ingErr = persistSyncStatus(out, statusPath, res.Status, ingErr)
	if ingErr != nil {
		if out != nil {
			warnf(out, "applecalendar sync incomplete: %v", ingErr)
		}
		return sourceIngestResultFromMemory(res), ingErr
	}
	return sourceIngestResultFromMemory(res), nil
}

// ingestGitHub snapshots source records immutably before reconciling their
// stable searchable projections. GitHub remains evidence: this path never calls
// the task ledger, selects work, or launches an agent.
func ingestGitHubDetailed(ctx context.Context, cfg Config, s Source, out io.Writer) (sourceIngestResult, error) {
	repos := s.Repositories
	if len(repos) == 0 {
		repos = githubissues.DefaultRepositories
	}
	fetcher, err := githubissues.NewLiveFetcher(repos, os.Getenv("MORA_GITHUB_TOKEN"))
	if err != nil {
		return sourceIngestResult{}, err
	}
	statusPath := syncStatusPathFor(cfg, s)
	st, _ := memory.LoadStatus(statusPath)
	st.Source = s.Name
	prog := newProgress(out, s.Name, "issues")
	write := func(mm memory.MappedMemory) error {
		// Snapshot bytes are attached to Meta only by the connector-private mapper
		// seam, never rendered or logged. The immutable write happens in Map below.
		if err := writeMappedMemory(cfg, mm); err != nil {
			return err
		}
		prog.tick()
		return nil
	}
	mapIssue := func(it memory.Item, scope string, budget int) memory.MappedMemory {
		mm := githubissues.MapIssue(it, scope, budget)
		if payload, ok := it.Payload.(githubissues.Payload); ok {
			if err := writeGitHubSnapshot(cfg, payload); err != nil {
				// memory.Ingest has no error-returning Map seam. Attach a sentinel that
				// makes the Write callback fail before the searchable projection lands.
				if mm.Meta == nil {
					mm.Meta = map[string]any{}
				}
				mm.Meta["snapshot_error"] = err.Error()
			}
		}
		return mm
	}
	writeChecked := func(mm memory.MappedMemory) error {
		if msg, ok := mm.Meta["snapshot_error"].(string); ok && msg != "" {
			return errors.New(msg)
		}
		return write(mm)
	}
	res, ingErr := memory.Ingest(memory.IngestParams{
		Context: ctx, Fetcher: fetcher, Kind: githubissues.KindIssue, Scope: s.Scope, BodyBudget: 64 * 1024,
		Status: st, Map: mapIssue, Write: writeChecked,
		Checkpoint: func(status *memory.SyncStatus) error { return memory.SaveStatus(statusPath, status) },
	})
	prog.done()
	return sourceIngestResultFromMemory(res), persistSyncStatus(out, statusPath, res.Status, ingErr)
}

func writeGitHubSnapshot(cfg Config, payload githubissues.Payload) error {
	s := payload.Snapshot
	stamp := strings.NewReplacer(":", "-", "/", "-").Replace(s.UpdatedAt)
	if stamp == "" {
		stamp = "unknown-update"
	}
	path := filepath.Join(cfg.StateDir, "source-evidence", "github", s.Repository,
		strconv.Itoa(s.Number), osSafeBase(stamp)+".json")
	body := append(append([]byte(nil), payload.Bytes...), '\n')
	if err := atomicCreate(path, body, 0o600); err != nil && !os.IsExist(err) {
		return fmt.Errorf("preserving immutable GitHub source snapshot: %w", err)
	}
	return nil
}

func connectGitHub(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("connect github", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var repos multiFlag
	fs.Var(&repos, "repo", "allowlisted owner/repo (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q (usage: mora connect github [--repo owner/repo]...)", fs.Arg(0))
	}
	if len(repos) == 0 {
		repos = append(repos, githubissues.DefaultRepositories...)
	}
	clean, err := githubissues.ValidateRepositories(repos)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := mutateSources(cfg, func(sources []Source) ([]Source, error) {
		now := time.Now().UTC().Format(time.RFC3339)
		for i := range sources {
			if sources[i].Type == "github" {
				sources[i].Repositories = clean
				sources[i].Enabled = genericutil.Ptr(true)
				return sources, nil
			}
		}
		return append(sources, Source{
			Name: "github", Type: "github", Scope: "personal", Repositories: clean,
			Enabled: genericutil.Ptr(true), CreatedAt: now,
		}), nil
	}); err != nil {
		return err
	}
	if os.Getenv("MORA_GITHUB_TOKEN") == "" {
		fmt.Fprintln(stdout, "GitHub token not set; using the lower-rate anonymous API for public repositories.")
		fmt.Fprintln(stdout, "Set MORA_GITHUB_TOKEN for private repositories or higher rate limits; Mora never stores or logs it.")
	}
	total, syncErr := backfillEnabledGitHub(ctx, cfg, stdout)
	fmt.Fprintf(stdout, "synced %d issue(s) from %s\n", total, strings.Join(clean, ", "))
	return syncErr
}

// connectIMessage is the one-command convenience (parallel to `connect google`):
// enable iMessage, show readiness with the Full Disk Access guidance, and — only if
// the database is actually readable — backfill conversations now. When FDA is not yet
// granted it stops at the honest guidance (no false backfill), so the user can grant
// access and re-run. macOS-only.
// connectFilesystem is the one-shot convenience for a filesystem source: it
// registers (or refreshes) the directory as an ENABLED filesystem source, indexes
// it, and rebuilds the search index — the same add+enable+ingest flow `connect
// google`/`connect imessage` run, so a folder is as turnkey as the other
// connectors (D-06: connect is the deliberate enable+backfill path). Unlike
// `sources add` (which lands DISABLED behind the consent gate, D-11), naming a
// folder here IS the consent. The default source name is the folder's base name
// so two folders coexist without an explicit --name; re-connecting the same name
// refreshes in place rather than stacking duplicates (mirrors addSource).
// connectFilesystemReceipt reports the registered source and the first walk it
// performed, so an agent can tell "registered" from "registered and indexed".
type connectFilesystemReceipt struct {
	Source string `json:"source"`
	Path   string `json:"path"`
	Scope  string `json:"scope"`
	Items  int    `json:"items"`
}

func connectFilesystem(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	// Pull a leading positional <path> out before flag parsing: Go's flag package
	// stops at the first non-flag arg, so `connect filesystem ~/dir --name x` would
	// otherwise silently drop --name. A leading flag form (`--name x ~/dir`) still
	// works via fs.Arg(0) below.
	var positional string
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, rest = args[0], args[1:]
	}
	fs := flag.NewFlagSet("connect filesystem", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "source name (default: the folder's base name)")
	scope := fs.String("scope", "personal", "scope")
	pathFlag := fs.String("path", "", "directory to index (alternative to the positional <path>)")
	jsonOut := fs.Bool("json", false, "emit the connect receipt as JSON")
	if err := fs.Parse(rest); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	// Registry repair notes, walk progress, and the closing setup state are
	// diagnostics; under --json they leave stdout to the receipt.
	progress := progressWriter(stdout, stderr, *jsonOut)
	path := *pathFlag
	if path == "" {
		path = positional
	}
	if path == "" {
		path = fs.Arg(0)
	}
	if path == "" {
		return errors.New("usage: mora connect filesystem <path> [--name <name>] [--scope <scope>]")
	}
	path = genericutil.ExpandHome(path)
	// Canonicalize to absolute: the scheduled `ingest --all` job runs from
	// launchd's cwd, not the user's shell, so a persisted relative path would
	// target the wrong (or no) folder.
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", path, err)
	}
	path = abs
	// Fail loudly on a typo'd path, and require a directory. The ingest walk also
	// fails closed if a previously valid root later disappears, but connect must not
	// register a broken source in the first place.
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot read %q: %w", path, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%q is not a directory; connect filesystem takes a folder", path)
	}
	// Resolve symlinks so the persisted path is the real folder ingest will walk:
	// filepath.WalkDir does NOT descend a symlinked root, so a symlinked folder
	// (common with iCloud "Desktop & Documents", Google Drive for Desktop, or
	// Dropbox) passes the os.Stat check above yet would index zero files. Resolving
	// here keeps validation and ingest pointed at the same directory.
	if resolved, rerr := filepath.EvalSymlinks(path); rerr == nil {
		path = resolved
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	srcName := *name
	if srcName == "" {
		srcName = defaultFilesystemSourceName(path)
	}
	s := Source{Name: srcName, Type: "filesystem", Scope: *scope, Path: path, Enabled: genericutil.Ptr(true), CreatedAt: time.Now().Format(time.RFC3339)}
	// Serialize the read-modify-write (P3) directly (not via mutateSources) so the
	// custom "cannot read existing sources" error survives. The lease covers ONLY
	// load->mutate->save and is released BEFORE the (potentially multi-minute)
	// ingest+rebuild below — a sources lease must never be held across ingest.
	if err := func() error {
		release, lerr := acquireSourcesLock(cfg, time.Now())
		if lerr != nil {
			return lerr
		}
		defer release()
		// Refuse to overwrite an unreadable sources.json: with the error swallowed, a
		// corrupt file would be replaced by ONLY the new source, destroying every other
		// registered connector. Bail and leave the file for the user to repair. The
		// reload happens INSIDE the lease so a concurrent writer's change is not lost.
		sources, err := loadSources(cfg)
		if err != nil {
			return fmt.Errorf("cannot read existing sources (fix or remove %s): %w", filepath.Join(cfg.ConfigDir, "sources.json"), err)
		}
		var next []Source
		for _, existing := range sources {
			if existing.Type == "filesystem" && existing.Path == "" {
				// Registry repair: older binaries minted a pathless filesystem row on
				// `connectors enable filesystem`. It can never ingest — it only fails
				// the hourly walk and raises a red "never synced" banner — and this
				// command is exactly the repair the ingest error recommends, so drop
				// it here while adding the healthy row.
				fmt.Fprintf(progress, "removed legacy filesystem source %q (it had no path and could never sync)\n", existing.Name)
				if p := syncStatusPathFor(cfg, existing); p != "" {
					_ = os.Remove(p) // drop its failed-sync status so `sync status` stops listing it
				}
				continue
			}
			if existing.Name == s.Name {
				// Same name + same folder => a deliberate re-connect: refresh in place
				// and preserve the original add time. Same name + a DIFFERENT folder
				// (e.g. two dirs whose base name collides) would silently clobber the
				// first — refuse and point at --name instead.
				if existing.Type == "filesystem" && existing.Path == s.Path {
					s.CreatedAt = existing.CreatedAt
					continue
				}
				return fmt.Errorf("a source named %q already exists (path %q); pick another name with --name", existing.Name, existing.Path)
			}
			next = append(next, existing)
		}
		next = append(next, s)
		return saveSources(cfg, next)
	}(); err != nil {
		return err
	}
	// Ingest now (the named, consented convenience path — same as connect google).
	// Surface a partial-ingest warning BEFORE the rebuild so a resumable ingest
	// error is never masked by a later index-rebuild failure.
	n, ingestErr := ingestSource(cfg, s, progress)
	if ingestErr != nil {
		warnf(progress, "%s indexed %d file(s) before stopping (resumable): %v", s.Name, n, ingestErr)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return err
	}
	if ingestErr != nil {
		return ingestErr
	}
	if *jsonOut {
		return emitReceipt(stdout, "mora.connect.filesystem", 1, connectFilesystemReceipt{
			Source: s.Name, Path: path, Scope: s.Scope, Items: n,
		})
	}
	okf(stdout, "Enabled filesystem and indexed %d file(s) from %s.", n, path)
	renderSetupState(cfg, stdout)
	return nil
}

// defaultFilesystemSourceName derives a stable, filesystem-safe source name from a
// directory path (its base name), so `connect filesystem ~/notes` and `connect
// filesystem ~/docs` coexist without an explicit --name. Falls back to
// "filesystem" for degenerate paths (root, ".", empty base).
func connectIMessage(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("connect imessage", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sinceDays := fs.Int("since-days", 0, "iMessage backlog window in days (default 365; negative = all-time)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if runtimeGOOS() == "windows" {
		fmt.Fprintln(stdout, "iMessage is macOS-only and cannot be enabled on Windows.")
		return errors.New("imessage is macOS-only")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := setSourceEnabled(cfg, "imessage", true); err != nil {
		return err
	}
	// Persist an explicit window override so this and future `sync imessage` runs
	// reuse it (0 keeps the 365-day default in windowForIMessage; negative = all-time).
	// Mirrors `connect google --since-days`.
	if *sinceDays != 0 {
		if err := setSourceSinceDays(cfg, "imessage", *sinceDays); err != nil {
			return err
		}
	}
	okf(stdout, "enabled imessage. iMessage reads your local Messages database — no login needed.")
	if *sinceDays < 0 {
		fmt.Fprintln(stdout, "iMessage will ingest all available history.")
	} else {
		days := *sinceDays
		if days == 0 {
			days = defaultIMessageLookbackDays
		}
		fmt.Fprintf(stdout, "iMessage will ingest the last %d days; use --since-days to change this window.\n", days)
	}
	ready := printIMessageReadiness(stdout, true)
	if !ready {
		// Honest stop: readiness guidance already printed the next steps.
		return nil
	}
	total, err := backfillEnabledIMessage(ctx, cfg, stdout)
	fmt.Fprintf(stdout, "synced %d item(s).\n", total)
	renderSetupState(cfg, stdout)
	return err
}

// backfillEnabledIMessage runs the gated iMessage backfill for every ENABLED
// imessage source, then rebuilds the index. Mirrors backfillEnabledGoogle: disabled
// sources are skipped (D-07), sync errors are surfaced (never swallowed).
func backfillEnabledIMessage(ctx context.Context, cfg Config, stdout io.Writer) (int, error) {
	return runEnabledSourceBackfill(ctx, cfg, stdout, func(s Source) bool { return s.Type == "imessage" }, func(s Source, err error) {
		warnf(stdout, "%s sync incomplete (resumable): %v", s.Name, err)
	})
}

// backfillEnabledAppleCalendar runs the local Apple Calendar backfill for every
// enabled source, then rebuilds the index once. It is the targeted recovery
// seam for `mora sync applecalendar` and mirrors the iMessage contract: disabled
// sources are skipped and every source failure remains loud.
func backfillEnabledAppleCalendar(ctx context.Context, cfg Config, stdout io.Writer) (int, error) {
	return runEnabledSourceBackfill(ctx, cfg, stdout, func(s Source) bool { return s.Type == "applecalendar" }, func(s Source, err error) {
		warnf(stdout, "%s sync incomplete (resumable): %v", s.Name, err)
	})
}

// testHookFSPreWrite, when non-nil (tests only), fires just before each per-file
// suppress-decision-and-write in ingestFilesystem. It is the deterministic seam
// used to inject a concurrent `mora forget` into the write window and prove the
// walk never resurrects an atom forgotten mid-walk (#113). Nil in production.
var testHookFSPreWrite func(id string)

func ingestFilesystem(cfg Config, s Source, out io.Writer) (int, error) {
	result, err := ingestFilesystemDetailed(context.Background(), cfg, s, out)
	return result.Materialized, err
}

func ingestFilesystemDetailed(ctx context.Context, cfg Config, s Source, out io.Writer) (result sourceIngestResult, err error) {
	// Governance chokepoint (#52): filesystem re-ingest renders directly and does
	// NOT route through writeMappedMemory, so consult the ledger here too —
	// otherwise `mora forget --chat <src-id>` removes a filesystem memory that the
	// very next walk resurrects. The ledger is re-read PER FILE, under the same
	// governance lease `mora forget` takes, right at the write (writeUnlessForgotten)
	// — NOT a once-per-walk snapshot, which left a TOCTOU window where a forget
	// committing mid-walk was missed and the stale walker resurrected the atom
	// (#113). Only stable_id (item) forgets can match a filesystem memory: it
	// carries no participant identity, so `forget --handle/--email` never targets it.
	// Legacy installs may carry a pathless filesystem row (older binaries minted
	// one on `connectors enable filesystem`). Walking "" yields the useless
	// `lstat : no such file or directory` — name the real problem and the fix.
	if s.Path == "" {
		return result, fmt.Errorf("filesystem source %q has no path — re-add it with `mora connect filesystem <path>` (or remove the row from sources.json)", s.Name)
	}
	ignore := map[string]bool{".git": true, "node_modules": true, "dist": true, "build": true, ".next": true, ".venv": true, "__pycache__": true, "site-packages": true, ".tox": true, "vendor": true, ".gradle": true, ".idea": true}
	err = filepath.WalkDir(s.Path, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			// WalkDir reports a missing root and unreadable directories through the
			// callback. Returning nil here used to convert both into a clean empty
			// walk, which then stamped LastSuccessAt and made a stale source look
			// healthy. Abort this source; the caller continues with the next source.
			return fmt.Errorf("walking filesystem source %q at %q: %w", s.Name, path, walkErr)
		}
		if d.IsDir() {
			if ignore[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		ext := filepath.Ext(path)
		if !curatedMetadataFile(base) && !curatedAllowedExt(ext) && !curatedExtractExt(ext) {
			return nil
		}
		var text string
		if curatedExtractExt(ext) {
			// Non-plain-text (.docx/.pdf): extract the words rather than index raw bytes.
			var t string
			var derr error
			switch strings.ToLower(ext) {
			case ".pdf":
				t, derr = extractPDFText(path)
			default:
				t, derr = extractDocxText(path)
			}
			if derr != nil || t == "" {
				return nil // unreadable/empty/oversized — skip, never index garbage.
			}
			text = t
		} else {
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return fmt.Errorf("reading filesystem source %q file %q: %w", s.Name, path, rerr)
			}
			if len(b) == 0 {
				return nil
			}
			text = string(b)
		}
		if len(text) > 512*1024 {
			return nil // keep the index lean — same bound as the raw-read path.
		}
		result.Examined++
		rel, _ := filepath.Rel(s.Path, path)
		id := "src_" + ContentHash(s.Name+":"+rel)
		m := Memory{ID: id, Scope: s.Scope, Type: "source", Title: rel, Tags: []string{s.Type, s.Name}, Source: path, CreatedAt: time.Now().Format(time.RFC3339), Text: text}
		dest := filepath.Join(sourcesRoot(cfg), s.Type, s.Name, id+".md")
		body, _ := renderMemory(m)
		if testHookFSPreWrite != nil {
			testHookFSPreWrite(id)
		}
		if err := ctx.Err(); err != nil {
			result.Failed++
			result.Missing++
			return err
		}
		// A3 rule d: the filesystem path does NOT route through writeMappedMemory, so
		// journal it here too (miss it and every filesystem memory is silently
		// unrecoverable after a killed ingest). Durable header BEFORE the write is
		// visible; path line after a real publish.
		sourceKey := ingestpkg.SourceKey("filesystem", s.Name)
		if jerr := ingestpkg.EnsureJournalHeader(cfg, sourceKey, ingestPublishSeams()); jerr != nil {
			result.Failed++
			result.Missing++
			return jerr
		}
		// Suppression re-check + write, atomic under the governance lease so a
		// `mora forget` that commits mid-walk is honored, never resurrected (#113).
		wrote, werr := writeUnlessForgotten(cfg, "", id, dest, body, 0o644)
		if werr != nil {
			result.Failed++
			result.Missing++
			return werr
		}
		if wrote {
			ingestpkg.RecordPublishedPath(cfg, sourceKey, dest, ingestPublishSeams())
			result.Materialized++
		} else {
			// Governance suppression is intentional exclusion, not missing work.
			result.Examined--
		}
		return nil
	})
	// Record freshness so the brief/digest classifies this source by its real sync
	// health (new/no-changes/stale) instead of "unavailable (sync error)". Filesystem
	// has no fetcher Status of its own, so the walk persists one here — mirroring what
	// the gmail/calendar/imessage sync paths write via memory.SaveStatus. Preserve the
	// previous success timestamps across a failed attempt: LastAttemptAt advances and
	// LastError is recorded, but LastSynced/LastSuccessAt advance only after a complete
	// walk (the same honest-snapshot contract memory.Ingest enforces).
	if p := syncStatusPathFor(cfg, s); p != "" {
		st, loadErr := memory.LoadStatus(p)
		if loadErr != nil || st == nil {
			// The prior implementation overwrote this status on every walk. Keep that
			// recovery behavior for a malformed legacy file rather than letting status
			// bookkeeping mask the walk outcome.
			st = &memory.SyncStatus{}
		}
		now := time.Now().UTC().Format(time.RFC3339)
		st.Source = s.Name
		st.LastAttemptAt = now
		if err == nil {
			st.ItemCount = result.Materialized
			st.LastSynced = now
			st.LastSuccessAt = now
			st.ErrorCount = 0
			st.LastError = ""
		} else {
			st.ErrorCount = 1
			st.LastError = err.Error()
		}
		err = persistSyncStatus(out, p, st, err)
	}
	return result, err
}

// sourceFreshness maps each synced source to its last-synced timestamp, scanning
// only the sync/ dir (the watermark store lives under brief/ and is never read
// here). It keys off the loaded SyncStatus.Source — NOT a "google-" filename-
// prefix strip, which mis-keyed iMessage ("imessage-<name>.json" → "imessage-
// <name>" instead of "imessage"). A never-synced source (status present but
// LastSynced=="") is INCLUDED with an empty value so it can read "unavailable"
// downstream (SC#3 gap) rather than being silently dropped, which hid a broken
// source.

// curatedExtractExt reports whether ext is a non-plain-text format Mora ingests by
// EXTRACTING its text (vs reading raw bytes). Today: .docx (stdlib zip+xml) and
// .pdf (pinned ledongthuc/pdf — pure Go, recover-wrapped, capped; see pdf.go).
// PDF extraction is lossy on exotic font encodings and yields nothing on scanned
// documents (no OCR — that would break the no-CGO/single-binary constraint); such
// files are skipped, never indexed as garbage.

// extractDocxText returns the visible text of a .docx by reading word/document.xml
// (the main body) and concatenating its <w:t> runs, breaking paragraphs on </w:p>.
// Pure stdlib (archive/zip + encoding/xml) — no new dependency, no CGO. Untrusted
// input is bounded by docxMaxDecompressed (zip-bomb guard); Go's xml.Decoder does
// not expand DTD entities, so billion-laughs does not apply.
func extractDocxText(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	var doc *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			doc = f
			break
		}
	}
	if doc == nil {
		return "", fmt.Errorf("not a docx (no word/document.xml): %s", filepath.Base(path))
	}
	rc, err := doc.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	dec := xml.NewDecoder(io.LimitReader(rc, docxMaxDecompressed))
	var b strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t": // <w:t> — a text run
				inText = true
			case "tab":
				b.WriteByte('\t')
			case "br", "cr":
				b.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p": // paragraph boundary
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
	}
	return strings.TrimSpace(b.String()), nil
}
