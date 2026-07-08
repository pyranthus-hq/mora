package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/mattn/go-isatty"
	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/imessage"
)

// cmdConnectors dispatches the connector-registry command group:
// `mora connectors list|enable|disable|setup`. It mirrors cmdSources' shape
// (arg-0 switch, loadConfig up front). stdin is threaded for the Plan-04 setup
// menu; the OAuth consent path reads NO stdin (browser loopback).
func cmdConnectors(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	if len(args) == 0 {
		return errors.New("usage: mora connectors list|enable|disable|setup")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("connectors list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "json output")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		sources, err := loadSources(cfg)
		if err != nil {
			return err
		}
		// Per-type enabled state (D-02): a catalog type enabled iff some source
		// row of that Type is enabled; absent => false.
		enabledByType := map[string]bool{}
		for _, s := range sources {
			if s.IsEnabled() {
				enabledByType[s.Type] = true
			}
		}
		catalog := connectorCatalogForGOOS(runtimeGOOS())
		rows := make([]catalogRow, 0, len(catalog))
		for _, c := range catalog {
			rows = append(rows, catalogRow{
				Type:      c.Type,
				Name:      c.DisplayName,
				Enabled:   enabledByType[c.Type],
				NeedsAuth: c.NeedsAuth,
			})
		}
		return emit(stdout, rows, *jsonOut)
	case "enable":
		if len(args) != 2 {
			return errors.New("usage: mora connectors enable <type>")
		}
		return enableConnector(ctx, cfg, args[1], stdout, stdin)
	case "disable":
		if len(args) != 2 {
			return errors.New("usage: mora connectors disable <type>")
		}
		return disableConnector(cfg, args[1], stdout)
	case "setup":
		// D-08: re-open the same interactive setup menu anytime.
		return runSetupMenu(ctx, cfg, stdin, stdout)
	default:
		return errors.New("usage: mora connectors list|enable|disable|setup")
	}
}

// enableConnector is the "log me in" half of consent (REG-02): it runs OAuth
// consent if the type needs it, flips the Enabled bit, then STOPS — it pulls
// ZERO data (REG-03 / D-04). Backfill is a separate, explicit step (sync/ingest).
// Unknown types are rejected (D-03 / ASVS V5), never silently no-op'd.
func enableConnector(ctx context.Context, cfg Config, ctype string, stdout io.Writer, stdin io.Reader) error {
	info, ok := lookupCatalog(ctype)
	if !ok {
		return fmt.Errorf("unknown connector %q; run `mora connectors list`", ctype)
	}
	if runtimeGOOS() == "windows" && macOSOnlyConnector(ctype) {
		fmt.Fprintf(stdout, "%s is macOS-only and cannot be enabled on Windows.\n", info.DisplayName)
		return fmt.Errorf("%s is macOS-only", ctype)
	}
	if info.NeedsAuth {
		// Run interactive consent only when we lack a saved token AND stdin is a
		// real terminal. The loopback flow opens a browser and BLOCKS up to 5
		// minutes on the HTTP callback, so it must never run on a non-TTY (tests,
		// pipes, the Plan-04 non-TTY menu path) — there we just flip the bit and
		// hint the user to authorize separately. Token reuse on re-enable.
		if _, err := google.LoadToken(googleTokenPath(cfg)); err != nil {
			if isInteractive(stdin) {
				printGoogleAuthPreamble(stdout)
				oc, err := google.ResolveOAuthConfig(google.Scopes)
				if err != nil {
					return err
				}
				tok, err := google.StartLoopbackAuth(ctx, oc, stdout)
				if err != nil {
					return err
				}
				if err := google.SaveToken(googleTokenPath(cfg), tok); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(stdout, "note: %s needs Google authorization — run `mora connect google` (or `mora connectors enable %s` in a terminal) to grant consent.\n", ctype, ctype)
			}
		} else {
			// CROSS-PHASE TOUCH (UI-SPEC §C): a saved token means the browser step is
			// skipped — say so, so the absent sign-in prompt isn't a "did it work?".
			fmt.Fprintln(stdout, "Reusing your saved Google sign-in — no need to authorize again.")
		}
		// Ensure the gmail/calendar source rows exist before flipping the bit.
		if err := ensureGoogleSources(cfg, ""); err != nil {
			return err
		}
	}
	if err := setSourceEnabled(cfg, ctype, true); err != nil {
		return err
	}
	// STOP — do NOT call ingestSource/NewLiveFetcher/backfill here (REG-03/D-04).
	if ctype == "imessage" {
		// No-auth path: the real gate is Full Disk Access, not a login (Surface 1).
		okf(stdout, "enabled imessage. iMessage reads your local Messages database — no login needed.")
		fmt.Fprintln(stdout, "Next: grant Full Disk Access, then pull data with `mora sync imessage`.")
		fmt.Fprintln(stdout, "Check readiness anytime with `mora doctor`.")
		if runtimeGOOS() != "darwin" {
			fmt.Fprintf(stdout, "note: iMessage ingest only runs on macOS; this machine is %s.\n", runtimeGOOS())
		}
		return nil
	}
	if ctype == "applecalendar" {
		// No-auth path, same gate as iMessage: local store + Full Disk Access.
		okf(stdout, "enabled applecalendar. Apple Calendar reads your local Calendar database — no login needed.")
		fmt.Fprintln(stdout, "Next: grant Full Disk Access (the same toggle iMessage uses), then pull data with `mora ingest run --source applecalendar`.")
		if runtimeGOOS() != "darwin" {
			fmt.Fprintf(stdout, "note: Apple Calendar ingest only runs on macOS; this machine is %s.\n", runtimeGOOS())
		}
		return nil
	}
	okf(stdout, "enabled %s. Pull data with `mora sync google` (or `mora ingest run --all`).", ctype)
	return nil
}

// printGoogleAuthPreamble prints the warm, read-only-reassuring intro shown right
// before the Google browser sign-in, at BOTH entry points (`connect google` and
// the in-menu enable) so the two read identically and Google feels like a peer of
// iMessage's friendly intro. CROSS-PHASE TOUCH (UI-SPEC §C, copy-only).
func printGoogleAuthPreamble(w io.Writer) {
	fmt.Fprintln(w, "Connecting Google (Gmail + Calendar)…")
	fmt.Fprintln(w, "Mora will open your browser to sign in with Google. It asks for READ-ONLY")
	fmt.Fprintln(w, "access to your mail and calendar — it can never send, delete, or change")
	fmt.Fprintln(w, "anything, and nothing leaves this machine.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, `Heads-up: Google shows a "Google hasn't verified this app" screen first.`)
	fmt.Fprintln(w, "That's expected. Mora is a small open-source app still going through Google's")
	fmt.Fprintln(w, `review, not a malicious one. Click "Advanced", then "Go to Mora" (Google labels`)
	fmt.Fprintln(w, `it "unsafe") to continue. The access stays read-only and on-device either way.`)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Prefer your own Google keys? Point Mora at them once and it skips the shared app:")
	fmt.Fprintln(w, "  MORA_GOOGLE_CREDENTIALS=/path/to/client.json mora connect google")
}

// isGoogleAuthError reports whether a sync/ingest error looks like an expired or
// invalid Google sign-in (the 7-day Testing-mode refresh-token trap), so callers
// can surface the specific recovery step instead of a generic resumable warning.
func isGoogleAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, marker := range []string{"oauth", "token", "invalid_grant", "unauthorized", "401", "expired", "refresh"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// renderSetupState prints the unified "you're ready" closing block shared by
// `connect google`, `connect imessage`, and the interactive setup menu (UI-SPEC
// §D): a per-connector enabled-state line in catalog order, then the two forward
// pointers. It is the ONE place that names the first useful command (`mora
// search`) and then how to wire Mora into an agent (`mora mcp serve`).
func renderSetupState(cfg Config, w io.Writer) {
	sources, _ := loadSources(cfg)
	enabled := map[string]bool{}
	for _, s := range sources {
		if s.IsEnabled() {
			enabled[s.Type] = true
		}
	}
	sty := newStyler(w, false)
	fmt.Fprintf(w, "\n%s\n", sty.accent("Setup complete. Here's where things stand:"))
	for _, c := range connectorCatalog {
		mark := sty.dim("·  not set up yet")
		if enabled[c.Type] {
			mark = sty.ok("✓  enabled")
		}
		fmt.Fprintf(w, "  %-16s %s\n", c.DisplayName, mark)
	}
	fmt.Fprintf(w, "\n%s\n", sty.accent("Next:"))
	fmt.Fprintln(w, "  1. Try a search →  mora search \"<a recent topic>\"")
	fmt.Fprintln(w, "  2. Wire Mora into your agent, once →  mora mcp serve")
	fmt.Fprintf(w, "%s\n", sty.dim("     Claude Code:  claude mcp add mora -s user -- mora mcp serve"))
	fmt.Fprintf(w, "%s\n", sty.dim("     Codex:        codex mcp add mora -- mora mcp serve"))
}

// applySetupSelection is the pure, TTY-free consequential half of the setup menu
// (Plan 04). It enables every selected connector via the Plan-02 enableConnector
// codepath (OAuth-then-STOP, REG-02/REG-03) and then — and ONLY if doBackfill is
// true — runs the gated google backfill. With doBackfill=false it performs ZERO
// ingest: no vault write, no sync call. Taking the confirm result as a parameter
// makes the headline consent guarantee ("no affirmative confirm ⇒ zero ingest",
// D-09) assertable in a unit test without a TTY or huh. It sits ON TOP of
// enableConnector — it never reimplements enable/auth (T-04-03).
func applySetupSelection(ctx context.Context, cfg Config, selected []string, doBackfill bool, stdout io.Writer, stdin io.Reader) error {
	for _, ctype := range selected {
		if err := enableConnector(ctx, cfg, ctype, stdout, stdin); err != nil {
			return err
		}
	}
	if !doBackfill {
		// Default-NO path (D-09): consent without pulling data. ZERO ingest.
		return nil
	}
	total, err := backfillEnabledGoogle(ctx, cfg, stdout)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "backfilled %d item(s). Run `mora sync status`.\n", total)
	return nil
}

// runSetupMenu launches the interactive connector setup menu (REG-01, D-08/D-09):
// a multi-select of catalog connectors, sequential OAuth for each via the enable
// path, then an explicit "Pull data now?" confirm DEFAULTING TO NO. It is
// TTY-guarded (Pitfall 2 / T-04-01): on non-TTY stdin (pipe / CI / `go test` with
// empty stdin) it prints a hint and returns immediately — it NEVER blocks. The
// menu only gathers selection + confirm; the consequential actions are delegated
// to the pure applySetupSelection seam.
func runSetupMenu(ctx context.Context, cfg Config, stdin io.Reader, stdout io.Writer) error {
	f, ok := stdin.(*os.File)
	if !ok || !isatty.IsTerminal(f.Fd()) {
		// Non-interactive (pipe / CI / test). Do NOT block (T-04-01).
		fmt.Fprintln(stdout, "Non-interactive terminal — skipping setup menu.")
		fmt.Fprintln(stdout, "Enable connectors with: mora connectors enable <type>")
		return nil
	}

	// The Apocrypha eye — shown once at the top of interactive setup (TTY only).
	printBanner(stdout)

	catalog := connectorCatalogForGOOS(runtimeGOOS())
	options := make([]huh.Option[string], 0, len(catalog))
	for _, c := range catalog {
		options = append(options, huh.NewOption(c.DisplayName, c.Type))
	}

	var selected []string
	selForm := huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select connectors to enable").
				Description("space or x toggles a connector · ↑/↓ moves · ctrl+a selects all · enter continues").
				Filterable(false).
				Options(options...).
				Value(&selected),
		),
	).WithInput(stdin).WithOutput(stdout)
	if err := selForm.Run(); err != nil {
		return err
	}
	if len(selected) == 0 {
		fmt.Fprintln(stdout, "No connectors selected — nothing was enabled.")
		fmt.Fprintln(stdout, "Tip: in the menu, press space or x to toggle each connector (enter only confirms the screen).")
		fmt.Fprintln(stdout, "Re-open the menu anytime with `mora connectors setup`, or enable one directly:")
		fmt.Fprintln(stdout, "  mora connectors enable <gmail|calendar|filesystem|imessage>")
		return nil
	}

	imessageSelected := containsType(selected, "imessage")

	// Canonical guided order (UI-SPEC §B): multi-select → (if iMessage) readiness →
	// Google detect-and-skip → deny-list → backfill confirm → enable → backfill.
	if imessageSelected {
		fmt.Fprintln(stdout, "Checking iMessage readiness…")
		printIMessageReadiness(stdout, true)
	}

	// CROSS-PHASE TOUCH (UI-SPEC §C/E-7, control-flow): detect Google placeholder
	// before opening anything; skip without dead-ending so iMessage/filesystem still
	// complete. The decision is the pure googleSetupStep helper (unit-tested).
	if remaining, skipMsg, skipped := googleSetupStep(selected); skipped {
		fmt.Fprintln(stdout, skipMsg)
		selected = remaining
		if len(selected) == 0 {
			return nil // nothing left to set up
		}
	}

	// Deny-list (optional, skippable — Surface 2). Both inputs default blank; pressing
	// enter twice = include everyone. Only shown when iMessage is selected.
	var denyContacts, denyConvos []string
	if imessageSelected {
		var contactsStr, convosStr string
		denyForm := huh.NewForm(
			huh.NewGroup(huh.NewInput().
				Title("Exclude any contacts? (optional)").
				Description("Enter phone numbers or emails to skip across ALL chats, comma-separated. Leave blank to include everyone.").
				Value(&contactsStr)),
			huh.NewGroup(huh.NewInput().
				Title("Exclude any conversations? (optional)").
				Description("Enter conversation names (as shown in Messages) to skip, comma-separated. Leave blank to include all conversations.").
				Value(&convosStr)),
		).WithInput(stdin).WithOutput(stdout)
		if err := denyForm.Run(); err != nil {
			return err
		}
		denyContacts = parseCSVList(contactsStr)
		denyConvos = parseCSVList(convosStr)
		if len(denyContacts) == 0 && len(denyConvos) == 0 {
			fmt.Fprintln(stdout, "Deny-list: none — all contacts and conversations will be ingested (within the 90-day lookback).")
		} else {
			fmt.Fprintln(stdout, "Deny-list saved:")
			fmt.Fprintf(stdout, "  contacts:      %s\n", strings.Join(denyContacts, ", "))
			fmt.Fprintf(stdout, "  conversations: %s\n", strings.Join(denyConvos, ", "))
			fmt.Fprintln(stdout, "These will be skipped on every iMessage sync. Edit anytime via `mora connectors setup`.")
		}
	}

	doBackfill := false // D-09: backfill defaults to NO.
	confirmForm := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Pull data now?").
				Description("You can always backfill later with `mora sync google` or `mora sync imessage`.").
				Affirmative("Yes").
				Negative("No").
				Value(&doBackfill),
		),
	).WithInput(stdin).WithOutput(stdout)
	if err := confirmForm.Run(); err != nil {
		return err
	}

	// Persist the deny-list BEFORE the consent seam (D-07): setIMessageDenyList
	// creates the imessage source row if needed and setSourceEnabled preserves
	// the deny fields, so the order is safe — and it makes the user's privacy
	// exclusions durable even if the google backfill inside
	// applySetupSelection fails (its error used to abort this function AFTER
	// "Deny-list saved:" was printed but BEFORE anything was persisted; the
	// next `mora sync imessage` then ingested exactly what they excluded).
	if imessageSelected {
		if err := setIMessageDenyList(cfg, denyContacts, denyConvos); err != nil {
			return err
		}
	}
	// Enable + (if confirmed) google backfill via the shared consent seam.
	if err := applySetupSelection(ctx, cfg, selected, doBackfill, stdout, stdin); err != nil {
		return err
	}
	if imessageSelected {
		if doBackfill {
			if ready, _ := imessage.ProbeReadable(chatDBPath()); ready && runtimeGOOS() == "darwin" {
				total, err := backfillEnabledIMessage(ctx, cfg, stdout)
				if err != nil {
					return err
				}
				fmt.Fprintf(stdout, "backfilled %d iMessage conversation(s).\n", total)
			} else {
				fmt.Fprintln(stdout, "note: iMessage isn't ready yet (Full Disk Access) — skipped. Run `mora doctor`, then `mora sync imessage`.")
			}
		}
	}
	renderSetupState(cfg, stdout)
	return nil
}

