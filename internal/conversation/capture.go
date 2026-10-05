package conversation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// A complete over-limit record errors; it is never silently clipped. Incomplete
// final JSONL records are deferred until the producer appends a newline.
const MaxRecordBytes = 16 * 1024 * 1024

type CaptureResult struct {
	Checked      string   `json:"checked"`
	Sessions     int      `json:"sessions"`
	Messages     int      `json:"messages"`
	Added        int      `json:"added"`
	Skipped      int      `json:"skipped"`
	PendingTails int      `json:"pending_tails"`
	Errors       []string `json:"errors"`
}
type sourceRecord struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	SessionID string          `json:"sessionId"`
	CWD       string          `json:"cwd"`
	Sidechain bool            `json:"isSidechain"`
	Payload   json.RawMessage `json:"payload"`
	Message   json.RawMessage `json:"message"`
}

func records(path string, visit func([]byte, sourceRecord) error) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	line := 0
	for {
		var b []byte
		for {
			part, e := r.ReadSlice('\n')
			b = append(b, part...)
			if len(b) > MaxRecordBytes {
				return false, fmt.Errorf("transcript record exceeds %d bytes", MaxRecordBytes)
			}
			if e == bufio.ErrBufferFull {
				continue
			}
			if e == io.EOF {
				return len(b) > 0, nil
			}
			if e != nil {
				return false, e
			}
			break
		}
		line++
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		var rec sourceRecord
		if !utf8.Valid(b) {
			return false, fmt.Errorf("invalid UTF-8 at transcript line %d", line)
		}
		if err = json.Unmarshal(b, &rec); err != nil {
			return false, fmt.Errorf("invalid transcript JSON at line %d: %w", line, err)
		}
		if err = visit(bytes.TrimSpace(b), rec); err != nil {
			return false, err
		}
	}
}

