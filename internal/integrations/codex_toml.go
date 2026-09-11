package integrations

import (
	"encoding/json"
	"strings"
)

const codexTable = "[mcp_servers.mora]"

// codexSection returns the line span [start, end) of the [mcp_servers.mora]
// table body, start being the header line index, or -1 when absent.
func codexSection(lines []string) (start, end int) {
	start = -1
	for i, line := range lines {
		if strings.TrimSpace(line) == codexTable {
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
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		inner := raw[1 : len(raw)-1]
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		inner = strings.ReplaceAll(inner, `\\`, `\`)
		return inner
	}
	return raw
}

func parseTOMLStringArray(raw string) []string {
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
	lines := strings.Split(string(body), "\n")
	start, end := codexSection(lines)
	command := "command = " + quoteTOMLString(binary)
	args := `args = ["mcp", "serve"]`
	if start < 0 {
		prefix := strings.TrimRight(string(body), "\n")
		if prefix != "" {
			prefix += "\n\n"
		}
		return []byte(prefix + codexTable + "\n" + command + "\n" + args + "\nstartup_timeout_sec = 120\n")
	}
	out := append([]string{}, lines[:start+1]...)
	seenCommand, seenArgs := false, false
	for i := start + 1; i < end; i++ {
		line := lines[i]
		switch tomlKey(line) {
		case "command":
			if !seenCommand {
				out = append(out, command)
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
	return []byte(strings.Join(out, "\n"))
}

// JSON string escapes are also valid TOML basic string escapes.
func quoteTOMLString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func removeCodexEntry(body []byte) ([]byte, bool) {
	lines := strings.Split(string(body), "\n")
	start, end := codexSection(lines)
	if start < 0 {
		return body, false
	}
	out := append(append([]string{}, lines[:start]...), lines[end:]...)
	return []byte(strings.Join(out, "\n")), true
}
