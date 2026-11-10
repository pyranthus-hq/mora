package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	indexstore "github.com/pyranthus-hq/mora/internal/index"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// tentativeHoldPatterns name text an agent may not save without review. The
// list is a coarse filter, not a security boundary: a false claim in plain
// words passes it. Tentative rows are safe because they stay out of
// authoritative surfaces, never because this filter caught everything.
var tentativeHoldPatterns = []struct {
	reason  string
	pattern *regexp.Regexp
}{
	{"sensitive", regexp.MustCompile(`(?i)\b(ssn|social security|passport|medical|diagnos(is|ed)|lawsuit|attorney|immigration|visa status)\b`)},
	{"aimed at agents", regexp.MustCompile(`(?i)\b(ignore|disregard|forget)\b.{0,30}\b(previous|prior|above|earlier|all)\b|\bsystem prompt\b|\b(you|agents?|assistants?|claude|codex)\s+(must|should|shall|need to|have to)\b|\bfrom now on\b|\b(always|never)\s+(use|run|call|do|say|tell|ask|write|approve|trust)\b`)},
}

// tentativeHoldReasons returns why a write must wait for the owner. An empty
// result means it may save at once as tentative. Type is an allow list, so a
// new type is held until someone decides otherwise. A correction stays held
// because it changes how other records are retrieved, and those readers do not
// leave out tentative rows. Scope does not matter: every automatic reader
// leaves out a tentative row of any type and scope.
func tentativeHoldReasons(m Memory) []string {
	var reasons []string
	switch m.Type {
	case "insight", "fact", "decision", "task":
	default:
		reasons = append(reasons, "type "+m.Type)
	}
	// source rides into the audit log, so it gets the same check as the text.
	text := m.Title + "\n" + m.Source + "\n" + m.Text
	if _, redacted := redactProposalText(text); redacted {
		reasons = append(reasons, "secret")
	}
	for _, p := range tentativeHoldPatterns {
		if p.pattern.MatchString(text) {
			reasons = append(reasons, p.reason)
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
	ID          string         `json:"id,omitempty"`
	Confirmed   bool           `json:"confirmed"`
	AuditLogged bool           `json:"audit_logged"`
	Tentative   []tentativeRow `json:"tentative,omitempty"`
}

// cmdConfirm is the owner action that turns a tentative memory into a normal
// one. There is no MCP tool for it, so an agent cannot confirm through Mora's
// tools. An agent with a shell can still run this command, the same limit
// `mora mcp proposals approve` has.
func cmdConfirm(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("confirm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	list := fs.Bool("list", false, "list tentative memories")
	jsonOut := fs.Bool("json", false, "json")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if *list == (fs.NArg() == 1) || fs.NArg() > 1 {
		return errors.New("usage: mora confirm <memory-id> [--json] | mora confirm --list [--json]")
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
	m, logged, err := confirmMemory(ctx, cfg, fs.Arg(0), "", stderr)
	if err != nil {
		return err
	}
	if *jsonOut {
		return emitReceipt(stdout, "mora.confirm", 1, confirmReceipt{ID: m.ID, Confirmed: true, AuditLogged: logged})
	}
	fmt.Fprintf(stdout, "confirmed %s (%s)\n", m.ID, agentLogTitle(m))
	return nil
}

// confirmMemory removes the tentative tag from one memory. A non-empty
// expectSHA is the content hash of the file the owner saw: the core refuses if
// the file it reads differs, and writes from that same read. A file system has
// no compare-and-swap, so the window left is the time between that read and the
// write.
func confirmMemory(ctx context.Context, cfg Config, id, expectSHA string, stderr io.Writer) (Memory, bool, error) {
	found, err := findMemory(cfg, id)
	if err != nil {
		return Memory{}, false, err
	}
	// Reparse the file: a read result can carry derived fields.
	m, err := parseMemory(found.Path)
	if err != nil {
		return Memory{}, false, err
	}
	if !memory.IsTentative(m) {
		return Memory{}, false, fmt.Errorf("memory %s is not tentative", m.ID)
	}
	raw, err := os.ReadFile(m.Path)
	if err != nil {
		return Memory{}, false, err
	}
	if expectSHA != "" && indexstore.ContentSHA256(raw) != expectSHA {
		return Memory{}, false, fmt.Errorf("memory %s changed after it was shown; nothing was done", m.ID)
	}
	body, err := withoutTentativeTag(raw)
	if err != nil {
		return Memory{}, false, fmt.Errorf("memory %s: %w", m.ID, err)
	}
	if m, err = parseMemoryBytes(m.Path, body); err != nil {
		return Memory{}, false, err
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
	if lerr := appendAgentWriteLog(cfg, time.Now(), "confirm", m, "cli"); lerr != nil {
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
// round trip would trim the body and drop keys this binary does not know.
func withoutTentativeTag(raw []byte) ([]byte, error) {
	lines := strings.SplitAfter(string(raw), "\n")
	for i, line := range lines {
		if i > 0 && strings.TrimRight(line, "\r\n") == "---" {
			break
		}
		if !strings.HasPrefix(line, "tags: [") {
			continue
		}
		edited := line
		for _, form := range []string{", " + memory.TentativeTag, memory.TentativeTag + ", ", memory.TentativeTag} {
			if strings.Contains(edited, form) {
				edited = strings.Replace(edited, form, "", 1)
				break
			}
		}
		if edited == line {
			break
		}
		lines[i] = edited
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