// Codex forks can retain the parent's session_meta while assigning a child
// UUID in the provider's rollout filename. Retain that provider identity.
var codexForkName = regexp.MustCompile(`([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

var errHeaderFound = errors.New("header found")
var errExcluded = errors.New("subagent transcript excluded")

func inspect(path, provider string) (Session, error) {
	meta := Session{Provider: provider, Source: path}
	scanned := 0
	_, err := records(path, func(_ []byte, r sourceRecord) error {
		scanned++
		if scanned > 100 {
			return errors.New("no conversation identity in first 100 records")
		}
		if provider == "codex" && r.Type == "session_meta" {
			var p struct {
				ID     string          `json:"id"`
				CWD    string          `json:"cwd"`
				Source json.RawMessage `json:"source"`
			}
			if err := json.Unmarshal(r.Payload, &p); err != nil {
				return err
			}
			if bytes.Contains(p.Source, []byte("subagent")) {
				return errExcluded
			}
			meta.ID = "codex/" + p.ID
			if fork := codexForkName.FindStringSubmatch(filepath.Base(path)); len(fork) == 3 && fork[1] == p.ID {
				meta.ID = "codex/" + fork[2]
			}
			meta.CWD = p.CWD
			if p.ID == "" || p.CWD == "" {
				return errors.New("incomplete Codex conversation identity")
			}
			return errHeaderFound
		}
		if provider == "claude" && !r.Sidechain && r.SessionID != "" && r.CWD != "" {
			meta.ID = "claude/" + r.SessionID
			meta.CWD = r.CWD
			return errHeaderFound
		}
		return nil
	})
	if errors.Is(err, errHeaderFound) {
		return meta, nil
	}
	if err != nil {
		return meta, err
	}
	return meta, errors.New("conversation identity unavailable")
}
func decodeMessage(provider string, raw []byte, r sourceRecord) (Message, bool, error) {
	m := Message{Date: r.Timestamp}
	if provider == "codex" {
		if r.Type != "response_item" {
			return m, false, nil
		}
		var p struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Channel string `json:"channel"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			return m, false, err
		}
		switch p.Type {
		case "message":
			if p.Role != "user" && p.Role != "assistant" || p.Channel == "analysis" {
				return m, false, nil
			}
			m.Kind = p.Role
			var texts []string
			for _, c := range p.Content {
				if c.Type == "input_text" || c.Type == "output_text" || c.Type == "text" {
					texts = append(texts, c.Text)
				} else {
					texts = append(texts, "["+c.Type+" content]")
				}
			}
			m.Text = strings.Join(texts, "\n")
		case "function_call", "custom_tool_call":
			m.Kind = "tool"
			m.Text = string(r.Payload)
		case "function_call_output", "custom_tool_call_output":
			m.Kind = "echo"
			m.Text = string(r.Payload)
		default:
			return m, false, nil
		}
		m.Raw = append(json.RawMessage(nil), r.Payload...)
	} else {
		if r.Sidechain || r.Type != "user" && r.Type != "assistant" {
			return m, false, nil
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(r.Message, &msg); err != nil {
			return m, false, err
		}
		m.Kind = r.Type
		var text string
		if err := json.Unmarshal(msg["content"], &text); err == nil {
			m.Text = text
		} else {
			var blocks []map[string]json.RawMessage
			if err := json.Unmarshal(msg["content"], &blocks); err != nil {
				return m, false, err
			}
			var kept []map[string]json.RawMessage
			var texts []string
			allResult := len(blocks) > 0
			for _, b := range blocks {
				var typ string
				_ = json.Unmarshal(b["type"], &typ)
				if typ == "thinking" || typ == "redacted_thinking" {
					continue
				}
				if typ != "tool_result" {
					allResult = false
				}
				kept = append(kept, b)
				if typ == "text" {
					var t string
					_ = json.Unmarshal(b["text"], &t)
					texts = append(texts, t)
				} else {
					v, _ := json.Marshal(b)
					texts = append(texts, string(v))
				}
			}
			if len(kept) == 0 {
				return m, false, nil
			}
			msg["content"], _ = json.Marshal(kept)
			m.Text = strings.Join(texts, "\n")
			if allResult {
				m.Kind = "echo"
			}
		}
		m.Raw, _ = json.Marshal(msg)
	}
	return m, true, nil
}
func captureFile(s Store, src Source, path string) (Session, int, int, bool, bool, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Session{}, 0, 0, false, false, err
	}
	if real != path {
		return Session{}, 0, 0, false, true, nil
	}
	meta, err := inspect(path, src.Provider)
	if errors.Is(err, errExcluded) {
		return meta, 0, 0, false, true, nil
	}
	if err != nil {
		return meta, 0, 0, false, false, err
	}
	if !allowed(src, meta.CWD) {
		return meta, 0, 0, false, true, nil
	}
	meta.Scope = src.Scope
	var messages []Message
	pending, err := records(path, func(raw []byte, r sourceRecord) error {
		if src.Provider == "claude" && r.SessionID != "" && "claude/"+r.SessionID != meta.ID {
			return errors.New("mixed conversation identities in transcript")
		}
		m, ok, e := decodeMessage(src.Provider, raw, r)
		if e != nil {
			return e
		}
		if ok {
			messages = append(messages, m)
		}
		return nil
	})
	if err != nil {
		return meta, 0, 0, pending, false, err
	}
	old, e := s.load(meta.ID)
	before := 0
	if e == nil {
		before = len(old.Messages)
	} else if !errors.Is(e, os.ErrNotExist) {
		return meta, 0, 0, pending, false, e
	}
	if err = s.Import(meta, messages); err != nil {
		return meta, 0, 0, pending, false, err
	}
	return meta, len(messages), len(messages) - before, pending, false, nil
}
func Capture(s Store, cfg Settings) (CaptureResult, error) {
	out := CaptureResult{Checked: time.Now().UTC().Format(time.RFC3339), Errors: []string{}}
	seen := map[string]bool{}
	for _, entry := range cfg.Sources {
		src, err := NormalizeSource(entry)
		if err != nil {
			out.Errors = append(out.Errors, err.Error())
			continue
		}
		var files []string
		if src.File != "" {
			files = []string{src.File}
		} else {
			err = filepath.WalkDir(src.Root, func(path string, d os.DirEntry, e error) error {
				if e != nil {
					return e
				}
				if d.IsDir() && d.Name() == "subagents" {
					return filepath.SkipDir
				}
				if !d.IsDir() && d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".jsonl") {
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				out.Errors = append(out.Errors, err.Error())
				continue
			}
		}
		for _, path := range files {
			if seen[path] {
				continue
			}
			_, n, added, pending, skip, e := captureFile(s, src, path)
			if e != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("%s: %v", path, e))
				continue
			}
			if skip {
				out.Skipped++
				continue
			}
			seen[path] = true
			out.Sessions++
			out.Messages += n
			out.Added += added
			if pending {
				out.PendingTails++
			}
		}
	}
	if len(out.Errors) > 0 {
		return out, fmt.Errorf("conversation capture failed for %d source(s); see errors", len(out.Errors))
	}
	return out, nil
}
