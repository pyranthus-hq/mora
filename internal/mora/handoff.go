package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/memoryfile"

	hookspkg "github.com/pyranthus-hq/mora/internal/hooks"
)

const (
	handoffMaxRequests  = 5
	handoffRequestRunes = 120
	handoffTailBytes    = 4 << 20
	handoffInputBytes   = 1 << 20
	handoffMaxAge       = 14 * 24 * time.Hour
	handoffTool         = "claude-code"
)

// handoffFile is the last session of one bound folder: the user's own last
// requests, shortened. It holds no model output and no reply text.
type handoffFile struct {
	At       string   `json:"at"`
	Tool     string   `json:"tool"`
	Scope    string   `json:"scope"`
	Root     string   `json:"root"`
	Requests []string `json:"requests"`
}

type sessionEndHookInput struct {
	CWD            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
}

func handoffPath(cfg Config, scope string) string {
	return filepath.Join(cfg.StateDir, "handoff", osSafeBase(memory.SafeFilename(scope))+".json")
}

// openRegularNoFollow opens path read-only without following a symlink or
// blocking on a FIFO. It returns nil unless the opened file is a regular file.
// It never changes the file mode: the transcript belongs to the user.
func openRegularNoFollow(path string) (*os.File, os.FileInfo) {
	// O_NOFOLLOW is zero on Windows, so refuse a symlink before opening.
	if li, err := os.Lstat(path); err != nil || !li.Mode().IsRegular() {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|givenOpenFlags, 0)
	if err != nil {
		return nil, nil
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil
	}
	return f, info
}

// handoffClean applies the checks Mora runs on text an agent writes (credential
// and agent-aimed) to the FULL request. A request that fails is dropped, not
// shortened. Only then are line breaks flattened and the text cut.
func handoffClean(req string) (string, bool) {
	if hasInvisibleUnicode(req) {
		return "", false
	}
	flat := flatProjectBrief(req)
	if flat == "" {
		return "", false
	}
	for _, text := range []string{req, flat} {
		if _, redacted := redactProposalText(text); redacted {
			return "", false
		}
	}
	for _, p := range tentativeHoldPatterns {
		if p.reason == "aimed at agents" && p.pattern.MatchString(flat) {
			return "", false
		}
	}
	return hookspkg.ClipRunes(flat, handoffRequestRunes), true
}

// handoffTranscriptOK accepts a .jsonl file that, with every symlink resolved,
// lies inside <home>/.claude/projects: Claude's own transcript folder.
func handoffTranscriptOK(cfg Config, path string) bool {
	home := cfg.HomeDir()
	if home == "" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && filepath.Ext(resolved) == ".jsonl" &&
		hookWithinProject(filepath.Join(home, ".claude", "projects"), resolved)
}

// transcriptRequests returns the last n requests the user typed, oldest first,
// from the tail of a Claude Code transcript. Only complete lines count.
func transcriptRequests(path, root string, n int) ([]string, bool) {
	lines, ok := transcriptTailLines(path)
	if !ok {
		return nil, false
	}
	var reqs []string
	inRoot := map[string]bool{}
	for _, line := range lines {
		if text, cwd, ok := typedRequest(line); ok {
			in, seen := inRoot[cwd]
			if !seen {
				in = hookWithinProject(root, cwd)
				inRoot[cwd] = in
			}
			if !in {
				continue
			}
			reqs = append(reqs, text)
		}
	}
	if len(reqs) > n {
		reqs = reqs[len(reqs)-n:]
	}
	return reqs, true
}

// transcriptTailLines returns the complete lines of the last handoffTailBytes of
// a regular, non-symlink transcript file.
func transcriptTailLines(path string) ([][]byte, bool) {
	f, info := openRegularNoFollow(path)
	if f == nil {
		return nil, false
	}
	defer f.Close()
	start := int64(0)
	if info.Size() > handoffTailBytes {
		start = info.Size() - handoffTailBytes
	}
	body, err := io.ReadAll(io.NewSectionReader(f, start, handoffTailBytes))
	if err != nil {
		return nil, false
	}
	if start > 0 { // the first line of a tail read is cut
		if i := bytes.IndexByte(body, '\n'); i >= 0 {
			body = body[i+1:]
		} else {
			body = nil
		}
	}
	if i := bytes.LastIndexByte(body, '\n'); i >= 0 {
		body = body[:i]
	} else {
		body = nil
	}
	return bytes.Split(body, []byte{'\n'}), true
}

