package rules

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	ActiveSchema   = "mora.rules.active.v1"
	MaxActiveBytes = 256 << 10
	MaxActiveRules = 200
)

// Modes an approval can set. block is not supported yet.
const (
	ModeOff    = "off"
	ModeShadow = "shadow"
	ModeWarn   = "warn"
)

// Dir is <StateDir>/rules, which holds active.json, approvals.jsonl and
// hook.jsonl. It is per device and never synced.
func Dir(stateDir string) string { return filepath.Join(stateDir, "rules") }

func ActivePath(stateDir string) string    { return filepath.Join(Dir(stateDir), "active.json") }
func ApprovalsPath(stateDir string) string { return filepath.Join(Dir(stateDir), "approvals.jsonl") }
func LogPath(stateDir string) string       { return filepath.Join(Dir(stateDir), "hook.jsonl") }

// ActiveRule is one compiled rule as the hook reads it.
type ActiveRule struct {
	ID      string `json:"id"`
	Hash    string `json:"hash"`
	Mode    string `json:"mode"`
	Holdout bool   `json:"holdout"`
	Tool    string `json:"tool"`
	Match   string `json:"match"`
	Exclude string `json:"exclude"`
	Unless  string `json:"unless"`
	Context string `json:"context"`
	Message string `json:"message"`
	Expires string `json:"expires,omitempty"`
}

// Expired reports whether a rule's grant has run out. The hook checks this on
// every call, because active.json is only rebuilt by a compile.
func (r ActiveRule) Expired(now time.Time) bool { return expiredAt(r.Expires, now) }

// Spec rebuilds the rule spec, so the hook compiles it with NewMatcher.
func (r ActiveRule) Spec() Spec {
	return Spec{Version: 1, Tool: r.Tool, Match: r.Match, Exclude: r.Exclude, Unless: r.Unless, Context: r.Context}
}

// ActiveSet is active.json. Version is not stored in the file: LoadActive
// fills it with "mora-active:" and the first 12 hex digits of the sha256 of
// the file bytes, for the log's rule_version field.
type ActiveSet struct {
	Schema     string       `json:"schema"`
	CompiledAt string       `json:"compiled_at"`
	Rules      []ActiveRule `json:"rules"`
	Version    string       `json:"-"`
}

// LoadActive reads active.json with hard bounds: at most MaxActiveBytes and
// MaxActiveRules. present is false only when the file does not exist. Any
// other failure returns an empty set with present true and an error.
func LoadActive(path string) (ActiveSet, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ActiveSet{}, false, nil
	}
	if err != nil {
		return ActiveSet{}, true, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, MaxActiveBytes+1))
	if err != nil {
		return ActiveSet{}, true, err
	}
	if len(body) > MaxActiveBytes {
		return ActiveSet{}, true, fmt.Errorf("active rules file is larger than %d bytes", MaxActiveBytes)
	}
	var set ActiveSet
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&set); err != nil {
		return ActiveSet{}, true, fmt.Errorf("active rules file: %w", err)
	}
	if dec.More() {
		return ActiveSet{}, true, errors.New("active rules file has trailing data")
	}
	if set.Schema != ActiveSchema {
		return ActiveSet{}, true, fmt.Errorf("active rules file schema is %q, want %q", set.Schema, ActiveSchema)
	}
	if len(set.Rules) > MaxActiveRules {
		return ActiveSet{}, true, fmt.Errorf("active rules file has %d rules; the limit is %d", len(set.Rules), MaxActiveRules)
	}
	sum := sha256.Sum256(body)
	set.Version = "mora-active:" + hex.EncodeToString(sum[:])[:12]
	return set, true, nil
}

// WriteActive publishes the set atomically at mode 0600.
func WriteActive(path string, set ActiveSet) error {
	set.Schema = ActiveSchema
	if set.Rules == nil {
		set.Rules = []ActiveRule{}
	}
	body, err := json.Marshal(set)
	if err != nil {
		return err
	}
	return atomicio.Write(path, append(body, '\n'), 0o600)
}
