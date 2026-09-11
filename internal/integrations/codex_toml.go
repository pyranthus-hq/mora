package integrations

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

const codexTable = "[mcp_servers.mora]"

// codexSection returns the line span [start, end) of the [mcp_servers.mora]
// table body, start being the header line index, or -1 when absent.
func codexSection(lines []string) (start, end int) {
	start = -1
	for i, line := range lines {
		if tomlHeader(line) == "mcp_servers.mora" {
			start = i
			break
		}
	}
	if start < 0 {
		return -1, -1
	}
	end = len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			end = i
			break
		}
	}
	return start, end
}

func tomlKey(line string) string {
	trimmed := strings.TrimSpace(line)
	if i := strings.Index(trimmed, "="); i > 0 {
		return strings.TrimSpace(trimmed[:i])
	}
	return ""
}

func parseTOMLString(raw string) string {
	raw, _ = tomlComment(raw)
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		if value, err := strconv.Unquote(raw); err == nil {
			return value
		}
	}
	return raw
}

func parseTOMLStringArray(raw string) []string {
	raw, _ = tomlComment(raw)
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, parseTOMLString(part))
	}
	return out
}

func readCodexEntry(body []byte) (*entry, error) {
	if codexInline(body) {
		return nil, errCodexInline
	}
	lines := strings.Split(string(body), "\n")
	start, end := codexSection(lines)
	if start < 0 {
		return nil, nil
	}
	e := &entry{}
	for _, line := range lines[start+1 : end] {
		key := tomlKey(line)
		value := strings.TrimSpace(line[strings.Index(line, "=")+1:])
		switch key {
		case "command":
			e.Command = parseTOMLString(value)
		case "args":
			e.Args = parseTOMLStringArray(value)
		}
	}
	return e, nil
}

// upsertCodexEntry changes only the direct Mora table, retaining subtables.
func upsertCodexEntry(body []byte, binary string) []byte {
	ending := codexLineEnding(body)
	lines := strings.Split(string(body), ending)
	start, end := codexSection(lines)
	command := "command = " + quoteTOMLString(binary)
	args := `args = ["mcp", "serve"]`
	if start < 0 {
		prefix := strings.TrimRight(string(body), "\r\n")
		if prefix != "" {
			prefix += ending + ending
		}
		return []byte(prefix + strings.Join([]string{codexTable, command, args, "startup_timeout_sec = 120", ""}, ending))
	}
	out := append([]string{}, lines[:start+1]...)
	seenCommand, seenArgs := false, false
	for i := start + 1; i < end; i++ {
		line := lines[i]
		switch tomlKey(line) {
		case "command":
			if !seenCommand {
				_, comment := tomlComment(line)
				out = append(out, command+comment)
				seenCommand = true
			}
		case "args":
			if !seenArgs {
				out = append(out, args)
				seenArgs = true
			}
			// Consume a multiline array before replacing it with the canonical args.
			value := strings.TrimSpace(line[strings.Index(line, "=")+1:])
			if strings.HasPrefix(value, "[") && !strings.Contains(value, "]") {
				for i+1 < end {
					i++
					if strings.Contains(lines[i], "]") {
						break
					}
				}
			}
		default:
			out = append(out, line)
		}
	}
	if !seenCommand {
		out = append(out, command)
	}
	if !seenArgs {
		out = append(out, args)
	}
	out = append(out, lines[end:]...)
	return []byte(strings.Join(out, ending))
}

// JSON string escapes are also valid TOML basic string escapes.
func quoteTOMLString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func removeCodexEntry(body []byte) ([]byte, bool) {
	ending := codexLineEnding(body)
	text := string(body)
	trailing := strings.HasSuffix(text, ending)
	lines := strings.Split(strings.TrimSuffix(text, ending), ending)
	var out []string
	removing, changed := false, false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			header := tomlHeader(line)
			removing = header == "mcp_servers.mora" || strings.HasPrefix(header, "mcp_servers.mora.")
			changed = changed || removing
		}
		if !removing {
			out = append(out, line)
		}
	}
	if !changed {
		return body, false
	}
	result := strings.Join(out, ending)
	if trailing {
		result += ending
	} else {
		result = strings.TrimRight(result, "\r\n")
	}
	return []byte(result), true
}

var errCodexInline = errors.New("codex config declares mcp_servers.mora inline; edit ~/.codex/config.toml by hand")

func codexLineEnding(body []byte) string {
	if strings.Contains(string(body), "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// unquotedIndex ignores delimiters inside basic and literal strings.
func unquotedIndex(text string, delimiter byte) int {
	var quote byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		if quote != 0 {
			if quote == '"' && c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
		} else if c == '"' || c == '\'' {
			quote = c
		} else if c == delimiter {
			return i
		}
	}
	return -1
}

// Return the comment with its preceding whitespace so command rewrites retain it.
func tomlComment(line string) (string, string) {
	if i := unquotedIndex(line, '#'); i >= 0 {
		start := i
		for start > 0 && (line[start-1] == ' ' || line[start-1] == '\t') {
			start--
		}
		return line[:start], line[start:]
	}
	return line, ""
}

func tomlPath(raw string) string {
	var parts []string
	for {
		i := unquotedIndex(raw, '.')
		part := raw
		if i >= 0 {
			part = raw[:i]
		}
		part = strings.TrimSpace(part)
		// Normalize quoted bare-key segments only: quoted dots remain distinct keys.
		if len(part) >= 2 && ((part[0] == '"' && part[len(part)-1] == '"') || (part[0] == '\'' && part[len(part)-1] == '\'')) {
			value := part[1 : len(part)-1]
			if part[0] == '"' {
				if decoded, err := strconv.Unquote(part); err == nil {
					value = decoded
				}
			}
			if !strings.ContainsAny(value, ".[]") {
				part = value
			}
		}
		parts = append(parts, part)
		if i < 0 {
			break
		}
		raw = raw[i+1:]
	}
	return strings.Join(parts, ".")
}

func tomlHeader(line string) string {
	line, _ = tomlComment(line)
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
		return ""
	}
	return tomlPath(strings.TrimSpace(line[1 : len(line)-1]))
}

func codexInline(body []byte) bool {
	section := ""
	for _, line := range strings.Split(string(body), "\n") {
		line, _ = tomlComment(line)
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = tomlHeader(line)
			continue
		}
		i := unquotedIndex(line, '=')
		if i < 0 {
			continue
		}
		key := tomlPath(line[:i])
		if section != "" {
			key = section + "." + key
		}
		if key == "mcp_servers.mora" || strings.HasPrefix(key, "mcp_servers.mora.") && (section == "" || section == "mcp_servers") {
			return true
		}
	}
	return false
}
