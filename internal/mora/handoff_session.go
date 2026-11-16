package mora

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"github.com/pyranthus-hq/mora/internal/memory"
)

const (
	handoffSessionIDMax           = 128
	handoffSessionScopeEntries    = 4096
	handoffSessionProposalEntries = 4096
	handoffSessionStatusEntries   = 4096
	handoffSessionAggregateBytes  = 8 << 20
	handoffSessionHeaderBytes     = 16 << 10
)

var handoffSessionEndDeadline = 1200 * time.Millisecond

// handoffSessionReceipt is state-dir-only correlation state. It deliberately
// stores no transcript text, token-bearing prompt text, or raw repository data.
type handoffSessionReceipt struct {
	Version         int    `json:"version"`
	Client          string `json:"client"`
	SessionID       string `json:"session_id"`
	Root            string `json:"root"`
	Scope           string `json:"scope"`
	TranscriptPath  string `json:"transcript_path"`
	Token           string `json:"token"`
	StartPolicy     string `json:"start_policy"`
	StartedAt       string `json:"started_at"`
	UpdatedAt       string `json:"updated_at"`
	Outcome         string `json:"outcome,omitempty"`
	MemoryID        string `json:"memory_id,omitempty"`
	ProposalID      string `json:"proposal_id,omitempty"`
	IntentID        string `json:"intent_id,omitempty"`
	Uncertainty     string `json:"uncertainty,omitempty"`
	Requests        int    `json:"requests,omitempty"`
	TranscriptState string `json:"transcript_state,omitempty"`
	StartHead       string `json:"start_head,omitempty"`
	StartWork       string `json:"start_work,omitempty"`
	EndHead         string `json:"end_head,omitempty"`
	EndWork         string `json:"end_work,omitempty"`
}

type handoffSessionStatus struct {
	Root        string `json:"root"`
	Scope       string `json:"scope"`
	UpdatedAt   string `json:"updated_at"`
	Outcome     string `json:"outcome"`
	MemoryID    string `json:"memory_id,omitempty"`
	ProposalID  string `json:"proposal_id,omitempty"`
	Uncertainty string `json:"uncertainty,omitempty"`
}

func handoffSessionStatuses(cfg Config) ([]handoffSessionStatus, bool) {
	dir := filepath.Join(cfg.StateDir, "handoff-sessions")
	entries, err := handoffSessionEntries(context.Background(), dir, handoffSessionStatusEntries)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true
	}
	if err != nil || len(entries) > handoffSessionStatusEntries {
		return nil, false
	}
	latest := map[string]handoffSessionReceipt{}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() == ".mora-lease-guards" {
			continue
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".json" {
			return nil, false
		}
		r, err := loadHandoffSession(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, false
		}
		if previous, exists := latest[r.Root]; !exists || previous.UpdatedAt < r.UpdatedAt {
			latest[r.Root] = r
		}
	}
	out := make([]handoffSessionStatus, 0, len(latest))
	for _, r := range latest {
		out = append(out, handoffSessionStatus{Root: r.Root, Scope: r.Scope, UpdatedAt: r.UpdatedAt, Outcome: r.Outcome, MemoryID: r.MemoryID, ProposalID: r.ProposalID, Uncertainty: r.Uncertainty})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Root < out[j].Root })
	return out, true
}