// typedRequest reports the text of a transcript line the user typed. Tool
// results, meta and sidechain lines, and command or reminder wrappers (text
// that starts with "<") are not requests.
func typedRequest(line []byte) (text, cwd string, ok bool) {
	var l struct {
		Type        string `json:"type"`
		IsMeta      bool   `json:"isMeta"`
		IsSidechain bool   `json:"isSidechain"`
		CWD         string `json:"cwd"`
		Message     struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type != "user" || l.IsMeta || l.IsSidechain {
		return "", "", false
	}
	if json.Unmarshal(l.Message.Content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			return "", "", false
		}
		var parts []string
		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				return "", "", false
			case "text":
				parts = append(parts, b.Text)
			}
		}
		text = strings.Join(parts, " ")
	}
	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(text, "<") {
		return "", "", false
	}
	return text, l.CWD, true
}

// hookSessionEnd keeps the user's last requests of a session that ends in a
// bound folder. It makes no model call, writes nothing to stdout and never
// fails the session: every error path returns nil.
func hookSessionEnd(ctx context.Context, args []string, stdin io.Reader) error {
	projectDir, ok := parseProjectDirArg("session-end", args)
	if !ok {
		return nil
	}
	var in sessionEndHookInput
	if json.NewDecoder(io.LimitReader(stdin, handoffInputBytes)).Decode(&in) != nil ||
		!filepath.IsAbs(in.CWD) || !filepath.IsAbs(in.TranscriptPath) {
		return nil
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return nil
	}
	if projectDir != "" && !hookProjectGate(cfg, projectDir, in.CWD) {
		return nil
	}
	scope, root, found, lerr := lookupProject(cfg, in.CWD)
	if lerr != nil || !found || !handoffTranscriptOK(cfg, in.TranscriptPath) {
		return nil
	}
	raw, ok := transcriptRequests(in.TranscriptPath, root, handoffMaxRequests)
	if !ok {
		return nil
	}
	path := handoffPath(cfg, scope)
	var kept []string
	for _, r := range raw {
		if c, ok := handoffClean(r); ok {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		_ = os.Remove(path)
		return nil
	}
	body, err := json.MarshalIndent(handoffFile{At: hookNow().UTC().Format(time.RFC3339), Tool: handoffTool, Scope: scope, Root: root, Requests: kept}, "", "  ")
	if err != nil {
		return nil
	}
	_ = atomicio.Write(path, append(body, '\n'), 0o600)
	return nil
}

// handoffBlock renders the handoff lines of a scope, or "" when there is none
// or it is older than 14 days. The file can be hand-edited and its text rides
// into a prompt, so every request goes through handoffClean again.
func handoffBlock(cfg Config, scope string, now time.Time) string {
	f, _ := openRegularNoFollow(handoffPath(cfg, scope))
	if f == nil {
		return ""
	}
	defer f.Close()
	var h handoffFile
	if json.NewDecoder(io.LimitReader(f, handoffInputBytes)).Decode(&h) != nil || h.Scope != scope || h.Tool != handoffTool {
		return ""
	}
	at, err := time.Parse(time.RFC3339, h.At)
	if err != nil || now.Sub(at) > handoffMaxAge {
		return ""
	}
	if len(h.Requests) > handoffMaxRequests {
		h.Requests = h.Requests[len(h.Requests)-handoffMaxRequests:]
	}
	out := []string{fmt.Sprintf("Recent requests in this folder, outcomes unknown (%s, %s):", at.UTC().Format("2006-01-02"), handoffTool)}
	for _, r := range h.Requests {
		if c, ok := handoffClean(r); ok {
			out = append(out, `- "`+c+`"`)
		}
	}
	if len(out) == 1 {
		return ""
	}
	return strings.Join(out, "\n")
}

const handoffUsage = "usage: mora handoff [show [--scope S] [--project DIR] [--json] | list [--scope S] [--project DIR] [--limit N] [--json] | write [--scope S] [--project DIR] --title TITLE [--direction D] [--why W] [--turns T] [--where W] [--next N] [--text TEXT] [--short] [--mine] [--json]]"

// cmdHandoff implements `mora handoff [show|list|write]` writing to os.Stdout/os.Stderr.
func cmdHandoff(cfg Config, args []string) error {
	return cmdHandoffIO(context.Background(), cfg, args, os.Stdin, os.Stdout, os.Stderr)
}

// cmdHandoffCLI is the entry point the Run dispatch calls: it resolves the
// config the way the other verbs do, then runs cmdHandoffIO on the given writers.
func cmdHandoffCLI(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	return cmdHandoffIO(ctx, cfg, args, stdin, stdout, stderr)
}

func cmdHandoffIO(ctx context.Context, cfg Config, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	sub := "show"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "show":
		return cmdHandoffShow(cfg, args, stdout)
	case "list":
		return cmdHandoffList(cfg, args, stdout)
	case "write":
		return cmdHandoffWrite(ctx, cfg, args, stdin, stdout, stderr)
	default:
		return fmt.Errorf("unknown handoff command %q; %s", sub, handoffUsage)
	}
}

