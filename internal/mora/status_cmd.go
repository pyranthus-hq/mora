package mora

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	hookspkg "github.com/pyranthus-hq/mora/internal/hooks"
	"github.com/pyranthus-hq/mora/internal/memory"
)

const (
	statusUsage       = "usage: mora status [--json] | mora status --web [--port N]"
	statusDefaultPort = 7781
	statusRecentGiven = 10
	statusProjectRows = 15
	statusReviewRows  = 25
	statusTitleRunes  = 100
	statusSnippetRune = 280
)

// statusReport is everything `mora status` shows. It is read-only: building it
// never writes the vault, the index or the state dir.
type statusReport struct {
	Folder      statusFolder    `json:"folder"`
	Vault       statusVault     `json:"vault"`
	MCP         statusMCP       `json:"mcp"`
	Review      statusReview    `json:"review"`
	LastGiven   *statusGiven    `json:"last_given,omitempty"`
	RecentGiven []statusGiven   `json:"recent_given,omitempty"`
	Project     []statusProjRow `json:"project_memories,omitempty"`
	// Errors lists every part that could not be read. A part in this list is
	// unknown, not empty: its zero value must not be read as "nothing there".
	Errors []statusError `json:"errors,omitempty"`
}

type statusError struct {
	Part  string `json:"part"`
	Error string `json:"error"`
}

type statusFolder struct {
	Cwd   string `json:"cwd"`
	Bound bool   `json:"bound"`
	Scope string `json:"scope,omitempty"`
	Root  string `json:"root,omitempty"`
}

type statusVault struct {
	Dir        string         `json:"dir"`
	Memories   int            `json:"memories"`
	Health     string         `json:"health"`
	Banner     string         `json:"banner,omitempty"`
	IndexState string         `json:"index_state"`
	IndexedAt  string         `json:"indexed_at,omitempty"`
	PendingOps int            `json:"pending_ops"`
	Sources    []statusSource `json:"sources"`
}

type statusSource struct {
	Key           string `json:"key"`
	State         string `json:"state"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
}

type statusReview struct {
	Saved int             `json:"saved"`
	Held  int             `json:"held"`
	Notes []statusRevNote `json:"notes,omitempty"`
}

type statusRevNote struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Scope   string   `json:"scope"`
	Type    string   `json:"type"`
	Title   string   `json:"title"`
	Snippet string   `json:"snippet,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
}

type statusGiven struct {
	At      string            `json:"at"`
	Surface string            `json:"surface"`
	Caller  string            `json:"caller"`
	Dir     string            `json:"dir,omitempty"`
	Count   *int              `json:"count,omitempty"`
	Written *bool             `json:"written,omitempty"`
	Notes   []statusGivenNote `json:"notes"`
}

type statusGivenNote struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

type statusProjRow struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at,omitempty"`
	Tentative bool   `json:"tentative,omitempty"`
}

func statusClip(s string, n int) string { return hookspkg.ClipRunes(flatProjectBrief(s), n) }

// buildStatus gathers the report. Each part is best effort: a part that cannot
// be read leaves its zero value and never hides the others. recent is how many
// given lines to include (the terminal shows one); withProject adds the bound
// project's notes and the review notes' text previews for the web page.
func buildStatus(ctx context.Context, cfg Config, cwd string, now time.Time, recent int, rich bool) statusReport {
	r := statusReport{Folder: statusFolder{Cwd: cwd}}
	r.MCP = statusMCPReport(ctx, cwd)
	r.Vault.Dir = cfg.VaultDir
	r.Vault.Sources = []statusSource{}
	fail := func(part string, err error) {
		r.Errors = append(r.Errors, statusError{Part: part, Error: err.Error()})
	}
	if files, err := allMemoryFiles(cfg); err == nil {
		r.Vault.Memories = len(files)
	} else {
		fail("vault", err)
	}
	h := healthOf(cfg, now)
	r.Vault.Health = h.State
	r.Vault.Banner = healthBannerFrom(h)
	r.Vault.IndexState, r.Vault.IndexedAt, r.Vault.PendingOps = h.Index.State, h.Index.IndexedAt, h.Index.PendingOps
	for _, s := range h.Sources {
		r.Vault.Sources = append(r.Vault.Sources, statusSource{Key: s.Key, State: s.State, LastSuccessAt: s.LastSuccessAt})
	}
	if scope, root, found, err := lookupProject(cfg, cwd); err != nil {
		fail("folder", err)
	} else if found {
		r.Folder.Bound, r.Folder.Scope, r.Folder.Root = true, scope, root
	}
	if notes, err := listReviewNotes(cfg, io.Discard); err != nil {
		fail("review", err)
	} else {
		for _, n := range notes {
			if n.Kind == reviewKindHeld {
				r.Review.Held++
			} else {
				r.Review.Saved++
			}
			if rich && len(r.Review.Notes) < statusReviewRows {
				r.Review.Notes = append(r.Review.Notes, statusRevNote{
					ID: n.ID, Kind: n.Kind, Scope: n.Scope, Type: n.Type,
					Title: statusClip(n.Title, statusTitleRunes), Snippet: statusClip(n.Text, statusSnippetRune), Reasons: n.Reasons,
				})
			}
		}
	}
	r.RecentGiven = statusGivenLines(cfg, max(recent, 1))
	if len(r.RecentGiven) > 0 {
		first := r.RecentGiven[0]
		r.LastGiven = &first
	}
	if recent <= 1 {
		r.RecentGiven = nil
	}
	if rich && r.Folder.Bound {
		if mems, err := listMemories(cfg, r.Folder.Scope, statusProjectRows); err != nil {
			fail("project notes", err)
		} else {
			for _, m := range mems {
				r.Project = append(r.Project, statusProjRow{ID: m.ID, Type: m.Type, Title: statusClip(reviewTitle(m), statusTitleRunes), CreatedAt: m.CreatedAt, Tentative: memory.IsTentative(m)})
			}
		}
	}
	return r
}

