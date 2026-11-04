package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

var (
	blobPrefix      = []byte(`{"type":"api-request`)
	interruptFlag   = []byte(`"interrupted":true`)
	interruptMarker = []byte(`[Request interrupted by user`)
	rejectSentence  = []byte(`The user doesn't want to proceed with this tool use`)
	toolUseWord     = []byte(`"tool_use"`)
	toolResultWord  = []byte(`"tool_result"`)
)

// claudeSkipped lists known types that carry nothing for this reader.
var claudeSkipped = map[string]bool{
	"attachment": true, "system": true, "api-request": true, "api-request-blob": true,
	"api-request-shape": true, "queue-operation": true, "custom-title": true, "agent-name": true,
	"mode": true, "permission-mode": true, "atis-latch": true, "last-prompt": true, "pr-link": true,
	"file-history-snapshot": true, "file-history-delta": true, "bridge-session": true,
	"cost-state": true, "tools-used": true, "summary": true,
}

// claudeLine has no field for cwd, gitBranch, userFeedback, prompt text or tool
// output, on purpose.
type claudeLine struct {
	Type                  string          `json:"type"`
	SessionID             string          `json:"sessionId"`
	AgentID               string          `json:"agentId"`
	Timestamp             string          `json:"timestamp"`
	ToolDenialKind        string          `json:"toolDenialKind"`
	InterruptedByShutdown bool            `json:"interruptedByShutdown"`
	TurnOrigin            string          `json:"turnOrigin"`
	Origin                json.RawMessage `json:"origin"`
	ToolUseResult         json.RawMessage `json:"toolUseResult"`
	Message               json.RawMessage `json:"message"`
}

type claudeMessage struct {
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Text      interruptText   `json:"text"`
	Content   rejectionText   `json:"content"`
}

// interruptText and rejectionText each keep one bool from a block's text and
// drop the text itself, so prompts and tool output are never held.
type interruptText bool

func (t *interruptText) UnmarshalJSON(raw []byte) error {
	*t = interruptText(bytes.HasPrefix(bytes.TrimPrefix(raw, []byte(`"`)), interruptMarker))
	return nil
}

type rejectionText bool

func (t *rejectionText) UnmarshalJSON(raw []byte) error {
	*t = rejectionText(bytes.Contains(raw, rejectSentence))
	return nil
}

type toolRef struct{ name, messageID string }

// claudeReader holds the state of one file.
type claudeReader struct {
	t        transcript
	st       *Stats
	v        Visitor
	tools    map[string]toolRef
	first    time.Time
	lineNo   int
	held     []Denial // interrupts waiting for the next prompt's origin
	sessions map[string]bool
	decoded  bool
}

func newClaudeReader(t transcript, st *Stats, v Visitor) *claudeReader {
	return &claudeReader{t: t, st: st, v: v, tools: map[string]toolRef{}, sessions: map[string]bool{}}
}

func (r *claudeReader) line(raw []byte) {
	r.lineNo++
	if bytes.HasPrefix(raw, blobPrefix) {
		return
	}
	var l claudeLine
	if json.Unmarshal(raw, &l) != nil {
		r.st.MalformedLines++
		return
	}
	r.decoded = true
	if l.SessionID != "" {
		r.sessions[r.sessionID(&l)] = true
	}
	if r.first.IsZero() {
		if ts, err := parseTS(l.Timestamp); err == nil {
			r.first = ts
		}
	}
	switch {
	case l.Type == "assistant" || l.Type == "user":
		r.record(raw, &l)
	case !claudeSkipped[l.Type]:
		r.st.UnknownRecords++
	}
}

func (r *claudeReader) sessionID(l *claudeLine) string {
	if r.t.subagent || l.SessionID == "" {
		return r.t.session
	}
	return l.SessionID
}

func (r *claudeReader) agentID(l *claudeLine) string {
	if r.t.agent != "" {
		return r.t.agent
	}
	return l.AgentID
}

