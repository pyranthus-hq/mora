// mora-bot-return transports the Grok/project:mora pilot into the owner's proposal queue.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"github.com/pyranthus-hq/mora/internal/mcp"
)

const maxBatch = 2 << 20

var proposalID = regexp.MustCompile(`^p_[A-Za-z0-9_-]{1,120}$`)

type remoteProposal struct {
	ID         string         `json:"id"`
	ProposedAt string         `json:"proposed_at"`
	ProposedBy string         `json:"proposed_by"`
	Arguments  map[string]any `json:"arguments"`
}
type batch struct {
	Version   int              `json:"version"`
	Proposals []remoteProposal `json:"proposals"`
	Problems  []string         `json:"problems,omitempty"`
}
type receipt struct {
	RemoteID string `json:"remote_id"`
	Digest   string `json:"digest"`
	Status   string `json:"status"`
}
type result struct {
	Imported []string `json:"imported"`
	Seen     int      `json:"already_seen"`
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mora-bot-return:", err)
		os.Exit(1)
	}
}
func run(args []string, in io.Reader, out io.Writer) (ret error) {
	if len(args) != 1 || (args[0] != "export" && args[0] != "import" && args[0] != "seen") {
		return errors.New("usage: mora-bot-return export|import|seen (Grok/project:mora only)")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if args[0] == "seen" {
		return emitSeen(cfg, out)
	}
	if args[0] == "export" {
		body, err := io.ReadAll(io.LimitReader(in, maxBatch+1))
		if err != nil {
			return err
		}
		if len(body) > maxBatch {
			return errors.New("receipt manifest too large")
		}
		seen := map[string]string{}
		if len(bytes.TrimSpace(body)) > 0 {
			if err := decode(body, &seen); err != nil {
				return err
			}
		}
		b, err := exportSeen(cfg, seen)
		if err != nil {
			return err
		}
		body, err = json.Marshal(b)
		if err != nil {
			return err
		}
		if len(body) > maxBatch {
			return errors.New("export exceeds batch limit")
		}
		_, err = out.Write(append(body, '\n'))
		return err
	}
	root := os.Getenv("MORA_TENTACLES_STATE_DIR")
	if root == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		root = filepath.Join(h, ".local/state/mora/tentacles")
	}
	statusPath := filepath.Join(root, "bot-return.json")
	status := map[string]any{"schema": 1, "job": "bot-return", "state": "running", "started_at": time.Now().UTC().Format(time.RFC3339), "operations": 0}
	if old, err := readFile(statusPath, 8192); err == nil {
		var prev map[string]any
		if json.Unmarshal(old, &prev) == nil {
			status["last_success"] = prev["last_success"]
		}
	}
	writeStatus := func() error {
		b, err := json.Marshal(status)
		if err != nil {
			return err
		}
		return atomicio.WriteDurable(statusPath, append(b, '\n'), 0600)
	}
	if err := writeStatus(); err != nil {
		return err
	}
	defer func() {
		status["finished_at"] = time.Now().UTC().Format(time.RFC3339)
		if ret != nil {
			status["state"] = "failed"
			status["detail"] = "Proposal intake failed; inspect the bot-return job log."
		} else {
			status["state"] = "succeeded"
			status["last_success"] = status["finished_at"]
			status["operations"] = 1
			status["detail"] = "Proposal intake completed. Notes await owner approval; this is not vault publication."
		}
		ret = errors.Join(ret, writeStatus())
	}()
	body, err := io.ReadAll(io.LimitReader(in, maxBatch+1))
	if err != nil {
		return err
	}
	if len(body) > maxBatch {
		return errors.New("import exceeds batch limit")
	}
	var b batch
	if err := decode(body, &b); err != nil {
		return err
	}
	r, err := intake(cfg, b)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(r)
}
func decode(body []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON content")
	}
	return nil
}
func readFile(path string, limit int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("invalid or oversized transport file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if len(b) > int(limit) {
		return nil, errors.New("transport file grew beyond limit")
	}
	return b, err
}
func validate(p remoteProposal) error {
	if !proposalID.MatchString(p.ID) || p.ProposedBy != "grok" || p.Arguments["scope"] != "project:mora" || p.Arguments["source"] != "agent:grok" {
		return errors.New("proposal outside Grok/project:mora pilot")
	}
	if _, err := time.Parse(time.RFC3339Nano, p.ProposedAt); err != nil {
		return errors.New("invalid proposal timestamp")
	}
	for key, raw := range p.Arguments {
		switch key {
		case "scope", "source", "title", "text", "type", "as_of", "durability", "flip_conditions", "review_by":
		default:
			return fmt.Errorf("unsupported proposal argument %q", key)
		}
		s, ok := raw.(string)
		if !ok || len(s) > 32768 || strings.ContainsRune(s, 0) {
			return errors.New("invalid proposal argument")
		}
		for _, r := range s {
			if unicode.IsControl(r) && r != '\n' && r != '\t' {
				return errors.New("control character in proposal")
			}
		}
	}
	for _, key := range []string{"title", "text"} {
		s, _ := p.Arguments[key].(string)
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("missing %s", key)
		}
	}
	switch p.Arguments["type"] {
	case "fact", "insight", "decision":
	default:
		return errors.New("pilot accepts only facts, insights and decisions")
	}
	if p.Arguments["type"] != "decision" {
		for _, k := range []string{"as_of", "durability", "flip_conditions", "review_by"} {
			if s, _ := p.Arguments[k].(string); s != "" {
				return errors.New("validity fields require decision")
			}
		}
	}
	return nil
}
func decisionPath(cfg config.Config, id string) string {
	return filepath.Join(cfg.StateDir, "mcp-proposal-decisions", fmt.Sprintf("%x.json", sha256.Sum256([]byte(id))))
}
func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
func export(cfg config.Config) (batch, error) {
	return exportSeen(cfg, nil)
}
func exportSeen(cfg config.Config, seen map[string]string) (batch, error) {
	b := batch{Version: 1, Proposals: []remoteProposal{}}
	entries, err := os.ReadDir(mcp.ProposalDir(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return b, err
	}
	if len(entries) > 4096 {
		return b, errors.New("proposal queue exceeds scan limit")
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		body, err := readFile(filepath.Join(mcp.ProposalDir(cfg), e.Name()), 256<<10)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} // An owner may decide a proposal during export.
		if err != nil {
			b.Problems = append(b.Problems, "Unreadable or oversized queue item")
			continue
		}
		var p remoteProposal
		if err := json.Unmarshal(body, &p); err != nil {
			b.Problems = append(b.Problems, "Malformed queue item")
			continue
		}
		if p.ProposedBy != "grok" || p.Arguments["scope"] != "project:mora" {
			continue
		}
		if err := decode(body, &p); err != nil {
			b.Problems = append(b.Problems, "Unsupported Grok proposal format")
			continue
		}
		if err := validate(p); err != nil {
			b.Problems = append(b.Problems, "Grok proposal outside pilot validation")
			continue
		}
		if e.Name() != p.ID+".json" {
			b.Problems = append(b.Problems, "Proposal filename mismatch")
			continue
		}
		canonical, _ := json.Marshal(p)
		if digest, ok := seen[p.ID]; ok && digest == fmt.Sprintf("%x", sha256.Sum256(canonical)) {
			continue
		}
		decided, err := exists(decisionPath(cfg, p.ID))
		if err != nil {
			return b, err
		}
		if decided {
			continue
		}
		// Leave room for envelope diagnostics; the next poll excludes delivered receipts.
		candidate := append(b.Proposals, p)
		encoded, _ := json.Marshal(candidate)
		if len(candidate) > 128 || len(encoded) > maxBatch/2 {
			break
		}
		b.Proposals = candidate
	}
	return b, nil
}
func localID(remoteID string) string {
	return fmt.Sprintf("p_return_%x", sha256.Sum256([]byte("windows-gateway/grok/"+remoteID)))
}
func intake(cfg config.Config, b batch) (result, error) {
	r := result{Imported: []string{}}
	// Require the explicit owner-approved pilot policy, including on older CLI installs.
	if cfg.MCPWritePolicy != "propose" {
		return r, errors.New("bot return requires explicit mcp_write_policy=propose")
	}
	if b.Version != 1 || b.Proposals == nil || len(b.Proposals) > 128 {
		return r, errors.New("invalid transport envelope")
	}
	ids := map[string]bool{}
	for _, p := range b.Proposals {
		if err := validate(p); err != nil {
			return r, err
		}
		if ids[p.ID] {
			return r, errors.New("duplicate proposal ID in batch")
		}
		ids[p.ID] = true
	}
	dir := filepath.Join(cfg.StateDir, "bot-return")
	err := leasefile.WithGuard(filepath.Join(dir, "intake"), func() error {
		if err := atomicio.SyncDirFn(cfg.StateDir); err != nil {
			return err
		}
		var failures []error
		for _, p := range b.Proposals {
			if err := func() error {
				id := localID(p.ID)
				body, _ := json.Marshal(p)
				digest := fmt.Sprintf("%x", sha256.Sum256(body))
				rp := filepath.Join(dir, id+".json")
				prior, err := readFile(rp, 8192)
				if err == nil {
					var old receipt
					if decode(prior, &old) != nil || old.RemoteID != p.ID || old.Digest != digest || (old.Status != "claimed" && old.Status != "queued") {
						return errors.New("return receipt conflict; inspect manually")
					}
					if old.Status == "claimed" {
						return errors.New("interrupted intake; inspect receipt and local proposal before recovery")
					}
					r.Seen++
					return nil
				}
				if !errors.Is(err, os.ErrNotExist) {
					return err
				}
				path, err := mcp.ProposalPath(cfg, id)
				if err != nil {
					return err
				}
				for _, path := range []string{path, decisionPath(cfg, id)} {
					yes, err := exists(path)
					if err != nil {
						return err
					}
					if yes {
						return errors.New("untracked local proposal or decision; inspect manually")
					}
				}
				claim := receipt{RemoteID: p.ID, Digest: digest, Status: "claimed"}
				if err := saveReceipt(rp, claim); err != nil {
					return err
				}
				// A permanent claim precedes staging; a crash never silently restages an already decided note.
				local := mcp.Proposal{ID: id, ProposedAt: p.ProposedAt, Arguments: p.Arguments}
				pending, _ := json.MarshalIndent(local, "", "  ")
				if err := atomicio.WriteDurable(path, append(pending, '\n'), 0600); err != nil {
					return err
				}
				claim.Status = "queued"
				if err := saveReceipt(rp, claim); err != nil {
					return err
				}
				r.Imported = append(r.Imported, id)
				return nil
			}(); err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", p.ID, err))
			}
		}
		if len(b.Problems) > 0 {
			failures = append(failures, fmt.Errorf("gateway reported %d invalid queue items; inspect gateway proposals", len(b.Problems)))
		}
		return errors.Join(failures...)
	})
	return r, err
}

func emitSeen(cfg config.Config, out io.Writer) error {
	dir := filepath.Join(cfg.StateDir, "bot-return")
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	seen := map[string]string{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "p_return_") || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		body, err := readFile(filepath.Join(dir, e.Name()), 8192)
		if err != nil {
			return err
		}
		var r receipt
		if decode(body, &r) != nil || !proposalID.MatchString(r.RemoteID) || len(r.Digest) != 64 {
			return errors.New("invalid return receipt")
		}
		if r.Status == "queued" {
			seen[r.RemoteID] = r.Digest
		}
	}
	body, err := json.Marshal(seen)
	if err != nil {
		return err
	}
	if len(body) > maxBatch {
		return errors.New("receipt manifest too large")
	}
	_, err = out.Write(append(body, '\n'))
	return err
}
func saveReceipt(path string, r receipt) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(path, append(b, '\n'), 0600)
}