// statusGivenLines returns the newest n given lines, newest first, with each
// note's title read from the vault now.
func statusGivenLines(cfg Config, n int) []statusGiven {
	lines := readGivenLines(cfg)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	titles := givenTitles(cfg, givenIDsOf(lines))
	out := make([]statusGiven, 0, len(lines))
	for i := len(lines) - 1; i >= 0; i-- {
		var g givenLine
		if json.Unmarshal(lines[i], &g) != nil {
			continue
		}
		row := statusGiven{At: g.At, Surface: g.Surface, Caller: g.Caller, Dir: g.Dir, Count: g.Count, Written: g.Written, Notes: []statusGivenNote{}}
		for _, id := range g.IDs {
			row.Notes = append(row.Notes, statusGivenNote{ID: id, Title: titles[id]})
		}
		out = append(out, row)
	}
	return out
}

func cmdStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("mora status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	web := fs.Bool("web", false, "serve a read-only status page on 127.0.0.1")
	port := fs.Int("port", statusDefaultPort, "port for --web")
	if err := fs.Parse(args); err != nil {
		return errors.New(statusUsage)
	}
	portSet := false
	fs.Visit(func(f *flag.Flag) { portSet = portSet || f.Name == "port" })
	if fs.NArg() != 0 || (*web && *jsonOut) || (portSet && !*web) || *port < 0 || *port > 65535 {
		return errors.New(statusUsage)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if *web {
		return serveStatusWeb(ctx, cfg, cwd, *port, stdout, stderr)
	}
	report := buildStatus(ctx, cfg, cwd, hookNow(), 1, false)
	if *jsonOut {
		return emitReceipt(stdout, "mora.status", 1, report)
	}
	printStatus(stdout, report)
	return nil
}

// ReviewFailed is for the status page template.
func (r statusReport) ReviewFailed() bool { return r.statusFailed("review") }

// statusFailed reports whether the named part could not be read.
func (r statusReport) statusFailed(part string) bool {
	for _, e := range r.Errors {
		if e.Part == part {
			return true
		}
	}
	return false
}

func printStatus(w io.Writer, r statusReport) {
	if r.Folder.Bound {
		fmt.Fprintf(w, "Folder:  bound to %s (%s)\n", rulesShow(clipProjectScope(r.Folder.Scope)), rulesShow(r.Folder.Root))
	} else {
		fmt.Fprintf(w, "Folder:  not bound to a project (%s)\n", rulesShow(r.Folder.Cwd))
	}
	fmt.Fprintf(w, "Vault:   %s: %d memory files\n", rulesShow(r.Vault.Dir), r.Vault.Memories)
	index := "index " + r.Vault.IndexState
	if r.Vault.IndexedAt != "" {
		index += ", indexed " + r.Vault.IndexedAt
	}
	if r.Vault.PendingOps > 0 {
		index += fmt.Sprintf(", %d write(s) waiting to be indexed", r.Vault.PendingOps)
	}
	fmt.Fprintf(w, "Health:  %s; %s\n", r.Vault.Health, index)
	if r.Vault.Banner != "" {
		fmt.Fprintf(w, "         %s\n", rulesShow(r.Vault.Banner))
	}
	for _, s := range r.Vault.Sources {
		last := "never synced"
		if s.LastSuccessAt != "" {
			last = "last synced " + s.LastSuccessAt
		}
		fmt.Fprintf(w, "Source:  %s: %s, %s\n", rulesShow(s.Key), s.State, last)
	}
	fmt.Fprintf(w, "MCP:     %s: %d tools; instructions %s, catalog %s, combined %s\n", r.MCP.Surface, r.MCP.ToolCount, r.MCP.Instructions.text(), r.MCP.Catalog.text(), r.MCP.Combined.text())
	fmt.Fprintf(w, "         %s\n", r.MCP.Caveat)
	for _, e := range r.Errors {
		fmt.Fprintf(w, "Error:   could not read %s: %s\n", rulesShow(e.Part), rulesShow(e.Error))
	}
	switch {
	case r.statusFailed("review"):
		fmt.Fprintln(w, "Review:  unknown, the review notes could not be read (see the error above)")
	case r.Review.Saved+r.Review.Held == 0:
		fmt.Fprintln(w, "Review:  nothing waits for review")
	default:
		fmt.Fprintf(w, "Review:  %d saved (tentative), %d held (proposals) wait for you: run `mora review`\n", r.Review.Saved, r.Review.Held)
	}
	if r.LastGiven == nil {
		fmt.Fprintln(w, "Given:   nothing handed to an agent yet")
		return
	}
	g := r.LastGiven
	count := "an unknown number of notes"
	if g.Count != nil {
		count = fmt.Sprintf("%d notes", *g.Count)
	}
	fmt.Fprintf(w, "Given:   %s: %s to %s, %s (`mora given` for more)\n", g.At, rulesShow(g.Surface), rulesShow(g.Caller), count)
	for _, n := range g.Notes {
		if n.Title == "" {
			fmt.Fprintf(w, "           %s\n", rulesShow(n.ID))
		} else {
			fmt.Fprintf(w, "           %s  %s\n", rulesShow(n.ID), rulesShow(n.Title))
		}
	}
}

// statusHostOK accepts only a loopback Host header. It is what stops a web page
// the person visits from reading this page through a DNS name it controls.
func statusHostOK(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = strings.Trim(host, "[]")
	}
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

// newStatusHandler serves the report as one HTML page and as JSON. It has no
// write endpoint: keeping or rejecting an agent note stays in `mora review`,
// in the owner's own terminal.
func newStatusHandler(ctx context.Context, cfg Config, cwd string, now func() time.Time, token string) http.Handler {
	mux := http.NewServeMux()
	serve := func(w http.ResponseWriter, r *http.Request, render func(statusReport) error, contentType string) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", contentType)
		if r.Method == http.MethodHead {
			return
		}
		if err := render(buildStatus(ctx, cfg, cwd, now(), statusRecentGiven, true)); err != nil {
			http.Error(w, "status unavailable", http.StatusInternalServerError)
		}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serve(w, r, func(rep statusReport) error { return statusPage.Execute(w, rep) }, "text/html; charset=utf-8")
	})
	mux.HandleFunc("/status.json", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, func(rep statusReport) error { return emitReceipt(w, "mora.status", 1, rep) }, "application/json")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !statusHostOK(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		// The page lists note titles and snippets, so it is served only under a
		// per-run random path segment, like the bearer token of `mora serve http`
		// (32 random bytes, constant-time compare). Anything else is a plain 404.
		seg, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if token == "" || subtle.ConstantTimeCompare([]byte(seg), []byte(token)) != 1 {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + rest
		mux.ServeHTTP(w, r2)
	})
}

