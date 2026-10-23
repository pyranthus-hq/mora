package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/mattn/go-isatty"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/eywa"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// The eywa connector talks to a governed company memory service through its
// agent MCP API. Pull is a normal connector sync: read-only evidence records
// under sources/eywa/. Push is a separate, owner-only CLI action that is off by
// default, previews exactly what leaves, and asks before sending. It is never
// an MCP tool, so no agent can trigger it under any mcp_write_policy.

const eywaSourceName = "eywa"

// Test seams. Production uses the defaults.
var (
	eywaHTTPClientFn  = func() *http.Client { return &http.Client{Timeout: 60 * time.Second} }
	eywaSleepFn       = func(ctx context.Context, d time.Duration) error { return sleepCtx(ctx, d) }
	eywaPollInterval  = 2 * time.Second
	confirmEywaPushFn = confirmEywaPush
)

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// eywaClientFor builds a client for one source row. The secret is read from
// the environment here and nowhere else.
func eywaClientFor(s Source) (*eywa.Client, error) {
	if s.Eywa == nil {
		return nil, errors.New("eywa is not configured — run `mora connect eywa --endpoint <mcp-url> --token-url <url> --client-id <id>`")
	}
	return eywa.NewClient(eywa.Config{
		Endpoint: s.Eywa.Endpoint, TokenURL: s.Eywa.TokenURL, ClientID: s.Eywa.ClientID,
		ClientSecret: os.Getenv(eywa.SecretEnv), OAuthScope: s.Eywa.OAuthScope,
		AgentID: s.Eywa.AgentID, Topic: s.Eywa.Topic,
	}, eywa.ClientOptions{HTTP: eywaHTTPClientFn(), Sleep: eywaSleepFn, Version: BuildVersion})
}

// eywaKnownVersions reads what the vault already holds about each pulled
// version, so a replacement seen only by id still resolves and earlier lineage
// is never lost. Read-only; an unreadable record is skipped.
func eywaKnownVersions(cfg Config) map[string]eywa.Known {
	known := map[string]eywa.Known{}
	root := filepath.Join(sourcesRoot(cfg), eywa.Provider)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		m, perr := parseMemory(path)
		if perr != nil || m.Provider != eywa.Provider {
			return nil
		}
		if id, k, ok := eywa.KnownFromMeta(m.Meta); ok {
			known[id] = k
		}
		return nil
	})
	return known
}

// eywaPageTracker wraps the pull fetcher so the wiring knows which feed
// position produced the page whose records are being written. A per-record
// failure pins the cursor to the start of the first page that had one.
type eywaPageTracker struct {
	inner     *eywa.Fetcher
	pageStart string // feed position the current page was requested from ("" = since-walk)
	failed    bool
	failStart string // position of the first page with a failed record
}

func (t *eywaPageTracker) FetchPage(kind memory.ItemKind, w memory.FetchWindow, cursor string) (memory.Page, error) {
	return t.FetchPageContext(context.Background(), kind, w, cursor)
}

func (t *eywaPageTracker) FetchPageContext(ctx context.Context, kind memory.ItemKind, w memory.FetchWindow, cursor string) (memory.Page, error) {
	start := cursor
	if start == "" {
		start = w.SyncCursor
	}
	page, err := t.inner.FetchPageContext(ctx, kind, w, cursor)
	if err == nil {
		t.pageStart = start
	}
	return page, err
}

func (t *eywaPageTracker) recordFailure() {
	if !t.failed {
		t.failed, t.failStart = true, t.pageStart
	}
}

// hold rewrites the persisted positions so nothing at or after the first failed
// change is skipped by the next sync. A mid-walk checkpoint and the between-run
// cursor are both pinned to the start of the page that held the first failure;
// the records written successfully on that page are rewritten unchanged
// (content-hash skip) when it is fetched again.
func (t *eywaPageTracker) hold(st *memory.SyncStatus) {
	if st == nil || !t.failed {
		return
	}
	st.PendingSyncCursor = ""
	if st.Checkpoint != "" {
		st.Checkpoint = t.failStart
		if t.failStart == "" {
			st.CursorMode = ""
		}
		return
	}
	st.IncrementalCursor = t.failStart
	st.CursorMode = ""
}

