package mora

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	givenLogName     = "given.log"
	givenLogMaxBytes = 2 << 20
	givenLineMax     = 2000
	givenMaxIDs      = 20
	givenCallerMax   = 40
	givenUsage       = "usage: mora given [--limit N] [--json]"
)

// givenLine is one hand-over: Mora gave these notes to an agent. It holds ids
// and counts only, never a prompt, a query, a title or note text. "Given" is
// not "used": Mora cannot know whether the agent read the notes.
type givenLine struct {
	At      string   `json:"at"`
	Surface string   `json:"surface"`
	Kind    string   `json:"kind"`
	Caller  string   `json:"caller"`
	Dir     string   `json:"dir"`
	Scope   string   `json:"scope"`
	Count   int      `json:"count"`
	IDs     []string `json:"ids"`
	More    int      `json:"more"`
	Written bool     `json:"written"`
}

// The log is the owner's local record. It is independent of `mora usage off`
// and never leaves the machine.
func givenLogPath(cfg Config) string { return filepath.Join(cfg.StateDir, givenLogName) }

// givenCaller keeps a caller to lower case [a-z0-9._-], at most 40 characters.
// A transport label with no client name, or a name with nothing usable, is
// "unknown".
func givenCaller(raw string) string {
	raw = strings.TrimPrefix(raw, "mcp-stdio/")
	if raw == "mcp-stdio" || raw == "mcp" {
		return "unknown"
	}
	c := sanitizeCallerToken(strings.ToLower(raw))
	if len(c) > givenCallerMax {
		c = c[:givenCallerMax]
	}
	if c == "" {
		return "unknown"
	}
	return c
}

// givenScope keeps only a project scope; any other scope is not logged.
func givenScope(s string) string {
	if validProjectScope(s) != nil {
		return ""
	}
	return s
}