// newStatusToken is 32 random bytes in hex, the size serve http uses.
func newStatusToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func serveStatusWeb(ctx context.Context, cfg Config, cwd string, port int, stdout, stderr io.Writer) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("cannot listen on 127.0.0.1:%d: %w (pick another with --port)", port, err)
	}
	defer ln.Close()
	token, err := newStatusToken()
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: newStatusHandler(ctx, cfg, cwd, hookNow, token), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	u := url.URL{Scheme: "http", Host: ln.Addr().String(), Path: "/" + token + "/"}
	fmt.Fprintf(stdout, "Mora status page (read-only, this machine only): %s\nPress Ctrl+C to stop.\n", u.String())
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		fmt.Fprintln(stderr, "status page stopped")
		return nil
	}
}

// statusPage escapes every value: note titles and text come from agents and
// from email, so none of it is ever trusted as markup.
var statusPage = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="15">
<title>Mora status</title>
<style>
:root{color-scheme:light dark;--bg:#fff;--fg:#1c1f24;--mut:#667;--card:#f4f5f7;--line:#d8dbe0;--ok:#127a3d;--warn:#a15c00;--bad:#b3261e}
@media (prefers-color-scheme:dark){:root{--bg:#14161a;--fg:#e6e8eb;--mut:#9aa1ab;--card:#1d2026;--line:#2e333b;--ok:#5bd68a;--warn:#f0b429;--bad:#ff8a80}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,sans-serif}
main{max-width:860px;margin:0 auto;padding:16px}
h1{font-size:20px;margin:8px 0 16px}h2{font-size:15px;margin:0 0 8px}
.card{background:var(--card);border:1px solid var(--line);border-radius:8px;padding:12px 14px;margin-bottom:12px}
.mut{color:var(--mut)}.ok{color:var(--ok)}.warn{color:var(--warn)}.bad{color:var(--bad)}
code{font:13px ui-monospace,monospace;word-break:break-all}
ul{margin:0;padding-left:18px}li{margin:4px 0}
.note{border-top:1px solid var(--line);padding:8px 0}.note:first-of-type{border-top:0}
</style></head><body><main>
<h1>Mora status <span class="mut">(read-only)</span></h1>
{{if .Errors}}<section class="card"><h2 class="bad">Could not read</h2><ul>{{range .Errors}}<li><b>{{.Part}}</b>: {{.Error}}</li>{{end}}</ul></section>{{end}}

<section class="card"><h2>This folder</h2>
{{if .Folder.Bound}}Bound to <b>{{.Folder.Scope}}</b> <span class="mut"><code>{{.Folder.Root}}</code></span>
{{else}}Not bound to a project. <span class="mut"><code>{{.Folder.Cwd}}</code></span>{{end}}</section>

<section class="card"><h2>Vault and index</h2>
<div><code>{{.Vault.Dir}}</code> · {{.Vault.Memories}} memory files</div>
<div>Health: <b class="{{if eq .Vault.Health "healthy"}}ok{{else if eq .Vault.Health "degraded"}}warn{{else}}bad{{end}}">{{.Vault.Health}}</b>
 · index <b>{{.Vault.IndexState}}</b>{{if .Vault.IndexedAt}} <span class="mut">(indexed {{.Vault.IndexedAt}})</span>{{end}}{{if .Vault.PendingOps}} · {{.Vault.PendingOps}} write(s) waiting to be indexed{{end}}</div>
{{if .Vault.Banner}}<div class="warn">{{.Vault.Banner}}</div>{{end}}
{{if .Vault.Sources}}<ul>{{range .Vault.Sources}}<li><code>{{.Key}}</code>: {{.State}} <span class="mut">{{if .LastSuccessAt}}last synced {{.LastSuccessAt}}{{else}}never synced{{end}}</span></li>{{end}}</ul>{{end}}</section>

<section class="card"><h2>Waiting for your review</h2>
{{if or .Review.Saved .Review.Held}}<div>{{.Review.Saved}} saved (tentative), {{.Review.Held}} held (proposals). To keep or reject them run <code>mora review</code> in your own terminal; this page cannot change anything.</div>
{{range .Review.Notes}}<div class="note"><b>{{.Title}}</b> <span class="mut">{{.Kind}} · {{.Scope}} · {{.Type}} · <code>{{.ID}}</code></span>
{{if .Reasons}}<div class="warn">held because: {{range $i, $r := .Reasons}}{{if $i}}, {{end}}{{$r}}{{end}}</div>{{end}}
{{if .Snippet}}<div class="mut">{{.Snippet}}</div>{{end}}</div>{{end}}
{{else if .ReviewFailed}}<span class="bad">Unknown: the review notes could not be read (see the errors above).</span>
{{else}}Nothing waits for review.{{end}}</section>

<section class="card"><h2>Recently handed to agents</h2>
{{if .RecentGiven}}{{range .RecentGiven}}<div class="note"><b>{{.Surface}}</b> to {{.Caller}} <span class="mut">{{.At}}{{if .Count}} · {{.Count}} notes{{end}}{{if .Dir}} · <code>{{.Dir}}</code>{{end}}</span>
{{if .Notes}}<ul>{{range .Notes}}<li>{{if .Title}}{{.Title}} {{end}}<span class="mut"><code>{{.ID}}</code></span></li>{{end}}</ul>{{end}}</div>{{end}}
{{else}}Nothing handed to an agent yet.{{end}}</section>

{{if .Folder.Bound}}<section class="card"><h2>Notes in {{.Folder.Scope}}</h2>
{{if .Project}}<ul>{{range .Project}}<li>{{.Title}} <span class="mut">{{.Type}}{{if .Tentative}} · tentative{{end}} · {{.CreatedAt}} · <code>{{.ID}}</code></span></li>{{end}}</ul>
{{else}}No notes in this project yet.{{end}}</section>{{end}}
</main></body></html>
`))