// ingestEywaDetailed pulls the change feed into read-only fact records. The
// feed cursor is a provider-native incremental position kept in the state dir
// SyncStatus. It advances past a page only when every record on it was written:
// a run with a failed fact lookup or vault write keeps the position at the
// start of the first page that failed, so the next sync retries that change.
func ingestEywaDetailed(ctx context.Context, cfg Config, s Source, out io.Writer) (sourceIngestResult, error) {
	client, err := eywaClientFor(s)
	if err != nil {
		return sourceIngestResult{}, err
	}
	known := eywaKnownVersions(cfg)
	fetcher := eywa.NewFetcher(client, func(id string) (eywa.Known, bool) { k, ok := known[id]; return k, ok }, time.Now)
	statusPath := syncStatusPathFor(cfg, s)
	st, _ := memory.LoadStatus(statusPath)
	st.Source = s.Name
	startCursor := st.IncrementalCursor
	tracker := &eywaPageTracker{inner: fetcher, pageStart: startCursor}
	days := s.SinceDays
	if days <= 0 {
		days = 35
	}
	prog := newProgress(out, s.Name, "facts")
	write := func(mm memory.MappedMemory) (bool, error) {
		if msg, ok := mm.Meta["eywa_record_error"].(string); ok && msg != "" {
			// A per-record refusal is counted as a dropped item and surfaced in the
			// sync status; it never reaches the vault.
			tracker.recordFailure()
			return false, errors.New(msg)
		}
		wrote, err := writeMappedMemoryDetailed(cfg, mm)
		if err != nil {
			tracker.recordFailure()
			return false, err
		}
		if wrote {
			prog.tick()
		}
		return wrote, nil
	}
	res, ingErr := memory.Ingest(memory.IngestParams{
		Context: ctx, Fetcher: tracker, Kind: eywa.KindFact, Scope: s.Scope, BodyBudget: 16 * 1024,
		Window: memory.FetchWindow{Since: time.Now().Add(-time.Duration(days) * 24 * time.Hour)},
		Status: st, Map: eywa.MapFact, WriteResult: write,
		Checkpoint: func(status *memory.SyncStatus) error {
			held := *status
			tracker.hold(&held)
			return memory.SaveStatus(statusPath, &held)
		},
	})
	prog.done()
	if res.Failed > 0 && !tracker.failed {
		// A record dropped before the write step (size limit): keep the run's
		// starting position rather than guess where it was.
		tracker.failed, tracker.failStart = true, startCursor
	}
	tracker.hold(res.Status)
	if out != nil && tracker.failed {
		fmt.Fprintln(out, "note: the change-feed position was kept at the first failed change; the next `mora sync eywa` retries it.")
	}
	if out != nil && fetcher.Stats.Skipped > 0 {
		fmt.Fprintf(out, "note: %d change(s) named versions this vault never pulled (older than the first sync window); they were skipped.\n", fetcher.Stats.Skipped)
	}
	return sourceIngestResultFromMemory(res), persistSyncStatus(out, statusPath, res.Status, ingErr)
}

func backfillEnabledEywa(ctx context.Context, cfg Config, stdout io.Writer) (int, error) {
	return runEnabledSourceBackfill(ctx, cfg, stdout, func(s Source) bool { return s.Type == "eywa" }, func(s Source, err error) {
		warnf(stdout, "%s sync incomplete (prior records preserved): %v", s.Name, err)
	})
}

func loadEywaSource(cfg Config) (Source, error) {
	sources, err := loadSources(cfg)
	if err != nil {
		return Source{}, err
	}
	for _, s := range sources {
		if s.Type == "eywa" && s.Eywa != nil {
			return s, nil
		}
	}
	return Source{}, errors.New("eywa is not configured — run `mora connect eywa --endpoint <mcp-url> --token-url <url> --client-id <id>`")
}

const connectEywaUsage = "usage: mora connect eywa --endpoint <mcp-url> --token-url <url> --client-id <id> [--oauth-scope <scope>] [--agent-id <id> --topic <topic>] [--scope <mora-scope>] [--no-sync]"

