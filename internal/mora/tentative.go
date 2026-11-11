package mora

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	indexstore "github.com/pyranthus-hq/mora/internal/index"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// tentativeHoldReasons returns why a write must wait for the owner. An empty
// result means it may save at once as tentative. Type is an allow list, so a
// new type is held until someone decides otherwise. Scope does not matter. The
// named automatic readers (see docs/guide.md, "MCP write policy") leave out a
// tentative row of any type and scope, and decorateCorrectionRows ignores
// tentative corrections until confirmed. Vault sync (`mora sync git`) and
// `mora backup` carry the file, tag included, so a saved tentative note can
// leave the machine that way.
func tentativeHoldReasons(m Memory) []string {
	var reasons []string
	switch m.Type {
	case "insight", "fact", "decision", "task", "correction", memory.TypeHandoff:
	default:
		reasons = append(reasons, "type "+m.Type)
	}
	// source rides into the audit log, so it gets the same check as the text.
	text := m.Title + "\n" + m.Source + "\n" + m.Text
	if _, redacted := redactProposalText(text); redacted {
		reasons = append(reasons, "secret")
	}
	if hasInvisibleUnicode(text) {
		reasons = append(reasons, "invisible unicode")
	}
	if hasControlRune(text) {
		reasons = append(reasons, "control characters")
	}
	// Match the whole text and also title+body alone (source can sit between an
	// instruction split across them), with line breaks flattened to spaces.
	bodies := []string{text, m.Title + "\n" + m.Text, strings.Join(strings.Fields(m.Title+" "+m.Text), " ")}
	for _, p := range tentativeHoldPatterns {
		for _, b := range bodies {
			if p.match(b) {
				reasons = append(reasons, p.reason)
				break
			}
		}
	}
	return reasons
}

type tentativeRow struct {
	ID    string `json:"id"`
	Scope string `json:"scope"`
	Title string `json:"title"`
}

type confirmReceipt struct {
	ID             string         `json:"id,omitempty"`
	Confirmed      bool           `json:"confirmed"`
	OwnerConfirmed bool           `json:"owner_confirmed,omitempty"`
	AuditLogged    bool           `json:"audit_logged"`
	Tentative      []tentativeRow `json:"tentative,omitempty"`
}