// appendGiven writes one line. A failure is returned for tests; callers
// ignore it, because the log must never change a hook or tool result.
func appendGiven(cfg Config, g givenLine, ids []string, count int) error {
	g.At = hookNow().UTC().Format(time.RFC3339)
	g.Caller = givenCaller(g.Caller)
	g.Count = count
	if len(ids) > givenMaxIDs {
		ids = ids[:givenMaxIDs]
	}
	g.IDs = append([]string{}, ids...)
	var line []byte
	for {
		g.More = count - len(g.IDs)
		if g.More < 0 {
			g.More = 0
		}
		var err error
		if line, err = json.Marshal(g); err != nil {
			return err
		}
		if len(line) < givenLineMax { // the newline makes it at most 2,000 bytes
			break
		}
		switch {
		case len(g.IDs) > 0:
			g.IDs = g.IDs[:len(g.IDs)-1]
		case len(g.Dir) > 0:
			g.Dir = clipUTF8(g.Dir, len(g.Dir)/2)
		default:
			return errors.New("given line too long")
		}
	}
	line = append(line, '\n')
	path := givenLogPath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// shortcut: two processes can both rotate at once and drop one old file; fine for a 2 MB local log.
	if info, err := os.Stat(path); err == nil && info.Size() > givenLogMaxBytes {
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(line)
	return errors.Join(werr, f.Close())
}

func clipUTF8(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// givenFromMCP returns what a note-returning MCP tool really handed over: the
// ids in the payload after budget trimming. ok is false for tools that return
// no notes. context_memory renders its notes into one text block under a
// budget, so it reports the count the tool reports and no ids.
func givenFromMCP(name string, args map[string]any, inv mcpToolInvocation) (ids []string, count int, ok bool) {
	type row struct {
		ID       string `json:"id"`
		StableID string `json:"stable_id"`
	}
	rowID := func(r row) string {
		if r.ID != "" {
			return r.ID
		}
		return r.StableID
	}
	switch name {
	case "context_memory":
		return nil, inv.trace.event.Results, true
	case "brief":
		res, isProject := inv.value.(projectBriefResult)
		if strArg(args, "project_dir", "") == "" || !isProject {
			return nil, 0, false
		}
		return res.IDs, len(res.IDs), true
	case "search_memory", "list_memory", "read_memory", "think":
	default:
		return nil, 0, false
	}
	raw, err := json.Marshal(inv.value)
	if err != nil {
		return nil, 0, false
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil, 0, false
	}
	var rows []row
	switch name {
	case "search_memory":
		_ = json.Unmarshal(top["results"], &rows)
	case "list_memory":
		_ = json.Unmarshal(top["memories"], &rows)
	case "read_memory":
		var one row
		if json.Unmarshal(top["memory"], &one) == nil && rowID(one) != "" {
			rows = []row{one}
		}
	case "think":
		var t struct {
			Evidence []row `json:"evidence"`
		}
		_ = json.Unmarshal(top["think"], &t)
		rows = t.Evidence
	}
	for _, r := range rows {
		if id := rowID(r); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, len(rows), true
}

// logGivenMCP records one MCP hand-over after the tool returned without error.
func logGivenMCP(ctx context.Context, name string, args map[string]any, inv mcpToolInvocation) {
	if inv.err != nil {
		return
	}
	ids, count, ok := givenFromMCP(name, args, inv)
	if !ok || count == 0 {
		return
	}
	g := givenLine{Surface: "mcp:" + name, Kind: name, Caller: agentCallerFrom(ctx), Dir: strArg(args, "project_dir", ""), Written: true}
	if name == "brief" {
		g.Scope = givenScope(inv.value.(projectBriefResult).Scope)
	} else {
		g.Scope = givenScope(strArg(args, "scope", ""))
	}
	_ = appendGiven(inv.cfg, g, ids, count)
}

// readGivenLines returns the valid log lines, oldest first, from given.log.1
// then given.log. A missing file is an empty log.
func readGivenLines(cfg Config) []json.RawMessage {
	var out []json.RawMessage
	for _, path := range []string{givenLogPath(cfg) + ".1", givenLogPath(cfg)} {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			var g givenLine
			if json.Unmarshal(line, &g) == nil && g.At != "" {
				out = append(out, append(json.RawMessage{}, line...))
			}
		}
	}
	return out
}

func lastGiven(cfg Config) *doctorLastGiven {
	lines := readGivenLines(cfg)
	if len(lines) == 0 {
		return nil
	}
	var g givenLine
	if json.Unmarshal(lines[len(lines)-1], &g) != nil {
		return nil
	}
	return &doctorLastGiven{At: g.At, Surface: g.Surface, Count: g.Count}
}

func cmdGiven(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("mora given", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("limit", 10, "number of lines")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return errors.New(givenUsage)
	}
	if fs.NArg() != 0 || *limit < 1 {
		return errors.New(givenUsage)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	lines := readGivenLines(cfg)
	if len(lines) > *limit {
		lines = lines[len(lines)-*limit:]
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	if *jsonOut {
		if lines == nil {
			lines = []json.RawMessage{}
		}
		return json.NewEncoder(stdout).Encode(lines)
	}
	if len(lines) == 0 {
		_, err := fmt.Fprintln(stdout, "Nothing given yet.")
		return err
	}
	for _, raw := range lines {
		var g givenLine
		if json.Unmarshal(raw, &g) != nil {
			continue
		}
		folder := g.Dir
		if folder == "" {
			folder = "-"
		}
		ids := "none"
		if len(g.IDs) > 0 {
			ids = strings.Join(g.IDs, ", ")
			if g.More > 0 {
				ids += fmt.Sprintf(" (+%d more)", g.More)
			}
		}
		if _, err := fmt.Fprintf(stdout, "%s  %s  %s  %s  %d notes  ids: %s\n", g.At, g.Surface, g.Caller, folder, g.Count, ids); err != nil {
			return err
		}
	}
	return nil
}
