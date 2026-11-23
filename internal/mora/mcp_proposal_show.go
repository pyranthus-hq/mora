package mora

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// JSON encoding can expand a maximum-size MCP request by six bytes per character.
const maxProposalInspectionBytes = mcpMaxRequestBytes * 8

var proposalInspectionID = regexp.MustCompile(`^p_[A-Za-z0-9_-]+$`)
var proposalCredentialPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), `[REDACTED]`},
	{regexp.MustCompile(`(?i)(https?://)[^/\s@]+@`), `${1}[REDACTED]@`},
	{regexp.MustCompile(`(?i)\b(Bearer|Basic)[ \t]+[A-Za-z0-9._~+/-]+=*`), `${1} [REDACTED]`},
	{regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{10,}|gh[opusr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|(?:AKIA|ASIA)[A-Z0-9]{16}|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)\b`), `[REDACTED]`},
	{regexp.MustCompile(`(?is)\b((?:[a-z0-9]+[_-])*(?:api[_-]?key|password|passwd|passphrase|secret|secret[_-]?(?:access[_-]?)?key|access[_-]?key[_-]?id|(?:access|refresh|auth|id)[_-]?token|client[_-]?secret|token|authorization)["']?[ \t]*[:=][ \t]*)(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;&]+)`), `${1}[REDACTED]`},
}

type mcpProposalInspection struct {
	ID         string         `json:"id"`
	ProposedAt string         `json:"proposed_at"`
	Status     string         `json:"status"`
	Scope      string         `json:"effective_scope"`
	Type       string         `json:"effective_type"`
	Source     string         `json:"effective_source"`
	Arguments  map[string]any `json:"arguments"`
	Redacted   bool           `json:"redacted"`
	Notice     string         `json:"notice,omitempty"`
	MemoryID   string         `json:"memory_id,omitempty"`
}

func readMCPProposalForInspection(cfg Config, id string) (mcpWriteProposal, time.Time, error) {
	if len(id) > 128 || !proposalInspectionID.MatchString(id) {
		return mcpWriteProposal{}, time.Time{}, errors.New("invalid MCP proposal ID; use an ID from mora mcp proposals list")
	}
	path, err := mcppkg.ProposalPath(cfg, id)
	if err != nil {
		return mcpWriteProposal{}, time.Time{}, errors.New("invalid MCP proposal ID")
	}
	dir, err := os.Lstat(mcpProposalDir(cfg))
	if err != nil || !dir.IsDir() {
		return mcpWriteProposal{}, time.Time{}, errors.New("pending proposal directory is missing or is not a regular directory")
	}
	file, err := os.Lstat(path)
	if err != nil || !file.Mode().IsRegular() {
		return mcpWriteProposal{}, time.Time{}, errors.New("pending proposal is missing or is not a regular file")
	}
	if file.Size() > maxProposalInspectionBytes {
		return mcpWriteProposal{}, time.Time{}, errors.New("proposal exceeds the inspection size limit")
	}
	root, err := os.OpenRoot(mcpProposalDir(cfg))
	if err != nil {
		return mcpWriteProposal{}, time.Time{}, errors.New("pending proposal directory is missing or is not a regular directory")
	}
	defer root.Close()
	openedDir, err := root.Stat(".")
	if err != nil || !os.SameFile(dir, openedDir) {
		return mcpWriteProposal{}, time.Time{}, errors.New("pending proposal directory is missing or is not a regular directory")
	}
	opened, err := root.Open(id + ".json")
	if err != nil {
		return mcpWriteProposal{}, time.Time{}, errors.New("cannot inspect invalid or unreadable MCP proposal")
	}
	defer opened.Close()
	body, err := readReviewSnapshot(opened, file, maxProposalInspectionBytes)
	if errors.Is(err, errReviewSnapshotTooLarge) {
		return mcpWriteProposal{}, time.Time{}, errors.New("proposal exceeds the inspection size limit")
	}
	if err != nil {
		return mcpWriteProposal{}, time.Time{}, errors.New("cannot inspect invalid or unreadable MCP proposal")
	}
	p, err := mcppkg.ParseProposal(id, body)
	if err != nil {
		return mcpWriteProposal{}, time.Time{}, errors.New("cannot inspect invalid or unreadable MCP proposal")
	}
	proposedAt, err := time.Parse(time.RFC3339, p.ProposedAt)
	if err != nil {
		return mcpWriteProposal{}, time.Time{}, errors.New("cannot inspect proposal with an invalid timestamp")
	}
	return p, proposedAt, nil
}

var errReviewSnapshotTooLarge = errors.New("review record exceeds its size limit")

// readReviewSnapshot verifies the opened file and caps the read even if the
// file grows after stat. The caller owns the handle.
func readReviewSnapshot(file *os.File, expected os.FileInfo, limit int64) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(expected, info) {
		return nil, errors.New("review record changed before inspection")
	}
	if info.Size() > limit {
		return nil, errReviewSnapshotTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errReviewSnapshotTooLarge
	}
	return body, nil
}

func showMCPWriteProposal(cfg Config, id string, stdout io.Writer) error {
	p, proposedAt, err := readMCPProposalForInspection(cfg, id)
	if err != nil {
		return err
	}
	memory, err := mcpMemoryFromArgs(p.Arguments, proposedAt)
	if err != nil {
		return errors.New("cannot inspect proposal with invalid write arguments")
	}
	arguments, redacted := redactProposalValue(p.Arguments)
	scope, scopeRedacted := redactProposalText(memory.Scope)
	kind, typeRedacted := redactProposalText(memory.Type)
	source, sourceRedacted := redactProposalText(memory.Source)
	inspection := mcpProposalInspection{ID: p.ID, ProposedAt: p.ProposedAt, Status: "pending", Scope: scope, Type: kind, Source: source, Arguments: arguments.(map[string]any), Redacted: redacted || scopeRedacted || typeRedacted || sourceRedacted}
	if inspection.Redacted {
		inspection.Notice = "Credential-like values were hidden. Reject rather than approve this redacted view; the stored proposal is unchanged."
	}
	d, err := readMCPProposalDecision(cfg, id)
	if err != nil {
		return err
	}
	if d != nil {
		inspection.Status, inspection.MemoryID = d.Status, d.MemoryID
		if inspection.Notice != "" {
			inspection.Notice += " "
		}
		inspection.Notice += "A decision has already been attempted. Do not approve again; if it is unresolved, run `mora mcp proposals recover " + id + "`."
	}
	var encoded bytes.Buffer
	if err := emitReceipt(&encoded, "mora.mcp.proposals", 1, mcpProposalsPayload{Proposals: []mcpProposalRow{}, Inspection: &inspection}); err != nil {
		return err
	}
	// Keep JSON round-trippable while making C1 and bidirectional controls inert in terminals.
	var safe strings.Builder
	for _, r := range encoded.String() {
		if unicode.IsControl(r) && r != '\n' && r != '\t' || unicode.Is(unicode.Cf, r) {
			if r <= 0xffff {
				fmt.Fprintf(&safe, `\u%04x`, r)
			} else {
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(&safe, `\u%04x\u%04x`, hi, lo)
			}
		} else {
			safe.WriteRune(r)
		}
	}
	_, err = io.WriteString(stdout, safe.String())
	return err
}

func redactProposalText(text string) (string, bool) {
	original := text
	for _, pattern := range proposalCredentialPatterns {
		text = pattern.pattern.ReplaceAllString(text, pattern.replacement)
	}
	return text, text != original
}

func redactProposalValue(value any) (any, bool) {
	switch v := value.(type) {
	case string:
		return redactProposalText(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		redacted := false
		for key, value := range v {
			safeKey, keyRedacted := redactProposalText(key)
			redacted = redacted || keyRedacted
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(key))
			sensitive := false
			for _, suffix := range []string{"password", "passwd", "passphrase", "apikey", "accesskey", "accesskeyid", "secretkey", "privatekey", "secret", "token", "authorization", "credential", "credentials"} {
				if strings.HasSuffix(normalized, suffix) {
					sensitive = true
					break
				}
			}
			if sensitive {
				out[safeKey] = "[REDACTED]"
				redacted = true
				continue
			}
			safe, changed := redactProposalValue(value)
			out[safeKey] = safe
			redacted = redacted || changed
		}
		return out, redacted
	case []any:
		out := make([]any, len(v))
		redacted := false
		for i, value := range v {
			safe, changed := redactProposalValue(value)
			out[i] = safe
			redacted = redacted || changed
		}
		return out, redacted
	default:
		return value, false
	}
}
