package integrations

import (
	"errors"
	"strings"
)

// The managed instruction block is fenced by these marker lines. wire replaces
// only what lies between them (inclusive) and unwire removes exactly that span,
// so everything a user wrote around the block is never touched.
const (
	BlockBegin = "<!-- BEGIN MORA (managed by `mora agents wire`; edits between these markers are replaced) -->"
	BlockEnd   = "<!-- END MORA -->"
)

// The share bullets describe today's retrieval: only search reads subscribed
// shares (pilot F5). When context, brief and think read shares too, update
// these lines; `mora agents wire` then refreshes every block in place.
const mcpBlockBody = `## Mora memory

Mora is this machine's cross-session memory. Memories live in a local Markdown vault; memories other machines publish arrive as read-only subscribed shares.

- Use the ` + "`mora`" + ` MCP tools, not the shell: ` + "`search_memory`" + ` to find, ` + "`read_memory`" + ` to read one memory in full, ` + "`write_memory`" + ` to save. Shell out only if these tools are missing, and then always pass ` + "`--json`" + `: ` + "`mora search \"<query>\" --json`" + `, ` + "`mora read <id> --json`" + `.
- Never read Mora's files directly (the vault, or the share corpus in Mora's data directory) with cat, rg or grep. The tools keep provenance and share labels attached; raw files drop them.
- Search first. At the start of a task, call ` + "`search_memory`" + ` on the task topic before substantive work, and before answering about past work, decisions or commitments.
- Shared memories carry the share that holds them: an ` + "`owner`" + ` field, and a ` + "`[<share>]`" + ` title prefix in CLI output. Today only ` + "`search_memory`" + ` reads subscribed shares; ` + "`brief`" + `, ` + "`context_memory`" + ` and ` + "`think`" + ` see only the local vault and can look empty on a machine that mostly subscribes. An empty brief does not mean there is no memory.
- Shared hits rank below local ones. If results look thin, raise ` + "`limit`" + ` to 15-20 or add ` + "`scope`" + ` before concluding nothing is known.
- Never invent missing context. Cite the memory ID you relied on.
- When a durable fact, decision, correction or shipped outcome emerges, search to avoid a duplicate, then save it with ` + "`write_memory`" + ` and read it back by ID. Never save secrets or credentials.`

const cliBlockBody = `## Mora memory

Mora is this machine's cross-session memory. This agent has no MCP, so use the ` + "`mora`" + ` CLI through the shell. Memories live in a local Markdown vault; memories other machines publish arrive as read-only subscribed shares.

- Always pass ` + "`--json`" + `: ` + "`mora search \"<query>\" --limit 15 --json`" + ` to find, ` + "`mora read <id> --json`" + ` to read one memory in full.
- Never read Mora's files directly (the vault, or the share corpus in Mora's data directory) with cat, rg or grep. The CLI keeps provenance and share labels attached; raw files drop them.
- Search first. At the start of a task, run ` + "`mora search`" + ` on the task topic before substantive work, and before answering about past work, decisions or commitments.
- Shared memories carry the share that holds them: an ` + "`owner`" + ` field in JSON, and a ` + "`[<share>]`" + ` title prefix in plain output. Today only ` + "`mora search`" + ` reads subscribed shares; ` + "`mora brief`" + `, ` + "`mora context`" + ` and ` + "`mora think`" + ` see only the local vault and can look empty on a machine that mostly subscribes. An empty brief does not mean there is no memory.
- Shared hits rank below local ones. If results look thin, raise ` + "`--limit`" + ` or add ` + "`--scope project:<name>`" + ` before concluding nothing is known.
- Never invent missing context. Cite the memory ID you relied on.
- When a durable fact, decision, correction or shipped outcome emerges, search to avoid a duplicate, then save it with ` + "`mora write --scope <scope> --type <insight|fact|decision|task> --title \"<title>\" --text \"<body>\" --json`" + ` and read it back by ID. Never save secrets or credentials.`

func mcpBlock() string { return BlockBegin + "\n" + mcpBlockBody + "\n" + BlockEnd }
func cliBlock() string { return BlockBegin + "\n" + cliBlockBody + "\n" + BlockEnd }

var errBrokenMarkers = errors.New("the Mora block markers are unmatched, out of order, or repeated; fix the file by hand")

// blockSpan finds the managed block as a byte range [start, end) that covers
// the BEGIN line through the END line and its newline. ok is false when the
// file has no block; a half or repeated block is an error, never a guess.
func blockSpan(text string) (start, end int, ok bool, err error) {
	begins := markerLines(text, BlockBegin)
	ends := markerLines(text, BlockEnd)
	switch {
	case len(begins) == 0 && len(ends) == 0:
		return 0, 0, false, nil
	case len(begins) != 1 || len(ends) != 1 || ends[0] < begins[0]:
		return 0, 0, false, errBrokenMarkers
	}
	start = begins[0]
	end = ends[0] + len(BlockEnd)
	if end < len(text) && text[end] == '\n' {
		end++
	}
	return start, end, true, nil
}

// markerLines returns the offsets of lines that are exactly marker. The BEGIN
// marker is matched on its stable prefix so a reworded suffix still pairs up.
func markerLines(text, marker string) []int {
	prefix := marker
	if marker == BlockBegin {
		prefix = "<!-- BEGIN MORA"
	}
	var out []int
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == marker || (marker == BlockBegin && strings.HasPrefix(trimmed, prefix)) {
			out = append(out, offset)
		}
		offset += len(line)
	}
	return out
}

// blockText is the managed block as it stands in a file, or "" without one.
func blockText(body []byte) string {
	text := string(body)
	start, end, ok, err := blockSpan(text)
	if err != nil || !ok {
		return ""
	}
	return strings.TrimRight(text[start:end], "\n")
}

// upsertBlock replaces the managed block in place or appends it after one
// blank line, so removeBlock can restore the original bytes.
func upsertBlock(body []byte, block string) ([]byte, error) {
	text := string(body)
	start, end, ok, err := blockSpan(text)
	if err != nil {
		return nil, err
	}
	if ok {
		return []byte(text[:start] + block + "\n" + text[end:]), nil
	}
	if text == "" {
		return []byte(block + "\n"), nil
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return []byte(text + "\n" + block + "\n"), nil
}

// removeBlock deletes the managed block and the blank line wire put before it.
func removeBlock(body []byte) ([]byte, bool, error) {
	text := string(body)
	start, end, ok, err := blockSpan(text)
	if err != nil || !ok {
		return body, false, err
	}
	before, after := text[:start], text[end:]
	if strings.HasSuffix(before, "\n\n") {
		if after == "" {
			before = before[:len(before)-1]
		} else if strings.HasPrefix(after, "\n") {
			after = after[1:]
		}
	}
	return []byte(before + after), true, nil
}