// connectEywa saves the connector's non-secret settings, enables it and, when
// the secret is present in the environment, runs the first pull.
func connectEywa(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("connect eywa", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	endpoint := fs.String("endpoint", "", "agent MCP endpoint (https)")
	tokenURL := fs.String("token-url", "", "OAuth2 token endpoint (https)")
	clientID := fs.String("client-id", "", "OAuth2 client id of the registered agent")
	oauthScope := fs.String("oauth-scope", "", "OAuth2 scope, if the token endpoint requires one")
	agentID := fs.String("agent-id", "", "agent id the service assigned (needed only for push)")
	topic := fs.String("topic", "", "the agent's scope topic (needed only for push)")
	scope := fs.String("scope", "global", "Mora scope for pulled facts")
	noSync := fs.Bool("no-sync", false, "save and enable without pulling now")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%v (%s)", err, connectEywaUsage)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q (%s)", fs.Arg(0), connectEywaUsage)
	}
	if err := eywa.ValidateEndpoints(*endpoint, *tokenURL, *clientID); err != nil {
		return fmt.Errorf("%v (%s)", err, connectEywaUsage)
	}
	if !validShareScope(*scope) {
		return fmt.Errorf("invalid scope %q — use personal, global, or project:<name>", *scope)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	settings := &memory.EywaSettings{Endpoint: *endpoint, TokenURL: *tokenURL, ClientID: *clientID,
		OAuthScope: *oauthScope, AgentID: *agentID, Topic: *topic}
	if err := mutateSources(cfg, func(sources []Source) ([]Source, error) {
		for i := range sources {
			if sources[i].Type == "eywa" {
				if sources[i].Eywa != nil {
					// Re-configuring never silently turns push on or off.
					settings.PushEnabled = sources[i].Eywa.PushEnabled
				}
				sources[i].Eywa = settings
				sources[i].Scope = *scope
				sources[i].Enabled = genericutil.Ptr(true)
				return sources, nil
			}
		}
		return append(sources, Source{
			Name: eywaSourceName, Type: "eywa", Scope: *scope, Eywa: settings,
			Enabled: genericutil.Ptr(true), CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}), nil
	}); err != nil {
		return err
	}
	okf(stdout, "enabled eywa. Pulled facts are read-only evidence; push stays off until `mora eywa push enable`.")
	if os.Getenv(eywa.SecretEnv) == "" {
		fmt.Fprintf(stdout, "Set %s to the agent's client secret, then run `mora sync eywa`. Mora never stores or logs it.\n", eywa.SecretEnv)
		return nil
	}
	if *noSync {
		fmt.Fprintln(stdout, "Pull facts with `mora sync eywa`.")
		return nil
	}
	total, syncErr := backfillEnabledEywa(ctx, cfg, stdout)
	fmt.Fprintf(stdout, "synced %d fact record(s)\n", total)
	return syncErr
}

// ---- push ----

