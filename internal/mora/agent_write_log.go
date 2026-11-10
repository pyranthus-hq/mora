package mora

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/companion"
	indexstore "github.com/pyranthus-hq/mora/internal/index"
)

// agentWriteLogName is the plain-text, append-only audit log of agent writes.
// It lives at the vault root next to log.md, so it travels with the memories it
// describes. Each line carries a timestamp, the action (write or undo), the
// memory id, scope, source, caller and a one-line title — never body text
// beyond what the memory file already holds.
const agentWriteLogName = "agent-writes.log"

const agentWriteLogTitleRunes = 120

func agentWriteLogPath(cfg Config) string { return filepath.Join(cfg.VaultDir, agentWriteLogName) }

type agentCallerKey struct{}

// withAgentCaller names the surface an agent write arrived through
// ("mcp-stdio/<client>", "http", "companion", "approved-proposal").
func withAgentCaller(ctx context.Context, caller string) context.Context {
	return context.WithValue(ctx, agentCallerKey{}, caller)
}

func agentCallerFrom(ctx context.Context) string {
	if c, ok := ctx.Value(agentCallerKey{}).(string); ok && c != "" {
		return c
	}
	return "mcp"
}

// sanitizeCallerToken keeps a client-supplied name to a short, log-safe token.
func sanitizeCallerToken(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if b.Len() >= 64 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// agentLogTitle is the memory's title, or its first non-empty text line, as a
// single bounded line.
func agentLogTitle(m Memory) string {
	t := strings.TrimSpace(m.Title)
	if t == "" {
		for _, line := range strings.Split(m.Text, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				t = line
				break
			}
		}
	}
	t = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, t)
	if rs := []rune(t); len(rs) > agentWriteLogTitleRunes {
		t = string(rs[:agentWriteLogTitleRunes]) + "…"
	}
	return t
}

func agentWriteLogLine(now time.Time, action string, m Memory, caller string) string {
	source := m.Source
	if source == "" {
		source = "-"
	}
	return fmt.Sprintf("%s %s %s scope=%s source=%s caller=%s title=%s\n",
		now.Format(time.RFC3339), action, m.ID, logToken(m.Scope), logToken(source), logToken(caller), strconv.Quote(agentLogTitle(m)))
}

// logToken keeps a field whitespace-free so each line splits on spaces.
func logToken(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, s)
	if s == "" {
		return "-"
	}
	return s
}

func appendAgentWriteLog(cfg Config, now time.Time, action string, m Memory, caller string) error {
	return atomicio.AppendFile(agentWriteLogPath(cfg), agentWriteLogLine(now, action, m, caller))
}

// agentWriteLogged reports whether the audit log records an agent write of id.
func agentWriteLogged(cfg Config, id string) (bool, error) {
	f, err := os.Open(agentWriteLogPath(cfg))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		fields := strings.SplitN(sc.Text(), " ", 4)
		if len(fields) >= 3 && fields[1] == "write" && fields[2] == id {
			return true, nil
		}
	}
	return false, sc.Err()
}

// isAgentWrittenMemory is true for memories an agent wrote: the MCP and
// companion sources, or any id the audit log records as an agent write (an
// agent can set its own source label, so the log is the stronger witness).
func isAgentWrittenMemory(cfg Config, m Memory) (bool, error) {
	if m.Source == "mcp" || m.Source == companion.OriginCompanion {
		return true, nil
	}
	return agentWriteLogged(cfg, m.ID)
}

type undoReceipt struct {
	ID          string `json:"id"`
	Deleted     bool   `json:"deleted"`
	AgentWrite  bool   `json:"agent_write"`
	AuditLogged bool   `json:"audit_logged"`
}

// cmdUndo removes one agent-written memory through the same delete path as
// `mora delete`, then records the undo in the audit log. It refuses memories
// an agent did not write unless --force is given.
func cmdUndo(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("undo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "remove a memory an agent did not write")
	jsonOut := fs.Bool("json", false, "json")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: mora undo <memory-id> [--force] [--json]")
	}
	if err := refuseDashLedPositional("undo", "memory id", fs.Arg(0)); err != nil {
		return err
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	m, err := findMemory(cfg, fs.Arg(0))
	if err != nil {
		return err
	}
	agent, logged, reconcile, err := undoMemory(ctx, cfg, m, *force, "", stderr)
	if err != nil {
		return err
	}
	var rerr error
	if *jsonOut {
		rerr = emitReceipt(stdout, "mora.undo", 1, undoReceipt{ID: m.ID, Deleted: true, AgentWrite: agent, AuditLogged: logged})
	} else {
		fmt.Fprintf(stdout, "undid %s (%s)\n", m.ID, agentLogTitle(m))
	}
	if reconcile {
		if err := reconcileAuthoredWrites(ctx, cfg); err != nil {
			fmt.Fprintf(stderr, "warning: memory removed and dropped from search, but full index reconciliation is pending: %v\n", err)
		}
	}
	return rerr
}

// undoMemory deletes one memory and appends the undo line. A non-empty
// expectSHA is the content hash of the file the owner saw: the core refuses if
// the file it reads differs, and deletes after that same read. A file system
// has no compare-and-swap, so the window left is the time between that read and
// the delete.
func undoMemory(ctx context.Context, cfg Config, m Memory, force bool, expectSHA string, stderr io.Writer) (agent, logged, reconcile bool, err error) {
	if expectSHA != "" {
		raw, rerr := os.ReadFile(m.Path)
		if rerr != nil {
			return false, false, false, rerr
		}
		if indexstore.ContentSHA256(raw) != expectSHA {
			return false, false, false, fmt.Errorf("memory %s changed after it was shown; nothing was done", m.ID)
		}
	}
	agent, err = isAgentWrittenMemory(cfg, m)
	if err != nil {
		return false, false, false, err
	}
	if !agent && !force {
		return false, false, false, fmt.Errorf("refusing to undo %s: it was not written by an agent (source: %s); pass --force to remove it anyway", m.ID, m.Source)
	}
	reconcile, err = deleteMemoryFile(ctx, cfg, m)
	if err != nil {
		if _, statErr := os.Stat(m.Path); errors.Is(statErr, os.ErrNotExist) {
			// The file is gone even though the index update failed: the undo
			// happened, so the audit log must say so.
			_ = appendAgentWriteLog(cfg, time.Now(), "undo", m, "cli")
		}
		return agent, false, false, err
	}
	logged = true
	if lerr := appendAgentWriteLog(cfg, time.Now(), "undo", m, "cli"); lerr != nil {
		logged = false
		fmt.Fprintf(stderr, "warning: memory %s removed, but the undo could not be recorded in %s: %v\n", m.ID, agentWriteLogName, lerr)
	}
	return agent, logged, reconcile, nil
}

// pendingMCPProposalCount counts staged propose-mode writes with no recorded
// decision.
func pendingMCPProposalCount(cfg Config) (int, error) {
	ps, err := listMCPWriteProposals(cfg)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range ps {
		d, err := readMCPProposalDecision(cfg, p.ID)
		if err != nil {
			return 0, err
		}
		if d == nil {
			n++
		}
	}
	return n, nil
}

// mcpClientName reads clientInfo.name from an initialize request.
func mcpClientName(params json.RawMessage) string {
	var p struct {
		ClientInfo struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	return sanitizeCallerToken(p.ClientInfo.Name)
}
