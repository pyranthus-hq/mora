package sessions

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// execCall matches the start of tools.exec_command({cmd: ...}), up to the value.
var execCall = regexp.MustCompile(`\b(?:tools\.)?exec_command\s*\(\s*\{[^{}]*?(?:"cmd"|'cmd'|\bcmd)\s*:\s*`)

var jsEscapes = strings.NewReplacer(`\\`, `\`, `\'`, `'`, `\"`, `"`, "\\`", "`", `\n`, "\n", `\t`, "\t")

var codexShellTools = map[string]bool{"shell": true, "shell_command": true, "exec_command": true, "exec": true}

var codexSkipped = map[string]bool{
	"turn_context": true, "world_state": true, "compacted": true, "token_usage_record": true,
}

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexMeta struct {
	ID        string `json:"id"`
	ThreadID  string `json:"thread_id"`
	SessionID string `json:"session_id"`
	Timestamp string `json:"timestamp"`
}

// codexItem has no field for tool output, on purpose. Its text cannot be
// trusted: "rejected by user" is also written by policy with no person involved.
type codexItem struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Input     json.RawMessage `json:"input"`
	Arguments json.RawMessage `json:"arguments"`
	Action    json.RawMessage `json:"action"`
}

type codexEvent struct {
	Type   string          `json:"type"`
	Status json.RawMessage `json:"status"`
	TurnID json.RawMessage `json:"turn_id"`
	CallID json.RawMessage `json:"call_id"`
}

// codexReader holds the state of one file.
type codexReader struct {
	t       transcript
	st      *Stats
	v       Visitor
	session string
	first   time.Time
	lineNo  int
	decoded bool
}

func newCodexReader(t transcript, st *Stats, v Visitor) *codexReader {
	return &codexReader{t: t, st: st, v: v}
}

func (r *codexReader) line(raw []byte) {
	r.lineNo++
	var l codexLine
	if json.Unmarshal(raw, &l) != nil {
		r.st.MalformedLines++
		return
	}
	r.decoded = true
	if r.first.IsZero() {
		if ts, err := parseTS(l.Timestamp); err == nil {
			r.first = ts
		}
	}
	var err error
	switch {
	case l.Type == "session_meta":
		err = r.meta(&l)
	case l.Type == "response_item":
		err = r.item(&l)
	case l.Type == "event_msg":
		err = r.event(&l)
	case !codexSkipped[l.Type]:
		r.st.UnknownRecords++
	}
	if err != nil {
		r.st.MalformedLines++
	}
}

// meta keeps the first session_meta, which is this file's own header.
func (r *codexReader) meta(l *codexLine) error {
	var m codexMeta
	if err := json.Unmarshal(l.Payload, &m); err != nil {
		return err
	}
	if r.session != "" {
		return nil
	}
	for _, id := range []string{m.ID, m.ThreadID, m.SessionID} {
		if id != "" {
			r.session = id
			break
		}
	}
	if ts, err := parseTS(m.Timestamp); err == nil {
		r.first = ts
	}
	return nil
}

func (r *codexReader) event(l *codexLine) error {
	var e codexEvent
	if err := json.Unmarshal(l.Payload, &e); err != nil {
		return err
	}
	switch {
	case e.Type == "turn_aborted":
		r.st.CodexTurnInterrupts++
		r.emitEvent(l, KindCodexTurnInterrupt, e.TurnID)
	case e.Type == "patch_apply_end" && string(e.Status) == `"declined"`:
		r.st.CodexPatchesDeclined++
		r.emitEvent(l, KindCodexPatchDeclined, e.CallID)
	}
	return nil
}

// emitEvent sends an event that names no command, so the Visitor can drop its
// copies. id is raw JSON, used only to match copies.
func (r *codexReader) emitEvent(l *codexLine, kind string, id json.RawMessage) {
	ts, err := parseTS(l.Timestamp)
	if err != nil {
		r.st.BadTimestamps++
		return
	}
	r.v.Denial(Denial{SessionID: r.sessionID(), ToolUseID: string(id), TS: ts, FileFirst: r.first, Kind: kind})
}

func (r *codexReader) item(l *codexLine) error {
	var p codexItem
	if err := json.Unmarshal(l.Payload, &p); err != nil {
		return err
	}
	var commands []string
	switch p.Type {
	case "custom_tool_call":
		var source string
		if !strings.HasSuffix(p.Name, "exec") || json.Unmarshal(p.Input, &source) != nil {
			return nil
		}
		var unreadable int
		commands, unreadable = literalExecCommands(source)
		r.st.CodexCommandsUnreadable += unreadable
	case "function_call":
		if !codexShellTools[p.Name[strings.LastIndex(p.Name, ".")+1:]] {
			return nil
		}
		commands = r.oneCommand(argumentsCommand(p.Arguments))
	case "local_shell_call":
		var action struct {
			Type    string          `json:"type"`
			Command json.RawMessage `json:"command"`
		}
		if json.Unmarshal(p.Action, &action) != nil || action.Type != "exec" {
			return nil
		}
		commands = r.oneCommand(commandText(action.Command))
	}
	id := p.CallID
	if id == "" {
		id = p.ID
	}
	r.emit(l, id, commands)
	return nil
}

func (r *codexReader) oneCommand(command string) []string {
	if command == "" {
		r.st.CodexCommandsUnreadable++
		return nil
	}
	return []string{command}
}

func (r *codexReader) emit(l *codexLine, id string, commands []string) {
	for i, command := range commands {
		child := id
		if id == "" {
			r.st.CallsWithoutID++
			child = fmt.Sprintf("%s:%d:%d", r.t.path, r.lineNo, i)
		} else if len(commands) > 1 {
			child = fmt.Sprintf("%s:%d", id, i)
		}
		ts, err := parseTS(l.Timestamp)
		if err != nil {
			r.st.BadTimestamps++
			continue
		}
		r.v.Call(Call{Harness: HarnessCodex, SessionID: r.sessionID(), ToolUseID: child, TS: ts,
			FileFirst: r.first, Command: command})
	}
}

func (r *codexReader) sessionID() string {
	if r.session != "" {
		return r.session
	}
	return r.t.session
}

func (r *codexReader) finish() []string {
	if !r.decoded {
		return nil
	}
	return []string{r.sessionID()}
}

// literalExecCommands recovers the literal cmd arguments from the exec wrapper
// source. It never runs anything. Each match that is not a plain literal, such
// as a variable, a concatenation or a template with a substitution, is counted.
func literalExecCommands(source string) (commands []string, unreadable int) {
	for _, m := range execCall.FindAllStringIndex(source, -1) {
		if command, ok := literalAt(source, m[1]); ok && command != "" {
			commands = append(commands, command)
		} else {
			unreadable++
		}
	}
	return commands, unreadable
}

func literalAt(source string, start int) (string, bool) {
	if start >= len(source) || !strings.ContainsRune("\"'`", rune(source[start])) {
		return "", false
	}
	quote := source[start]
	end := start + 1
	for end < len(source) && source[end] != quote {
		if source[end] == '\\' {
			end++
		}
		end++
	}
	if end >= len(source) {
		return "", false
	}
	// Concatenations are not complete literal commands.
	rest := strings.TrimSpace(source[end+1:])
	if rest == "" || (rest[0] != ',' && rest[0] != '}') {
		return "", false
	}
	token := source[start : end+1]
	if quote == '"' {
		var command string
		err := json.Unmarshal([]byte(token), &command)
		return command, err == nil
	}
	if quote == '`' && strings.Contains(token, "${") {
		return "", false
	}
	return jsEscapes.Replace(token[1 : len(token)-1]), true
}