// eywaPushRecord is the state-dir record of one pushed memory. It holds ids,
// hashes and statuses only, never memory text.
type eywaPushRecord struct {
	ContentHash string `json:"content_hash"`
	SourceURI   string `json:"source_uri"`
	WriteID     string `json:"write_id,omitempty"`
	Status      string `json:"status,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Error       string `json:"error,omitempty"`
	PushedAt    string `json:"pushed_at,omitempty"`
	CheckedAt   string `json:"checked_at,omitempty"`
}

type eywaPushState struct {
	Records map[string]eywaPushRecord `json:"records"`
}

func eywaPushStatePath(cfg Config) string {
	return filepath.Join(cfg.StateDir, "eywa", "push-"+eywaSourceName+".json")
}

func loadEywaPushState(cfg Config) (eywaPushState, error) {
	st := eywaPushState{Records: map[string]eywaPushRecord{}}
	b, err := os.ReadFile(eywaPushStatePath(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("eywa push state %s is unreadable: %w", eywaPushStatePath(cfg), err)
	}
	if st.Records == nil {
		st.Records = map[string]eywaPushRecord{}
	}
	return st, nil
}

func saveEywaPushState(cfg Config, st eywaPushState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := eywaPushStatePath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicio.Write(path, append(b, '\n'), 0o600)
}

// eywaPushItem is one memory in a push plan.
type eywaPushItem struct {
	Update  eywa.Update
	Memory  Memory
	State   string // "new", "changed", "unchanged", "refused"
	Refusal string
}

type eywaPushPlan struct {
	Scope string
	Topic string
	Items []eywaPushItem
}

func (p eywaPushPlan) pending() []eywaPushItem {
	var out []eywaPushItem
	for _, it := range p.Items {
		if it.State == "new" || it.State == "changed" {
			out = append(out, it)
		}
	}
	return out
}

// buildEywaPushPlan selects one scope of authored memories exactly as
// `mora share` does: memories/ only, never connector records (so pulled facts
// can never echo back), never tombstones or hidden Teach revisions, never
// pending MCP proposals (they are outside the vault until approved).
func buildEywaPushPlan(cfg Config, s Source, scope string) (eywaPushPlan, error) {
	mems, err := collectShareMemories(cfg, scope)
	if err != nil {
		return eywaPushPlan{}, err
	}
	st, err := loadEywaPushState(cfg)
	if err != nil {
		return eywaPushPlan{}, err
	}
	plan := eywaPushPlan{Scope: scope, Topic: s.Eywa.Topic}
	for _, m := range mems {
		full, perr := parseMemory(m.Path)
		if perr != nil {
			return eywaPushPlan{}, fmt.Errorf("reading %s: %w", m.Path, perr)
		}
		item := eywaPushItem{Memory: full}
		u, berr := eywa.BuildUpdate(s.Eywa.AgentID, full.ID, full.Title, full.Text)
		if berr != nil {
			item.State, item.Refusal = "refused", berr.Error()
			item.Update = eywa.Update{MemoryID: full.ID, Title: full.Title}
			plan.Items = append(plan.Items, item)
			continue
		}
		item.Update = u
		prior, had := st.Records[full.ID]
		switch {
		case !had || prior.WriteID == "":
			item.State = "new"
		case prior.ContentHash != u.ContentHash:
			item.State = "changed"
		default:
			item.State = "unchanged"
		}
		plan.Items = append(plan.Items, item)
	}
	return plan, nil
}

func printEywaPushPreview(w io.Writer, s Source, plan eywaPushPlan, full bool) {
	pending := plan.pending()
	fmt.Fprintf(w, "eywa push — scope %s → topic %q at %s\n", plan.Scope, plan.Topic, s.Eywa.Endpoint)
	fmt.Fprintf(w, "%d memory update(s) would leave this machine:\n", len(pending))
	for _, it := range plan.Items {
		switch it.State {
		case "new", "changed":
			fmt.Fprintf(w, "  + %s\t(%s) %s\n", it.Update.MemoryID, it.State, it.Update.Title)
			fmt.Fprintf(w, "      source_uri:   %s\n      content_hash: %s\n      characters:   %d\n",
				it.Update.SourceURI, it.Update.ContentHash, len([]rune(it.Update.Text)))
			if full {
				fmt.Fprintln(w, "      text (sent as both text and lineage excerpt):")
				for _, line := range strings.Split(it.Update.Text, "\n") {
					fmt.Fprintf(w, "        | %s\n", line)
				}
			}
		case "refused":
			fmt.Fprintf(w, "  ! %s\tnot sent: %s\n", it.Update.MemoryID, it.Refusal)
		}
	}
	unchanged := len(plan.Items) - len(pending)
	for _, it := range plan.Items {
		if it.State == "refused" {
			unchanged--
		}
	}
	if unchanged > 0 {
		fmt.Fprintf(w, "  (%d already pushed and unchanged; not re-sent)\n", unchanged)
	}
	fmt.Fprintln(w, "The service governs every update: it may admit it, send it to curator review (in_review), or hold it. Mora does not decide the outcome.")
	if !full {
		fmt.Fprintln(w, "full text: `mora eywa preview --scope "+plan.Scope+"`")
	}
}

func confirmEywaPush(stdin io.Reader, stdout io.Writer, scope string, n int) error {
	f, ok := stdin.(*os.File)
	if !ok || !isatty.IsTerminal(f.Fd()) {
		return fmt.Errorf("refusing to push non-interactively without --yes — review with `mora eywa preview --scope %s`, then re-run `mora eywa push --scope %s --yes`", scope, scope)
	}
	var yes bool
	confirm := huh.NewConfirm().
		Title(fmt.Sprintf("Send %d update(s) to the company memory service?", n)).
		Description("The updates listed above leave this machine. The service decides whether they are admitted.").
		Affirmative("Send").
		Negative("Cancel").
		Value(&yes)
	if err := confirm.Run(); err != nil {
		return err
	}
	if !yes {
		return errors.New("push cancelled — nothing left this machine")
	}
	return nil
}

type eywaPushRow struct {
	MemoryID string `json:"memory_id"`
	WriteID  string `json:"write_id,omitempty"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Error    string `json:"error,omitempty"`
}