func resolveHandoffScope(cfg Config, explicitScope, projectDir string, defaultGlobal bool) (string, error) {
	if explicitScope != "" {
		if err := memoryfile.ValidateWriteScope(explicitScope); err != nil {
			return "", err
		}
		return explicitScope, nil
	}
	if projectDir != "" {
		sc, _, found, err := lookupProject(cfg, projectDir)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("folder %q is not bound to a project; run `mora project bind --scope project:NAME %s`", projectDir, projectDir)
		}
		return sc, nil
	}
	if wd, err := os.Getwd(); err == nil {
		if sc, _, found, lerr := lookupProject(cfg, wd); lerr == nil && found {
			return sc, nil
		}
	}
	if defaultGlobal {
		return "global", nil
	}
	return "", nil
}

func listHandoffMemories(cfg Config, scope string, limit int, now time.Time) ([]Memory, error) {
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return nil, err
	}
	var all []Memory
	for _, path := range files {
		m, perr := parseMemory(path)
		if perr != nil || m.DeletedAt != "" || m.Type != memory.TypeHandoff {
			continue
		}
		if scope != "" && m.Scope != scope {
			continue
		}
		all = append(all, m)
	}
	all, err = currentMemories(cfg, suppressPendingDeletes(cfg, all), now)
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool {
		ai, _ := time.Parse(time.RFC3339, all[i].CreatedAt)
		aj, _ := time.Parse(time.RFC3339, all[j].CreatedAt)
		if !ai.Equal(aj) {
			return ai.After(aj)
		}
		return all[i].ID > all[j].ID
	})
	// When unbound (scope == ""), prefer project:* scopes if any exist.
	if scope == "" {
		var projOnly []Memory
		for _, m := range all {
			if strings.HasPrefix(m.Scope, "project:") {
				projOnly = append(projOnly, m)
			}
		}
		if len(projOnly) > 0 {
			all = projOnly
		}
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func loadHandoffFallback(cfg Config, scope string, now time.Time) *handoffFile {
	if scope == "" {
		return nil
	}
	f, _ := openRegularNoFollow(handoffPath(cfg, scope))
	if f == nil {
		return nil
	}
	defer f.Close()
	var h handoffFile
	if json.NewDecoder(io.LimitReader(f, handoffInputBytes)).Decode(&h) != nil || h.Scope != scope || h.Tool != handoffTool {
		return nil
	}
	at, err := time.Parse(time.RFC3339, h.At)
	if err != nil || now.Sub(at) > handoffMaxAge {
		return nil
	}
	if len(h.Requests) > handoffMaxRequests {
		h.Requests = h.Requests[len(h.Requests)-handoffMaxRequests:]
	}
	var cleaned []string
	for _, r := range h.Requests {
		if c, ok := handoffClean(r); ok {
			cleaned = append(cleaned, c)
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	h.Requests = cleaned
	return &h
}

type handoffShowPayload struct {
	Scope              string       `json:"scope,omitempty"`
	Memory             *Memory      `json:"memory,omitempty"`
	TranscriptFallback *handoffFile `json:"transcript_fallback,omitempty"`
}

func cmdHandoffShow(cfg Config, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("mora handoff show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scopeFlag := fs.String("scope", "", "scope")
	projectFlag := fs.String("project", "", "bound project folder")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New(handoffUsage)
	}
	scope, err := resolveHandoffScope(cfg, *scopeFlag, *projectFlag, false)
	if err != nil {
		return err
	}
	now := hookNow()
	mems, err := listHandoffMemories(cfg, scope, 1, now)
	if err != nil {
		return err
	}
	var latest *Memory
	if len(mems) > 0 {
		withProv := memory.WithProvenance(mems[0])
		latest = &withProv
		if scope == "" {
			scope = latest.Scope
		}
	}
	var fallback *handoffFile
	if fb := loadHandoffFallback(cfg, scope, now); fb != nil {
		if latest == nil {
			fallback = fb
		} else {
			fbAt, err1 := time.Parse(time.RFC3339, fb.At)
			memAt, err2 := time.Parse(time.RFC3339, latest.CreatedAt)
			if err1 == nil && (err2 != nil || fbAt.After(memAt)) {
				fallback = fb
			}
		}
	}
	if *jsonOut {
		return emitReceipt(stdout, "mora.handoff.show", 1, handoffShowPayload{
			Scope:              scope,
			Memory:             latest,
			TranscriptFallback: fallback,
		})
	}
	if latest == nil && fallback == nil {
		if scope != "" {
			_, err := fmt.Fprintf(stdout, "No handoff recorded for %s.\n", scope)
			return err
		}
		_, err := fmt.Fprintln(stdout, "No handoff recorded.")
		return err
	}
	var out []string
	if latest != nil {
		date := "undated"
		if len(latest.CreatedAt) >= 10 {
			date = latest.CreatedAt[:10]
		}
		out = append(out, fmt.Sprintf("Handoff [%s] %s (%s, %s)\n%s", date, latest.Title, latest.ID, latest.Scope, latest.Text))
	}
	if fallback != nil {
		if blk := handoffBlock(cfg, fallback.Scope, now); blk != "" {
			out = append(out, blk)
		}
	}
	_, err = fmt.Fprintln(stdout, strings.Join(out, "\n\n"))
	return err
}

func cmdHandoffList(cfg Config, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("mora handoff list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scopeFlag := fs.String("scope", "", "scope")
	projectFlag := fs.String("project", "", "bound project folder")
	limit := fs.Int("limit", 10, "max handoffs to list")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 || *limit < 1 {
		return errors.New(handoffUsage)
	}
	scope, err := resolveHandoffScope(cfg, *scopeFlag, *projectFlag, false)
	if err != nil {
		return err
	}
	items, err := listHandoffMemories(cfg, scope, *limit, hookNow())
	if err != nil {
		return err
	}
	if *jsonOut {
		items = withProvenance(items)
		return emitReceipt(stdout, "mora.handoff.list", 1, newMemoriesPayload(items))
	}
	if len(items) == 0 {
		_, err := fmt.Fprintln(stdout, "No handoff memories found.")
		return err
	}
	for _, m := range items {
		date := "undated"
		if len(m.CreatedAt) >= 10 {
			date = m.CreatedAt[:10]
		}
		if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", m.ID, date, m.Scope, m.Title); err != nil {
			return err
		}
	}
	return nil
}

func formatHandoffBody(direction, why, turns, where, next, rawText string, short bool) (string, error) {
	direction = strings.TrimSpace(direction)
	why = strings.TrimSpace(why)
	turns = strings.TrimSpace(turns)
	where = strings.TrimSpace(where)
	next = strings.TrimSpace(next)
	rawText = strings.TrimSpace(rawText)

	hasSections := direction != "" || why != "" || turns != "" || where != "" || next != ""
	if !hasSections {
		if rawText == "" {
			return "", errors.New("handoff write requires --text or structured handoff flags (--direction, --where, --next)")
		}
		return rawText, nil
	}
	var lines []string
	if short {
		if direction != "" {
			lines = append(lines, "Direction: "+direction)
		}
		if where != "" {
			lines = append(lines, "Where it stands: "+where)
		}
		if next != "" {
			lines = append(lines, "Next step: "+next)
		}
		if why != "" {
			lines = append(lines, "Why: "+why)
		}
		if turns != "" {
			lines = append(lines, "Turns: "+turns)
		}
	} else {
		if direction != "" {
			lines = append(lines, "Direction: "+direction)
		}
		if why != "" {
			lines = append(lines, "Why: "+why)
		}
		if turns != "" {
			lines = append(lines, "Turns: "+turns)
		}
		if where != "" {
			lines = append(lines, "Where it stands: "+where)
		}
		if next != "" {
			lines = append(lines, "Next step: "+next)
		}
	}
	if rawText != "" {
		lines = append(lines, rawText)
	}
	return strings.Join(lines, "\n"), nil
}

func cmdHandoffWrite(ctx context.Context, cfg Config, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("mora handoff write", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scopeFlag := fs.String("scope", "", "scope")
	projectFlag := fs.String("project", "", "bound project folder")
	title := fs.String("title", "", "title")
	direction := fs.String("direction", "", "1-line direction")
	why := fs.String("why", "", "why this approach was chosen")
	turns := fs.String("turns", "", "key turns or progress")
	progress := fs.String("progress", "", "alias for --turns")
	where := fs.String("where", "", "where the work stands now")
	next := fs.String("next", "", "next step")
	text := fs.String("text", "", "free-form handoff text")
	short := fs.Bool("short", false, "short 1-3 sentence handoff format")
	source := fs.String("source", "manual", "source")
	mine := fs.Bool("mine", false, "save as your own statement (owner-confirmed); needs your own terminal and a typed yes")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *mine {
		if err := requireMineTerminal(stdin, "mora handoff write"); err != nil {
			return err
		}
	}
	if *text == "" && fs.NArg() > 0 {
		*text = strings.Join(fs.Args(), " ")
	}
	if *turns == "" && *progress != "" {
		*turns = *progress
	}
	if strings.TrimSpace(*title) == "" {
		return errors.New("--title is required")
	}
	body, err := formatHandoffBody(*direction, *why, *turns, *where, *next, *text, *short)
	if err != nil {
		return err
	}
	scope, err := resolveHandoffScope(cfg, *scopeFlag, *projectFlag, true)
	if err != nil {
		return err
	}
	m := Memory{
		Scope:     scope,
		Type:      memory.TypeHandoff,
		Title:     strings.TrimSpace(*title),
		Source:    *source,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Text:      body,
	}
	var opts []createOption
	if *mine {
		if !askOwner(stderr, stdin, m, "Save this as your own statement?") {
			return errors.New("Not saved.")
		}
		opts = append(opts, withOwnerConfirmed())
	}
	m, op, err := createMemory(ctx, cfg, m, opts...)
	if err != nil {
		return err
	}
	done := func(indexed bool) error {
		if *jsonOut {
			return emitReceipt(stdout, "mora.write", 1, newWriteReceipt(m, indexed, *mine))
		}
		if err := emit(stdout, m, false); err != nil || !*mine {
			return err
		}
		_, err := fmt.Fprintln(stdout, "saved as owner-confirmed: agents read it as your own statement")
		return err
	}
	if err := indexUpsert(ctx, cfg, m); err != nil {
		if errors.Is(err, errRebuildBlocked) {
			fmt.Fprintf(stderr, "warning: memory saved but the search index was not updated (vault looks empty or unfamiliar); run `mora index rebuild --force` after checking vault_dir\n")
			return done(false)
		}
		return err
	}
	if authoredWriteProjectionPending(cfg, op) {
		if err := markFTSCommitted(cfg, op); err != nil {
			fmt.Fprintf(stderr, "warning: memory saved and searchable, but its index marker could not be updated: %v\n", err)
		}
	}
	if err := reconcileAuthoredWrites(ctx, cfg); err != nil {
		fmt.Fprintf(stderr, "warning: memory saved and text search updated, but full index reconciliation is pending: %v\n", err)
	}
	return done(true)
}