func handoffSessionValidID(id string) bool {
	if id == "" || len(id) > handoffSessionIDMax {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func handoffSessionKey(client, sessionID, root string) string {
	s := sha256.Sum256([]byte(client + "\x00" + sessionID + "\x00" + root))
	return hex.EncodeToString(s[:])
}

func handoffSessionPath(cfg Config, client, sessionID, root string) string {
	return filepath.Join(cfg.StateDir, "handoff-sessions", handoffSessionKey(client, sessionID, root)+".json")
}

func handoffSessionToken() (string, error) {
	b := make([]byte, 18)
	if _, err := io.ReadFull(cryptorand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func loadHandoffSession(path string) (handoffSessionReceipt, error) {
	b, err := readHandoffSessionFile(context.Background(), path, handoffSessionHeaderBytes)
	if err != nil {
		return handoffSessionReceipt{}, err
	}
	var r handoffSessionReceipt
	if err := json.Unmarshal(b, &r); err != nil || r.Version != 1 || !handoffSessionValidID(r.SessionID) || r.Token == "" || r.Root == "" || r.Scope == "" {
		if err == nil {
			err = errors.New("invalid handoff session receipt")
		}
		return handoffSessionReceipt{}, err
	}
	return r, nil
}

func saveHandoffSession(path string, r handoffSessionReceipt) error {
	r.UpdatedAt = hookNow().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicio.WriteDurable(path, append(b, '\n'), 0o600)
}

func lockHandoffSession(path string, deadline time.Time) (*os.File, error) {
	guard := leasefile.GuardPath(path)
	if err := os.MkdirAll(filepath.Dir(guard), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(guard, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for err = leasefile.TryLock(f); err != nil && time.Now().Before(deadline); err = leasefile.TryLock(f) {
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// startHandoffSession returns a token only after the receipt is durable. A new
// start after a final outcome creates a new generation rather than suppressing
// resumed work with an old saved outcome.
func startHandoffSession(cfg Config, client, sessionID, root, scope, transcript string) (handoffSessionReceipt, error) {
	if client != "claude" || !handoffSessionValidID(sessionID) || root == "" || scope == "" || transcript == "" {
		return handoffSessionReceipt{}, errors.New("invalid handoff session start")
	}
	path := handoffSessionPath(cfg, client, sessionID, root)
	lock, err := lockHandoffSession(path, time.Now().Add(handoffSessionEndDeadline))
	if err != nil {
		return handoffSessionReceipt{}, err
	}
	defer func() { _ = leasefile.Unlock(lock); _ = lock.Close() }()
	if old, err := loadHandoffSession(path); err == nil && old.Outcome == "" {
		if old.Client != client || old.Scope != scope || old.Root != root || old.TranscriptPath != transcript {
			return handoffSessionReceipt{}, errors.New("handoff session start mismatch")
		}
		return old, nil
	}
	token, err := handoffSessionToken()
	if err != nil {
		return handoffSessionReceipt{}, err
	}
	now := hookNow().UTC().Format(time.RFC3339Nano)
	r := handoffSessionReceipt{Version: 1, Client: client, SessionID: sessionID, Root: root, Scope: scope, TranscriptPath: transcript, Token: token, StartPolicy: configMCPWritePolicy(cfg), StartedAt: now}
	startCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if head, work, ok := handoffSessionGit(startCtx, root); ok {
		r.StartHead, r.StartWork = head, work
	}
	if err := saveHandoffSession(path, r); err != nil {
		return handoffSessionReceipt{}, err
	}
	return r, nil
}

func handoffSessionSource(token string) string { return "handoff-session:" + token }

func handoffSessionContentAllowed(m Memory) bool { return len(tentativeHoldReasons(m)) == 0 }

func handoffSessionIntent(token string) string {
	return "mem_handoff_" + memory.ContentHash(token)
}

func handoffSessionScopeDir(cfg Config, scope string) string {
	return filepath.Join(memoriesRoot(cfg), strings.NewReplacer(":", string(os.PathSeparator), "/", string(os.PathSeparator)).Replace(scope))
}

type handoffAssociation struct {
	state      string // match, none, unknown
	memoryID   string
	proposalID string
	otherNote  bool
}

func matchesHandoffSession(m Memory, r handoffSessionReceipt) bool {
	return m.Type == memory.TypeHandoff && m.Scope == r.Scope && m.DeletedAt == "" && m.Source == handoffSessionSource(r.Token)
}

func afterHandoffSessionStart(created, started string) bool {
	c, cerr := time.Parse(time.RFC3339Nano, created)
	s, serr := time.Parse(time.RFC3339Nano, started)
	if cerr != nil || serr != nil {
		return true // malformed time cannot establish an idle window
	}
	// Markdown commonly persists seconds; avoid rejecting a same-second row.
	if !strings.Contains(created, ".") {
		s = s.Truncate(time.Second)
	}
	return !c.Before(s)
}

func readHandoffSessionMemory(ctx context.Context, path string, total *int64) (Memory, error) {
	if ctx.Err() != nil {
		return Memory{}, ctx.Err()
	}
	f, info := openRegularNoFollow(path)
	if f == nil || !info.Mode().IsRegular() {
		return Memory{}, errors.New("unsafe memory candidate")
	}
	defer f.Close()
	scanner := bufio.NewScanner(io.LimitReader(f, handoffSessionHeaderBytes+1))
	var header bytes.Buffer
	for scanner.Scan() {
		if ctx.Err() != nil {
			return Memory{}, ctx.Err()
		}
		line := scanner.Text()
		if header.Len() == 0 && line != "---" {
			return Memory{}, errors.New("missing memory header")
		}
		closing := header.Len() > 0 && line == "---"
		header.WriteString(line + "\n")
		if header.Len() > handoffSessionHeaderBytes || *total+int64(header.Len()) > handoffSessionAggregateBytes {
			return Memory{}, errors.New("memory scan budget")
		}
		if closing {
			*total += int64(header.Len())
			return parseMemoryBytes(path, header.Bytes())
		}
	}
	return Memory{}, errors.New("incomplete memory header")
}

func readHandoffSessionFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	f, info := openRegularNoFollow(path)
	if f == nil {
		return nil, errors.New("unsafe session file")
	}
	defer f.Close()
	if info.Size() > limit {
		return nil, errors.New("session file budget")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("session file budget")
	}
	return b, ctx.Err()
}

func handoffSessionEntries(ctx context.Context, path string, limit int) ([]os.DirEntry, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("unsafe session directory")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > limit {
		return nil, errors.New("session directory budget")
	}
	return entries, ctx.Err()
}

// scanHandoffAssociation is intentionally local and bounded. It reads every
// regular scope entry within its budget; errors and overflow are unknown, never none.
func scanHandoffAssociation(ctx context.Context, cfg Config, r handoffSessionReceipt) handoffAssociation {
	if r.IntentID != "" {
		path, err := memoryPath(cfg, Memory{ID: r.IntentID, Scope: r.Scope})
		if err != nil {
			return handoffAssociation{state: "unknown"}
		}
		if _, statErr := os.Lstat(path); statErr == nil {
			var total int64
			m, readErr := readHandoffSessionMemory(ctx, path, &total)
			if readErr == nil && m.ID == r.IntentID && matchesHandoffSession(m, r) {
				return handoffAssociation{state: "match", memoryID: m.ID}
			}
			return handoffAssociation{state: "unknown"}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return handoffAssociation{state: "unknown"}
		}
	}
	dir := handoffSessionScopeDir(cfg, r.Scope)
	entries, err := handoffSessionEntries(ctx, dir, handoffSessionScopeEntries)
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
	} else if err != nil || len(entries) > handoffSessionScopeEntries {
		return handoffAssociation{state: "unknown"}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	otherNote := false
	var total int64
	for _, e := range entries {
		if ctx.Err() != nil {
			return handoffAssociation{state: "unknown"}
		}
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(dir, e.Name())
		m, err := readHandoffSessionMemory(ctx, path, &total)
		if err != nil {
			return handoffAssociation{state: "unknown"}
		}
		if matchesHandoffSession(m, r) {
			return handoffAssociation{state: "match", memoryID: m.ID}
		}
		if m.Scope == r.Scope && afterHandoffSessionStart(m.CreatedAt, r.StartedAt) {
			// An untagged concurrent note blocks only the idle skip.
			// It never claims this session's association.
			otherNote = true
		}
	}
	proposalEntries, err := handoffSessionEntries(ctx, mcpProposalDir(cfg), handoffSessionProposalEntries)
	if !errors.Is(err, os.ErrNotExist) && (err != nil || len(proposalEntries) > handoffSessionProposalEntries) {
		return handoffAssociation{state: "unknown"}
	}
	proposalBytes := int64(0)
	for _, entry := range proposalEntries {
		if ctx.Err() != nil {
			return handoffAssociation{state: "unknown"}
		}
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return handoffAssociation{state: "unknown"}
		}
		b, readErr := readHandoffSessionFile(ctx, filepath.Join(mcpProposalDir(cfg), entry.Name()), handoffSessionAggregateBytes-proposalBytes)
		if readErr != nil {
			return handoffAssociation{state: "unknown"}
		}
		proposalBytes += int64(len(b))
		var p mcppkg.Proposal
		if json.Unmarshal(b, &p) != nil || p.ID != strings.TrimSuffix(entry.Name(), ".json") {
			return handoffAssociation{state: "unknown"}
		}
		m, err := mcpMemoryFromArgs(p.Arguments, hookNow())
		if err != nil {
			return handoffAssociation{state: "unknown"}
		}
		if m.Type == memory.TypeHandoff && m.Scope == r.Scope && m.Source == handoffSessionSource(r.Token) {
			return handoffAssociation{state: "match", proposalID: p.ID}
		}
		if m.Scope == r.Scope {
			otherNote = true
		}
	}
	return handoffAssociation{state: "none", otherNote: otherNote}
}

func handoffSessionGit(ctx context.Context, root string) (head, digest string, ok bool) {
	const maxGitBytes = 512 << 10
	run := func(overrides []string, args ...string) ([]byte, error) {
		base := []string{"-c", "core.fsmonitor=false", "-c", "diff.external="}
		base = append(base, overrides...)
		cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
		cmd.Dir = root
		cmd.Env = []string{"GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + os.DevNull, "LC_ALL=C"}
		for _, key := range []string{"PATH", "SystemRoot", "WINDIR", "TMP", "TEMP", "TMPDIR"} {
			if value, exists := os.LookupEnv(key); exists {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(out, maxGitBytes+1))
		if readErr != nil || len(body) > maxGitBytes {
			_ = cmd.Process.Kill()
		}
		waitErr := cmd.Wait()
		if readErr != nil || len(body) > maxGitBytes {
			return nil, errors.New("git output budget")
		}
		return body, waitErr
	}
	filterRows, err := run(nil, "config", "--includes", "--null", "--name-only", "--get-regexp", `^filter\..*\.(clean|process|required)$`)
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(filterRows) != 0 || ctx.Err() != nil {
			return "", "", false
		}
	}
	var filters []string
	for _, key := range strings.Split(string(filterRows), "\x00") {
		if key == "" {
			continue
		}
		if !strings.HasPrefix(key, "filter.") || len(filters) > 512 {
			return "", "", false
		}
		switch {
		case strings.HasSuffix(key, ".clean"), strings.HasSuffix(key, ".process"):
			filters = append(filters, "-c", key+"=")
		case strings.HasSuffix(key, ".required"):
			filters = append(filters, "-c", key+"=false")
		default:
			return "", "", false
		}
	}
	h, err := run(filters, "rev-parse", "HEAD")
	if err != nil {
		return "", "", false
	}
	status, err := run(filters, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return "", "", false
	}
	untracked, err := run(filters, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", "", false
	}
	var untrackedMeta bytes.Buffer
	for i, raw := range bytes.Split(untracked, []byte{0}) {
		if ctx.Err() != nil || i > handoffSessionScopeEntries || untrackedMeta.Len() > maxGitBytes {
			return "", "", false
		}
		if len(raw) == 0 {
			continue
		}
		path := filepath.Join(root, string(raw))
		rel, relErr := filepath.Rel(root, path)
		info, statErr := os.Lstat(path)
		if relErr != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || statErr != nil || !info.Mode().IsRegular() {
			return "", "", false
		}
		fmt.Fprintf(&untrackedMeta, "%s\x00%d\x00%d\n", rel, info.Size(), info.ModTime().UnixNano())
	}
	staged, err := run(filters, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--binary")
	if err != nil {
		return "", "", false
	}
	diff, err := run(filters, "diff", "--no-ext-diff", "--no-textconv", "--binary")
	if err != nil {
		return "", "", false
	}
	sum := sha256.Sum256(append(append(append(status, untrackedMeta.Bytes()...), staged...), diff...))
	head = strings.TrimSpace(string(h))
	decoded, err := hex.DecodeString(head)
	if err != nil || len(decoded) != 20 && len(decoded) != 32 {
		return "", "", false
	}
	return head, hex.EncodeToString(sum[:]), true
}

func transcriptSessionRequests(path, root, wantID string, n int) (out []string, complete bool, state string) {
	f, info := openRegularNoFollow(path)
	if f == nil {
		return nil, false, "transcript_unavailable"
	}
	f.Close()
	lines, ok := transcriptTailLines(path)
	if !ok {
		return nil, false, "transcript_unavailable"
	}
	complete = info.Size() <= handoffTailBytes
	state = "complete"
	if !complete {
		state = "transcript_truncated"
	}
	for _, line := range lines {
		var envelope struct {
			SessionID  string `json:"session_id"`
			SessionIDC string `json:"sessionId"`
		}
		if json.Unmarshal(line, &envelope) != nil {
			complete, state = false, "transcript_malformed"
			continue
		}
		if envelope.SessionID != wantID && envelope.SessionIDC != wantID {
			complete, state = false, "transcript_unattributed"
			continue
		}
		text, cwd, accepted := typedRequest(line)
		if accepted && hookWithinProject(root, cwd) {
			out = append(out, text)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, complete, state
}

func handoffSessionCapture(ctx context.Context, cfg Config, r handoffSessionReceipt) handoffSessionReceipt {
	deadlineAt := time.Now().Add(handoffSessionEndDeadline)
	if earlier, ok := ctx.Deadline(); ok && earlier.Before(deadlineAt) {
		deadlineAt = earlier
	}
	deadline, cancel := context.WithDeadline(ctx, deadlineAt)
	defer cancel()
	if deadline.Err() != nil {
		r.Outcome, r.Uncertainty = "timeout", "before_lease"
		return r
	}
	path := handoffSessionPath(cfg, r.Client, r.SessionID, r.Root)
	f, err := lockHandoffSession(path, deadlineAt)
	if err != nil {
		r.Outcome, r.Uncertainty = "timeout", "lease_busy"
		return r
	}
	defer func() { _ = leasefile.Unlock(f); _ = f.Close() }()
	if fresh, loadErr := loadHandoffSession(path); loadErr == nil {
		if fresh.Token != r.Token || fresh.Client != r.Client || fresh.SessionID != r.SessionID || fresh.Root != r.Root || fresh.Scope != r.Scope || fresh.TranscriptPath != r.TranscriptPath {
			return fresh
		}
		r = fresh // never finalize against a stale receipt loaded before the lease
	} else {
		return r
	}
	defer func() { _ = saveHandoffSession(path, r) }()
	if r.Outcome == "refused" || r.Outcome == "idle" {
		return r // a rejected generation never reopens when policy later loosens
	}

	assoc := scanHandoffAssociation(deadline, cfg, r)
	switch assoc.state {
	case "unknown":
		r.Outcome, r.Uncertainty = "association_unknown", "scan_incomplete"
		if deadline.Err() != nil {
			r.Outcome, r.Uncertainty = "timeout", "scan_deadline"
		}
		return r
	case "match":
		if assoc.memoryID != "" {
			if r.Outcome != "index_lag" || r.MemoryID != assoc.memoryID {
				r.Outcome = "saved"
			}
			r.MemoryID = assoc.memoryID
		} else {
			r.Outcome, r.ProposalID = "pending", assoc.proposalID
		}
		return r
	}
	if r.ProposalID != "" {
		r.Outcome, r.Uncertainty = "association_unknown", "proposal_missing"
		return r
	}
	if deadline.Err() != nil {
		r.Outcome, r.Uncertainty = "timeout", "before_capture"
		return r
	}
	requests, complete, transcriptState := transcriptSessionRequests(r.TranscriptPath, r.Root, r.SessionID, handoffMaxRequests)
	r.Requests, r.TranscriptState = len(requests), transcriptState
	if head, work, ok := handoffSessionGit(deadline, r.Root); ok {
		r.EndHead, r.EndWork = head, work
	} else {
		r.Uncertainty = "repo_unavailable"
	}
	if complete && !assoc.otherNote && len(requests) < 2 && r.StartHead != "" && r.StartHead == r.EndHead && r.StartWork != "" && r.StartWork == r.EndWork {
		r.Outcome = "idle"
		return r
	}

	current, cfgErr := loadConfigFor(deadline)
	if cfgErr != nil || current.VaultDir != cfg.VaultDir || current.StateDir != cfg.StateDir {
		r.Outcome, r.Uncertainty = "refused", "configuration_changed"
		return r
	}
	scope, root, bound, bindErr := lookupProject(current, r.Root)
	if bindErr != nil || !bound || root != r.Root || scope != r.Scope {
		r.Outcome, r.Uncertainty = "refused", "binding_changed"
		return r
	}
	cfg = current
	if deadline.Err() != nil {
		r.Outcome, r.Uncertainty = "timeout", "before_intent"
		return r
	}
	r.IntentID = handoffSessionIntent(r.Token)
	if err := saveHandoffSession(path, r); err != nil {
		r.Outcome, r.Uncertainty = "error", "intent"
		return r
	}
	policy := agentWritePolicy(r.StartPolicy, configMCPWritePolicy(cfg))
	cleaned := make([]string, 0, len(requests))
	for _, request := range requests {
		if clean, cleanOK := handoffClean(request); cleanOK {
			cleaned = append(cleaned, clean)
		}
	}
	withheld := len(requests) - len(cleaned)
	lines := []string{fmt.Sprintf("Automatic session-end handoff. Requests observed: %d; outcomes unknown.", len(cleaned))}
	for _, request := range cleaned {
		lines = append(lines, `- "`+request+`"`)
	}
	if withheld > 0 {
		lines = append(lines, fmt.Sprintf("- %d request(s) withheld by safety checks.", withheld))
	}
	change := "unknown"
	if r.StartWork != "" && r.EndWork != "" && r.StartHead != "" && r.EndHead != "" {
		change = "unchanged"
		if r.StartWork != r.EndWork || r.StartHead != r.EndHead {
			change = "changed"
		}
	}
	if r.EndHead != "" {
		lines = append(lines, "Observed HEAD: "+r.EndHead+".")
	}
	lines = append(lines, "Repository change since start: "+change+". Authorship and PR state unknown.")
	summary := strings.Join(lines, "\n")
	args := map[string]any{
		"scope": r.Scope, "type": memory.TypeHandoff, "source": handoffSessionSource(r.Token),
		"title": "Session handoff", "text": summary,
	}
	m, err := mcpMemoryFromArgs(args, hookNow())
	if err != nil {
		r.Outcome, r.Uncertainty = "error", "assembled_invalid"
		return r
	}
	holds := tentativeHoldReasons(m)
	action, actionErr := mcppkg.MutationAction(policy, "write_memory")
	if actionErr != nil || action == mcppkg.ActionRefuse {
		r.Outcome, r.Uncertainty = "refused", "policy"
		return r
	}
	if action == mcppkg.ActionExecute && len(holds) > 0 {
		r.Outcome, r.Uncertainty = "refused", "content_held"
		return r
	}
	if deadline.Err() != nil {
		r.Outcome, r.Uncertainty = "timeout", "before_dispatch"
		return r
	}
	proposing := action == mcppkg.ActionPropose || action == mcppkg.ActionTentative && len(holds) > 0
	var value any
	switch {
	case proposing:
		value, err = stageMCPWriteProposalHeld(cfg, args, "", holds)
	case action == mcppkg.ActionTentative:
		value, err = mcpWriteMemoryWith(deadline, cfg, args, withTentative(), withExplicitMemoryID(r.IntentID))
	default:
		value, err = mcpWriteMemoryWith(deadline, cfg, args, withExplicitMemoryID(r.IntentID))
	}
	if err != nil {
		if deadline.Err() != nil {
			r.Outcome, r.Uncertainty = "timeout", "dispatch"
		} else {
			r.Outcome, r.Uncertainty = "error", "dispatch"
		}
		return r
	}
	if proposing {
		if result, ok := value.(map[string]any); ok {
			if proposal, ok := result["proposal"].(map[string]any); ok {
				r.ProposalID, _ = proposal["id"].(string)
			}
		}
		if r.ProposalID == "" {
			r.Outcome, r.Uncertainty = "error", "proposal_receipt"
			return r
		}
		r.Outcome = "pending"
		return r
	}
	storedPath, pathErr := memoryPath(cfg, Memory{ID: r.IntentID, Scope: r.Scope})
	var total int64
	stored, readErr := readHandoffSessionMemory(deadline, storedPath, &total)
	if pathErr != nil || readErr != nil || stored.ID != r.IntentID || !matchesHandoffSession(stored, r) {
		r.Outcome, r.Uncertainty = "association_unknown", "publication_unverified"
		return r
	}
	r.MemoryID, r.Outcome = r.IntentID, "saved"
	if result, ok := value.(map[string]any); ok {
		if stale, _ := result["index_stale"].(bool); stale {
			r.Outcome = "index_lag"
		}
	}
	return r
}

func markHandoffSessionUnattributed(cfg Config, path string) {
	lock, err := lockHandoffSession(path, time.Now().Add(handoffSessionEndDeadline))
	if err != nil {
		return
	}
	defer func() { _ = leasefile.Unlock(lock); _ = lock.Close() }()
	r, err := loadHandoffSession(path)
	if err != nil || r.Outcome != "" {
		return
	}
	r.Outcome, r.Uncertainty = "unattributed", "end_mismatch"
	_ = saveHandoffSession(path, r)
}

func sessionID(start sessionStartHookInput) string {
	return start.SessionID
}

func sessionEndID(end sessionEndHookInput) string {
	return end.SessionID
}

func handoffSessionContext(token string) string {
	return "Mora session handoff correlation: if you write a handoff with MCP write_memory, use type handoff and source " + handoffSessionSource(token) + "."
}

func handoffSessionResolvedTranscript(cfg Config, path string) (string, bool) {
	if !handoffTranscriptOK(cfg, path) {
		return "", false
	}
	p, err := filepath.EvalSymlinks(path)
	return p, err == nil
}