// argumentsCommand reads function_call arguments, a JSON string or an object.
func argumentsCommand(arguments json.RawMessage) string {
	var text string
	if json.Unmarshal(arguments, &text) == nil {
		arguments = []byte(text)
	}
	var args struct {
		Cmd     json.RawMessage `json:"cmd"`
		Command json.RawMessage `json:"command"`
	}
	if json.Unmarshal(arguments, &args) != nil {
		return ""
	}
	if len(args.Cmd) > 0 {
		return commandText(args.Cmd)
	}
	return commandText(args.Command)
}

// commandText reads a command string or argv list. ["bash", "-lc", "x"] runs x.
func commandText(raw json.RawMessage) string {
	var command string
	if json.Unmarshal(raw, &command) == nil {
		return command
	}
	var argv []string
	if json.Unmarshal(raw, &argv) != nil {
		return ""
	}
	if len(argv) >= 3 {
		shell := argv[0][strings.LastIndexAny(argv[0], `/\`)+1:]
		if (shell == "bash" || shell == "sh" || shell == "zsh") && (argv[1] == "-c" || argv[1] == "-lc" || argv[1] == "-ic") {
			return argv[2]
		}
	}
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

// shellQuote keeps argv boundaries when a list is joined, so an argument with
// a space stays one token for the lexer.
func shellQuote(arg string) string {
	if arg != "" && !strings.ContainsAny(arg, " \t\n'\"\\$`&|;<>()*?#~") {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}