// googleSetupStep implements the guided-setup Google placeholder detect-and-skip
// (UI-SPEC §C/E-7, control-flow). When gmail/calendar are selected but Google creds
// are a placeholder/unconfigured, it returns the exact skippable BYO message and the
// selection with the google types removed — so the guided sequence CONTINUES
// (iMessage/filesystem still complete) instead of dead-ending. Detection reuses
// google.IsConfigured (the SAME guard as ResolveOAuthConfig), which opens NO browser
// or loopback. Pure (no I/O) so it is unit-testable without a TTY.
func googleSetupStep(selected []string) (remaining []string, skipMsg string, skipped bool) {
	if !containsType(selected, "gmail") && !containsType(selected, "calendar") {
		return selected, "", false
	}
	if google.IsConfigured() {
		return selected, "", false
	}
	return withoutTypes(selected, "gmail", "calendar"),
		"Skipping Google for now — set up creds later with `mora connectors enable gmail`. iMessage and filesystem need no creds.",
		true
}

// disableConnector is a non-destructive bit-flip (REG-04 / D-13): it sets Enabled
// false to stop ingest but KEEPS the OAuth token and all ingested memories
// (D-14/D-15). It is the deliberate ANTI-analog of cmdDisconnect — it MUST NOT
// call google.RevokeToken or remove the token file (that is disconnect's job).
func disableConnector(cfg Config, ctype string, stdout io.Writer) error {
	if _, ok := lookupCatalog(ctype); !ok {
		return fmt.Errorf("unknown connector %q; run `mora connectors list`", ctype)
	}
	if err := setSourceEnabled(cfg, ctype, false); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s disabled. Ingest stopped; existing memories kept and searchable. Re-enable instantly with `mora connectors enable %s`.\n", ctype, ctype)
	return nil
}
func cmdDisconnect(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) < 1 || args[0] != "google" {
		return errors.New("usage: mora disconnect google")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	tokPath := googleTokenPath(cfg)
	if tok, err := google.LoadToken(tokPath); err == nil {
		_ = google.RevokeToken(ctx, tok) // best effort
	}
	if err := os.Remove(tokPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintln(stdout, "disconnected google; token revoked and removed")
	return nil
}