// cmdConfirm is the owner action that turns a tentative memory into a normal
// one. There is no MCP tool for a plain confirm, so an agent cannot confirm
// through Mora's tools. An agent with a shell can still run this command, the
// same limit `mora mcp proposals approve` has. With --mine it asks to make the
// note owner-confirmed instead: see confirmMine, and mcpConfirmMemory, its
// chat-client twin, which asks through MCP elicitation.
func cmdConfirm(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	fs := flag.NewFlagSet("confirm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	list := fs.Bool("list", false, "list tentative memories")
	jsonOut := fs.Bool("json", false, "json")
	mine := fs.Bool("mine", false, "make the note your own statement (owner-confirmed); needs your own terminal and a typed yes")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if *list == (fs.NArg() == 1) || fs.NArg() > 1 || (*mine && *list) {
		return errors.New("usage: mora confirm <memory-id> [--mine] [--json] | mora confirm --list [--json]")
	}
	if *mine {
		if err := requireMineTerminal(stdin, "mora confirm"); err != nil {
			return err
		}
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	if *list {
		rows, err := listTentativeMemories(cfg)
		if err != nil {
			return err
		}
		if *jsonOut {
			return emitReceipt(stdout, "mora.confirm", 1, confirmReceipt{Tentative: rows})
		}
		for _, r := range rows {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", r.ID, r.Scope, r.Title)
		}
		return nil
	}
	if err := refuseDashLedPositional("confirm", "memory id", fs.Arg(0)); err != nil {
		return err
	}
	if *mine {
		return confirmMine(ctx, cfg, fs.Arg(0), *jsonOut, stdout, stderr, stdin)
	}
	m, logged, err := confirmMemory(ctx, cfg, fs.Arg(0), "", "", stderr)
	if err != nil {
		return err
	}
	if *jsonOut {
		return emitReceipt(stdout, "mora.confirm", 1, confirmReceipt{ID: m.ID, Confirmed: true, AuditLogged: logged})
	}
	fmt.Fprintf(stdout, "confirmed %s (%s)\n", m.ID, agentLogTitle(m))
	return nil
}

// requireMineTerminal is the check `mora review` makes, worded for --mine: a
// person at their own terminal, not a pipe and not a known agent session. It
// cannot prove a human typed: a program inside a fake terminal passes.
func requireMineTerminal(stdin io.Reader, cmd string) error {
	if key := rulesAgentSession(); key != "" {
		return fmt.Errorf("refusing %s --mine from inside an agent session (%s is set). Run it in your own terminal.", cmd, key)
	}
	if !rulesInteractive(stdin) {
		return fmt.Errorf("refusing %s --mine from a non-interactive shell. Run it in your own terminal.", cmd)
	}
	return nil
}

// askOwner shows every field the owner-confirmed digest binds, with the text
// last, then reads one answer line from stdin. Only y or yes counts. The prompt
// goes to stderr so a --json receipt keeps stdout.
func askOwner(stderr io.Writer, stdin io.Reader, m Memory, question string) bool {
	for _, f := range memory.OwnerConfirmedFields(m) {
		if f[0] == "text" {
			fmt.Fprintf(stderr, "text:\n%s\n", reviewShowText(f[1]))
			continue
		}
		fmt.Fprintf(stderr, "%s: %s\n", f[0], rulesShow(f[1]))
	}
	fmt.Fprintf(stderr, "%s [y/N] ", question)
	// Not rulesReadConfirm: that one drops the read error, so "y" then EOF
	// would count as yes. Only a complete line, ending in a newline, counts.
	line, err := bufio.NewReader(stdin).ReadString('\n')
	fmt.Fprintln(stderr)
	if err != nil || !strings.HasSuffix(line, "\n") {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// confirmMine makes one agent note owner-confirmed after the owner reads it and
// types yes. The digest and content hash are of the file as shown; confirmMemory
// refuses if the file changed before it writes.
func confirmMine(ctx context.Context, cfg Config, id string, jsonOut bool, stdout, stderr io.Writer, stdin io.Reader) error {
	m, raw, already, err := ownerConfirmCandidate(cfg, id)
	if err != nil {
		return err
	}
	if already {
		if jsonOut {
			return emitReceipt(stdout, "mora.confirm", 1, confirmReceipt{ID: m.ID, Confirmed: true, OwnerConfirmed: true})
		}
		fmt.Fprintf(stdout, "%s is already owner-confirmed; nothing changed\n", m.ID)
		return nil
	}
	if !askOwner(stderr, stdin, m, "Confirm this exact text as your own statement?") {
		return errors.New("Not confirmed. Nothing was changed.")
	}
	m, logged, err := confirmMemory(ctx, cfg, id, indexstore.ContentSHA256(raw), memory.OwnerConfirmedDigest(m), stderr)
	if err != nil {
		return err
	}
	if jsonOut {
		return emitReceipt(stdout, "mora.confirm", 1, confirmReceipt{ID: m.ID, Confirmed: true, OwnerConfirmed: true, AuditLogged: logged})
	}
	fmt.Fprintf(stdout, "owner-confirmed %s (%s)\n", m.ID, agentLogTitle(m))
	return nil
}

// ownerConfirmCandidate loads the note `mora confirm --mine` and the MCP
// confirm_memory tool may offer the owner: an agent-written note in the
// owner's own vault. raw is the file as read, so the caller can bind the
// content hash of exactly what it shows. already is true when the note is
// owner-confirmed as it stands.
func ownerConfirmCandidate(cfg Config, id string) (m Memory, raw []byte, already bool, err error) {
	found, err := findMemory(cfg, id)
	if err != nil {
		return Memory{}, nil, false, err
	}
	if m, err = parseMemory(found.Path); err != nil {
		return Memory{}, nil, false, err
	}
	if m.Owner != "" || !strings.HasPrefix(m.ID, "mem_") || memory.DeriveProvenance(m) != memory.ProvenanceAuthored {
		return Memory{}, nil, false, fmt.Errorf("memory %s is not an agent-written note; only those can be confirmed as your own statement", m.ID)
	}
	if memory.IsOwnerConfirmed(m) {
		return m, nil, true, nil
	}
	if raw, err = os.ReadFile(m.Path); err != nil {
		return Memory{}, nil, false, err
	}
	return m, raw, false, nil
}

// confirmMemory removes the tentative tag from one memory. A non-empty
// expectSHA is the content hash of the file the owner saw: the core refuses if
// the file it reads differs, and writes from that same read. A file system has
// no compare-and-swap, so the window left is the time between that read and the
// write. A non-empty ownerDigest (only the `mora review` keep and
// `mora confirm --mine` pass one) swaps the tentative tag for the
// owner-confirmed tag in the same write, or adds it to a note that is not
// tentative, and needs expectSHA so the digest is of the bytes the owner saw.
// The MCP confirm_memory tool passes one too, through confirmMemoryVia, and
// only after the owner answered yes in its elicitation prompt.
func confirmMemory(ctx context.Context, cfg Config, id, expectSHA, ownerDigest string, stderr io.Writer) (Memory, bool, error) {
	return confirmMemoryVia(ctx, cfg, id, expectSHA, ownerDigest, "cli", stderr)
}

// confirmMemoryVia is confirmMemory with the audit-log caller named: "cli" for
// the commands, mcpConfirmCaller for the MCP elicitation path.
func confirmMemoryVia(ctx context.Context, cfg Config, id, expectSHA, ownerDigest, caller string, stderr io.Writer) (Memory, bool, error) {
	if ownerDigest != "" && expectSHA == "" {
		return Memory{}, false, errors.New("owner confirmation needs the content hash of the shown note")
	}
	found, err := findMemory(cfg, id)
	if err != nil {
		return Memory{}, false, err
	}
	// Reparse the file: a read result can carry derived fields.
	m, err := parseMemory(found.Path)
	if err != nil {
		return Memory{}, false, err
	}
	if ownerDigest == "" && !memory.IsTentative(m) {
		return Memory{}, false, fmt.Errorf("memory %s is not tentative", m.ID)
	}
	raw, err := os.ReadFile(m.Path)
	if err != nil {
		return Memory{}, false, err
	}
	if expectSHA != "" && indexstore.ContentSHA256(raw) != expectSHA {
		return Memory{}, false, fmt.Errorf("memory %s changed after it was shown; nothing was done", m.ID)
	}
	action, swap := "confirm", ""
	if ownerDigest != "" {
		action, swap = "owner-confirm", memory.OwnerConfirmedTagPrefix+ownerDigest
	}
	body, err := withoutTentativeTag(raw, swap)
	if err != nil {
		return Memory{}, false, fmt.Errorf("memory %s: %w", m.ID, err)
	}
	if m, err = parseMemoryBytes(m.Path, body); err != nil {
		return Memory{}, false, err
	}
	if ownerDigest != "" && (memory.OwnerConfirmedDigest(m) != ownerDigest || !memory.IsOwnerConfirmed(m)) {
		return Memory{}, false, fmt.Errorf("memory %s cannot be owner-confirmed as shown; nothing was done", m.ID)
	}
	op, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindWrite, Path: m.Path, ContentSHA256: indexstore.ContentSHA256(body)})
	if err != nil {
		return Memory{}, false, err
	}
	if err := atomicio.Write(m.Path, body, 0o644); err != nil {
		_ = unmarkIndexDirty(cfg, op.OpID)
		return Memory{}, false, err
	}
	logged := true
	if lerr := appendAgentWriteLog(cfg, time.Now(), action, m, caller); lerr != nil {
		logged = false
		fmt.Fprintf(stderr, "warning: memory %s confirmed, but %s could not be appended: %v\n", m.ID, agentWriteLogName, lerr)
	}
	// The op stays on failure, so the index reads dirty until a rebuild covers it.
	if err := indexUpsert(ctx, cfg, m); err != nil {
		fmt.Fprintf(stderr, "warning: memory %s confirmed, but the search index could not be updated: %v; run `mora index rebuild`\n", m.ID, err)
	} else if authoredWriteProjectionPending(cfg, op) {
		if merr := markFTSCommitted(cfg, op); merr != nil {
			fmt.Fprintf(stderr, "warning: memory confirmed, but its index marker could not be updated: %v\n", merr)
		}
		if rerr := reconcileAuthoredWrites(ctx, cfg); rerr != nil {
			fmt.Fprintf(stderr, "warning: memory confirmed, but full index reconciliation is pending: %v\n", rerr)
		}
	}
	return m, logged, nil
}

// withoutTentativeTag edits only the frontmatter tags line, because a render
// round trip would trim the body and drop keys this binary does not know. A
// non-empty swap takes the tentative tag's place instead of nothing, or is
// added when the note has no tentative tag. Any
// owner-confirmed tag already on the line is dropped, so only the swap here
// can make the note owner-confirmed.
func withoutTentativeTag(raw []byte, swap string) ([]byte, error) {
	lines := strings.SplitAfter(string(raw), "\n")
	for i, line := range lines {
		if i > 0 && strings.TrimRight(line, "\r\n") == "---" {
			break
		}
		if !strings.HasPrefix(line, "tags: [") {
			continue
		}
		body := strings.TrimRight(line, "\r\n")
		inner, ok := strings.CutSuffix(strings.TrimPrefix(body, "tags: ["), "]")
		if !ok {
			break
		}
		var kept []string
		found := false
		for _, tag := range genericutil.SplitCSV(inner) {
			switch {
			case tag == memory.TentativeTag:
				if !found && swap != "" {
					kept = append(kept, swap)
				}
				found = true
			case strings.HasPrefix(tag, memory.OwnerConfirmedTagPrefix):
			default:
				kept = append(kept, tag)
			}
		}
		if !found && swap != "" {
			kept, found = append(kept, swap), true
		}
		if !found {
			break
		}
		lines[i] = "tags: [" + strings.Join(kept, ", ") + "]" + line[len(body):]
		return []byte(strings.Join(lines, "")), nil
	}
	return nil, errors.New("tentative tag not found in the frontmatter tags line")
}

func withoutTentative(mems []Memory) []Memory {
	kept := make([]Memory, 0, len(mems))
	for _, m := range mems {
		if !memory.IsTentative(m) {
			kept = append(kept, m)
		}
	}
	return kept
}

func listTentativeMemories(cfg Config) ([]tentativeRow, error) {
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return nil, err
	}
	rows := []tentativeRow{}
	for _, path := range files {
		m, perr := parseMemory(path)
		if perr != nil || m.DeletedAt != "" || !memory.IsTentative(m) {
			continue
		}
		rows = append(rows, tentativeRow{ID: m.ID, Scope: m.Scope, Title: agentLogTitle(m)})
	}
	return rows, nil
}