// record handles assistant and user lines. Most lines hold no tool block, and
// those skip the second decode.
func (r *claudeReader) record(raw []byte, l *claudeLine) {
	if l.Type == "user" {
		r.releaseHeld(l)
	}
	if len(l.Message) > 0 && l.Message[0] != '{' {
		r.st.MalformedLines++
		return
	}
	assistant := l.Type == "assistant"
	if assistant && !bytes.Contains(raw, toolUseWord) {
		return
	}
	if !assistant && !bytes.Contains(raw, toolResultWord) && !bytes.Contains(raw, interruptMarker) {
		return
	}
	var m claudeMessage
	if json.Unmarshal(l.Message, &m) != nil {
		r.st.MalformedLines++
		return
	}
	if len(m.Content) == 0 || m.Content[0] != '[' {
		return // a plain string prompt, or drift
	}
	var blocks []claudeBlock
	if json.Unmarshal(m.Content, &blocks) != nil {
		r.st.MalformedLines++
		return
	}
	for i, b := range blocks {
		switch {
		case assistant && b.Type == "tool_use":
			r.toolUse(l, m.ID, b, i)
		case !assistant && b.Type == "tool_result":
			r.toolResult(raw, l, b)
		case !assistant && b.Type == "text" && bool(b.Text):
			r.st.InterruptedBetweenCalls++
		}
	}
}

func (r *claudeReader) toolUse(l *claudeLine, messageID string, b claudeBlock, index int) {
	if b.ID != "" {
		r.tools[b.ID] = toolRef{b.Name, messageID}
	}
	var in struct {
		Command string `json:"command"`
	}
	if b.Name != "Bash" || json.Unmarshal(b.Input, &in) != nil || in.Command == "" {
		return
	}
	id := b.ID
	if id == "" {
		r.st.CallsWithoutID++
		id = fmt.Sprintf("%s:%d:%d", r.t.path, r.lineNo, index)
	}
	ts, err := parseTS(l.Timestamp)
	if err != nil {
		r.st.BadTimestamps++
		return
	}
	r.v.Call(Call{Harness: HarnessClaude, SessionID: r.sessionID(l), AgentID: r.agentID(l), ToolUseID: id,
		MessageID: messageID, TS: ts, FileFirst: r.first, Command: in.Command})
}

func (r *claudeReader) toolResult(raw []byte, l *claudeLine, b claudeBlock) {
	ref, found := r.tools[b.ToolUseID]
	kind := l.ToolDenialKind
	if kind == "" && (ref.name == "Bash" || !found) && interruptedFlag(raw, l.ToolUseResult) {
		kind = "interrupted"
	}
	if kind == "" {
		if b.IsError && ref.name == "Bash" && bool(b.Content) {
			r.st.UntypedRejections++
		}
		return
	}
	if !found {
		r.st.DenialsWithoutCall++
		return
	}
	ts, err := parseTS(l.Timestamp)
	if err != nil {
		r.st.BadTimestamps++
		return
	}
	d := Denial{SessionID: r.sessionID(l), AgentID: r.agentID(l), ToolUseID: b.ToolUseID, MessageID: ref.messageID,
		TS: ts, FileFirst: r.first, Kind: kind, Tool: ref.name, Subagent: r.t.subagent, Shutdown: l.InterruptedByShutdown}
	if kind == "interrupted" {
		r.held = append(r.held, d)
		return
	}
	r.v.Denial(d)
}

// interruptedFlag reports toolUseResult.interrupted. toolUseResult can also be
// a string or an array, and most lines fail the cheap check first.
func interruptedFlag(raw []byte, result json.RawMessage) bool {
	if !bytes.Contains(raw, interruptFlag) || len(result) == 0 || result[0] != '{' {
		return false
	}
	var r struct {
		Interrupted bool `json:"interrupted"`
	}
	return json.Unmarshal(result, &r) == nil && r.Interrupted
}

// releaseHeld emits held interrupts once a user record names its origin.
func (r *claudeReader) releaseHeld(l *claudeLine) {
	if len(r.held) == 0 {
		return
	}
	origin := l.TurnOrigin
	if origin == "" && len(l.Origin) > 0 {
		var o struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(l.Origin, &o) == nil {
			origin = o.Kind
		}
	}
	if origin != "" {
		r.flush(origin)
	}
}

func (r *claudeReader) flush(origin string) {
	for _, d := range r.held {
		d.NextOrigin = origin
		r.v.Denial(d)
	}
	r.held = nil
}

// finish flushes what is held and returns the file's session ids.
func (r *claudeReader) finish() []string {
	r.flush("")
	if r.decoded && len(r.sessions) == 0 {
		return []string{r.t.session}
	}
	ids := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	return ids
}
