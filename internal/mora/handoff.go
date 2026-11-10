package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/memory"

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

// transcriptRequests returns the last n requests the user typed, oldest first,
// from the tail of a Claude Code transcript. Only complete lines count.
func transcriptRequests(path string, n int) ([]string, bool) {
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
	var reqs []string
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		if text, ok := typedRequest(line); ok {
			reqs = append(reqs, text)
		}
	}
	if len(reqs) > n {
		reqs = reqs[len(reqs)-n:]
	}
	return reqs, true
}

// typedRequest reports the text of a transcript line the user typed. Tool
// results, meta and sidechain lines, and command or reminder wrappers (text
// that starts with "<") are not requests.
func typedRequest(line []byte) (string, bool) {
	var l struct {
		Type        string `json:"type"`
		IsMeta      bool   `json:"isMeta"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type != "user" || l.IsMeta || l.IsSidechain {
		return "", false
	}
	var text string
	if json.Unmarshal(l.Message.Content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			return "", false
		}
		var parts []string
		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				return "", false
			case "text":
				parts = append(parts, b.Text)
			}
		}
		text = strings.Join(parts, " ")
	}
	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(text, "<") {
		return "", false
	}
	return text, true
}

// hookSessionEnd keeps the user's last requests of a session that ends in a
// bound folder. It makes no model call, writes nothing to stdout and never
// fails the session: every error path returns nil.
func hookSessionEnd(ctx context.Context, stdin io.Reader) error {
	var in sessionEndHookInput
	if json.NewDecoder(io.LimitReader(stdin, handoffInputBytes)).Decode(&in) != nil ||
		!filepath.IsAbs(in.CWD) || !filepath.IsAbs(in.TranscriptPath) {
		return nil
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return nil
	}
	scope, root, found, lerr := lookupProject(cfg, in.CWD)
	if lerr != nil || !found {
		return nil
	}
	raw, ok := transcriptRequests(in.TranscriptPath, handoffMaxRequests)
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