type eywaPushReceipt struct {
	Scope   string        `json:"scope,omitempty"`
	Sent    int           `json:"sent"`
	Failed  int           `json:"failed"`
	Pending int           `json:"pending"`
	Updates []eywaPushRow `json:"updates"`
}

const eywaUsage = `usage:
  mora eywa push enable|disable           turn the explicit push opt-in on or off (off by default)
  mora eywa preview --scope <scope>       show exactly what a push would send, full text; sends nothing
  mora eywa push --scope <scope> [--yes] [--wait 10s] [--json]
                                          send new or changed authored memories as governed agent updates
  mora eywa status [--json]               re-check the service's decision on pushed updates`

func cmdEywa(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	if len(args) == 0 || genericutil.IsHelpFlag(args[0]) {
		if len(args) == 0 {
			return errors.New(eywaUsage)
		}
		_, err := fmt.Fprintln(stdout, eywaUsage)
		return err
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	switch args[0] {
	case "push":
		if len(args) == 2 && (args[1] == "enable" || args[1] == "disable") {
			return setEywaPushEnabled(cfg, args[1] == "enable", stdout)
		}
		return eywaPush(ctx, cfg, args[1:], stdout, stderr, stdin)
	case "preview":
		return eywaPreview(cfg, args[1:], stdout)
	case "status":
		return eywaStatus(ctx, cfg, args[1:], stdout, stderr)
	default:
		return errors.New(eywaUsage)
	}
}

func setEywaPushEnabled(cfg Config, on bool, stdout io.Writer) error {
	if err := mutateSources(cfg, func(sources []Source) ([]Source, error) {
		for i := range sources {
			if sources[i].Type == "eywa" && sources[i].Eywa != nil {
				if on && (sources[i].Eywa.AgentID == "" || sources[i].Eywa.Topic == "") {
					return nil, errors.New("push needs the agent id and scope topic — re-run `mora connect eywa ... --agent-id <id> --topic <topic>`")
				}
				copied := *sources[i].Eywa
				copied.PushEnabled = on
				sources[i].Eywa = &copied
				return sources, nil
			}
		}
		return nil, errors.New("eywa is not configured — run `mora connect eywa` first")
	}); err != nil {
		return err
	}
	if on {
		okf(stdout, "eywa push enabled. Every push still previews and asks before anything leaves; the service governs each update.")
	} else {
		okf(stdout, "eywa push disabled. Nothing will be sent.")
	}
	return nil
}

func eywaPreview(cfg Config, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("eywa preview", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scope := fs.String("scope", "", "Mora scope of authored memories")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *scope == "" {
		return errors.New("usage: mora eywa preview --scope <scope>")
	}
	s, err := loadEywaSource(cfg)
	if err != nil {
		return err
	}
	plan, err := buildEywaPushPlan(cfg, s, *scope)
	if err != nil {
		return err
	}
	if !s.Eywa.PushEnabled {
		fmt.Fprintln(stdout, "note: push is disabled; `mora eywa push enable` is required before anything can be sent.")
	}
	printEywaPushPreview(stdout, s, plan, true)
	return nil
}

func eywaPush(ctx context.Context, cfg Config, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	fs := flag.NewFlagSet("eywa push", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scope := fs.String("scope", "", "Mora scope of authored memories")
	yes := fs.Bool("yes", false, "send without interactive confirmation")
	wait := fs.Duration("wait", 10*time.Second, "how long to poll for the service's decision")
	jsonOut := fs.Bool("json", false, "emit the push receipt as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *scope == "" {
		return errors.New("usage: mora eywa push --scope <scope> [--yes] [--wait 10s] [--json]")
	}
	human := stdout
	if *jsonOut {
		human = stderr
	}
	s, err := loadEywaSource(cfg)
	if err != nil {
		return err
	}
	if !s.Eywa.PushEnabled {
		return errors.New("eywa push is off — it is opt-in. Review with `mora eywa preview --scope " + *scope + "`, then run `mora eywa push enable`")
	}
	plan, err := buildEywaPushPlan(cfg, s, *scope)
	if err != nil {
		return err
	}
	pending := plan.pending()
	printEywaPushPreview(human, s, plan, false)
	receipt := eywaPushReceipt{Scope: *scope, Updates: []eywaPushRow{}}
	if len(pending) == 0 {
		fmt.Fprintln(human, "nothing to push.")
		if *jsonOut {
			return emitReceipt(stdout, "mora.eywa.push", 1, receipt)
		}
		return nil
	}
	if !*yes {
		if err := confirmEywaPushFn(stdin, human, *scope, len(pending)); err != nil {
			return err
		}
	}
	client, err := eywaClientFor(s)
	if err != nil {
		return err
	}
	st, err := loadEywaPushState(cfg)
	if err != nil {
		return err
	}
	var fatal error
	for _, it := range pending {
		ack, rerr := client.Remember(ctx, s.Eywa.Topic, it.Update)
		now := time.Now().UTC().Format(time.RFC3339)
		if rerr != nil {
			var te *eywa.ToolError
			if !errors.As(rerr, &te) {
				// Auth, rate-limit or availability: stop; unsent updates stay pending.
				fatal = rerr
				break
			}
			rec := st.Records[it.Update.MemoryID]
			rec.Error, rec.CheckedAt = rerr.Error(), now
			st.Records[it.Update.MemoryID] = rec
			receipt.Failed++
			receipt.Updates = append(receipt.Updates, eywaPushRow{MemoryID: it.Update.MemoryID, Status: "failed", Error: rerr.Error()})
			continue
		}
		st.Records[it.Update.MemoryID] = eywaPushRecord{ContentHash: it.Update.ContentHash, SourceURI: it.Update.SourceURI,
			WriteID: ack.WriteID, Status: ack.Status, Reason: ack.Reason, PushedAt: now, CheckedAt: now}
		// Record each write id before polling, so a crash never forgets a send.
		if err := saveEywaPushState(cfg, st); err != nil {
			return err
		}
		receipt.Sent++
	}
	if err := saveEywaPushState(cfg, st); err != nil {
		return err
	}
	logUsage(cfg, usageEvent{Tool: "eywa_push", Results: receipt.Sent})
	if fatal != nil {
		return fmt.Errorf("eywa push stopped after %d update(s): %w", receipt.Sent, fatal)
	}
	sent := map[string]bool{}
	for _, it := range pending {
		if rec := st.Records[it.Update.MemoryID]; rec.WriteID != "" && rec.Error == "" {
			sent[it.Update.MemoryID] = true
		}
	}
	rows, pollErr := pollEywaStatuses(ctx, cfg, client, &st, sent, *wait)
	receipt.Updates = append(receipt.Updates, rows...)
	for _, r := range rows {
		if !eywa.FinalStatus(r.Status) {
			receipt.Pending++
		}
	}
	sort.Slice(receipt.Updates, func(i, j int) bool { return receipt.Updates[i].MemoryID < receipt.Updates[j].MemoryID })
	printEywaRows(human, receipt.Updates)
	if receipt.Pending > 0 {
		fmt.Fprintf(human, "%d update(s) not yet decided; check later with `mora eywa status`.\n", receipt.Pending)
	}
	if *jsonOut {
		if err := emitReceipt(stdout, "mora.eywa.push", 1, receipt); err != nil {
			return err
		}
	}
	if pollErr != nil {
		return fmt.Errorf("eywa push sent %d update(s) but could not read every outcome: %w", receipt.Sent, pollErr)
	}
	if receipt.Failed > 0 {
		return fmt.Errorf("%d of %d update(s) were refused by the service", receipt.Failed, len(pending))
	}
	return nil
}

// pollEywaStatuses polls get_memory(write_id) until every selected write has a
// final status or the wait elapses, and records what it read.
func pollEywaStatuses(ctx context.Context, cfg Config, client *eywa.Client, st *eywaPushState, ids map[string]bool, wait time.Duration) ([]eywaPushRow, error) {
	deadline := time.Now().Add(wait)
	var pollErr error
	for {
		open := 0
		for id := range ids {
			rec := st.Records[id]
			if eywa.FinalStatus(rec.Status) {
				continue
			}
			ws, err := client.GetWriteStatus(ctx, rec.WriteID)
			if err != nil {
				pollErr = err
				break
			}
			rec.Status, rec.Reason, rec.CheckedAt = ws.Status, ws.Reason, time.Now().UTC().Format(time.RFC3339)
			st.Records[id] = rec
			if !eywa.FinalStatus(ws.Status) {
				open++
			}
		}
		if err := saveEywaPushState(cfg, *st); err != nil {
			return nil, err
		}
		if pollErr != nil || open == 0 || !time.Now().Before(deadline) {
			break
		}
		if err := eywaSleepFn(ctx, eywaPollInterval); err != nil {
			pollErr = err
			break
		}
	}
	rows := make([]eywaPushRow, 0, len(ids))
	for id := range ids {
		rec := st.Records[id]
		rows = append(rows, eywaPushRow{MemoryID: id, WriteID: rec.WriteID, Status: rec.Status, Reason: rec.Reason})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].MemoryID < rows[j].MemoryID })
	return rows, pollErr
}

func describeEywaStatus(status string) string {
	switch status {
	case "admitted", "admitted-unchanged":
		return "admitted by the service"
	case "in_review":
		return "sent to curator review — not served until a curator approves"
	case "held":
		return "held by the service (failed a lineage or content check)"
	case "rejected":
		return "rejected by a curator"
	case "no-fact":
		return "no durable fact was found in the text"
	case "received":
		return "received; not decided yet"
	case "failed":
		return "refused"
	default:
		return status
	}
}

func printEywaRows(w io.Writer, rows []eywaPushRow) {
	for _, r := range rows {
		line := fmt.Sprintf("  %s → %s: %s", r.MemoryID, orNone(r.WriteID), describeEywaStatus(r.Status))
		if r.Reason != "" {
			line += " (" + r.Reason + ")"
		}
		if r.Error != "" {
			line += " — " + r.Error
		}
		fmt.Fprintln(w, line)
	}
}

func orNone(s string) string {
	if s == "" {
		return "(no write)"
	}
	return s
}

func eywaStatus(ctx context.Context, cfg Config, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("eywa status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: mora eywa status [--json]")
	}
	st, err := loadEywaPushState(cfg)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for id, rec := range st.Records {
		if rec.WriteID != "" {
			ids[id] = true
		}
	}
	receipt := eywaPushReceipt{Updates: []eywaPushRow{}}
	var pollErr error
	if len(ids) > 0 {
		needs := false
		for id := range ids {
			if !eywa.FinalStatus(st.Records[id].Status) {
				needs = true
			}
		}
		if needs {
			s, err := loadEywaSource(cfg)
			if err != nil {
				return err
			}
			client, err := eywaClientFor(s)
			if err != nil {
				return err
			}
			// One pass, no waiting.
			_, pollErr = pollEywaStatuses(ctx, cfg, client, &st, ids, 0)
		}
		for id := range ids {
			rec := st.Records[id]
			receipt.Updates = append(receipt.Updates, eywaPushRow{MemoryID: id, WriteID: rec.WriteID, Status: rec.Status, Reason: rec.Reason})
			receipt.Sent++
			if !eywa.FinalStatus(rec.Status) {
				receipt.Pending++
			}
		}
	}
	sort.Slice(receipt.Updates, func(i, j int) bool { return receipt.Updates[i].MemoryID < receipt.Updates[j].MemoryID })
	if *jsonOut {
		if err := emitReceipt(stdout, "mora.eywa.status", 1, receipt); err != nil {
			return err
		}
	} else if len(receipt.Updates) == 0 {
		fmt.Fprintln(stdout, "No updates have been pushed.")
	} else {
		printEywaRows(stdout, receipt.Updates)
	}
	_ = stderr
	return pollErr
}
