package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Rule states, as `mora rules list` shows them. An invalid rule shows
// "invalid: <error>".
const (
	StateActive   = "active"
	StateInactive = "inactive"
	StateChanged  = "changed since approval"
	StateMissing  = "missing file"
)

// Approval is one approvals.jsonl row. The file lives in the state dir, so it
// is per device, never synced and not reachable through any MCP tool. The
// last row per rule_id wins.
type Approval struct {
	V        int    `json:"v"`
	TS       string `json:"ts"`
	RuleID   string `json:"rule_id"`
	RuleHash string `json:"rule_hash"`
	Mode     string `json:"mode"`
	Holdout  bool   `json:"holdout"`
	Via      string `json:"via"`
}

// ValidateMode accepts off, shadow and warn. block is refused for now.
func ValidateMode(mode string) error {
	switch mode {
	case ModeOff, ModeShadow, ModeWarn:
		return nil
	case "block":
		return errors.New("mode block is not supported yet; use warn or shadow")
	}
	return fmt.Errorf("mode must be off, shadow or warn, got %q", mode)
}

// AppendApproval adds one approval row with a single O_APPEND write.
func AppendApproval(stateDir string, a Approval) error {
	if !idRE.MatchString(a.RuleID) {
		return fmt.Errorf("invalid rule id %q", a.RuleID)
	}
	if err := ValidateMode(a.Mode); err != nil {
		return err
	}
	a.V = 1
	if a.TS == "" {
		a.TS = time.Now().UTC().Format(TSLayout)
	}
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return appendLine(ApprovalsPath(stateDir), append(body, '\n'))
}

// LoadApprovals returns the latest approval per rule id. Lines that do not
// decode, or carry a bad id or mode, are skipped. A missing file is empty.
func LoadApprovals(stateDir string) (map[string]Approval, error) {
	out := map[string]Approval{}
	data, err := os.ReadFile(ApprovalsPath(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var a Approval
		if json.Unmarshal(line, &a) != nil || a.V != 1 || !idRE.MatchString(a.RuleID) || ValidateMode(a.Mode) != nil {
			continue
		}
		if a.Mode != ModeOff && !strings.HasPrefix(a.RuleHash, "sha256:") {
			continue
		}
		out[a.RuleID] = a
	}
	return out, nil
}

// Check reads the vault rule behind an approval and returns its state. Only
// StateActive means the rule may be compiled: the file parses, validates and
// still hashes to the approved value.
func Check(vaultDir string, a Approval) (Rule, string) {
	r, err := ReadRule(vaultDir, a.RuleID)
	if errors.Is(err, os.ErrNotExist) {
		return Rule{}, StateMissing
	}
	if err != nil {
		return Rule{}, "invalid: " + err.Error()
	}
	if err := Validate(r); err != nil {
		return r, "invalid: " + err.Error()
	}
	if r.ID != a.RuleID {
		return r, fmt.Sprintf("invalid: file id %q does not match its file name", r.ID)
	}
	if a.Mode == ModeOff {
		return r, StateInactive
	}
	if Hash(r) != a.RuleHash {
		return r, StateChanged
	}
	return r, StateActive
}

// Compile rebuilds active.json from the latest approvals. A rule is included
// only when a human approved its exact current hash with mode shadow or warn,
// so compile can drop rules but never add one nobody approved.
func Compile(vaultDir, stateDir string) (ActiveSet, error) {
	approvals, err := LoadApprovals(stateDir)
	if err != nil {
		return ActiveSet{}, err
	}
	ids := make([]string, 0, len(approvals))
	for id := range approvals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	set := ActiveSet{Schema: ActiveSchema, CompiledAt: time.Now().UTC().Format(time.RFC3339), Rules: []ActiveRule{}}
	for _, id := range ids {
		a := approvals[id]
		r, state := Check(vaultDir, a)
		if state != StateActive {
			continue
		}
		set.Rules = append(set.Rules, ActiveRule{
			ID: r.ID, Hash: a.RuleHash, Mode: a.Mode, Holdout: a.Holdout,
			Tool: r.Spec.Tool, Match: r.Spec.Match, Exclude: r.Spec.Exclude,
			Unless: r.Spec.Unless, Context: r.Spec.Context, Message: r.Message,
		})
	}
	if len(set.Rules) > MaxActiveRules {
		return ActiveSet{}, fmt.Errorf("%d approved rules; the limit is %d. Turn some off with mora rules off", len(set.Rules), MaxActiveRules)
	}
	if err := WriteActive(ActivePath(stateDir), set); err != nil {
		return ActiveSet{}, err
	}
	return set, nil
}
