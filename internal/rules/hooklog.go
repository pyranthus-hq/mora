package rules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// TSLayout is the log timestamp form. It matches score_c.py's TS_RE.
const TSLayout = "2006-01-02T15:04:05.000000Z"

// RuleRef names one active rule in a log row. H is the short hash.
type RuleRef struct {
	ID   string `json:"id"`
	H    string `json:"h"`
	Mode string `json:"mode"`
}

// Row is one hook.jsonl line. The field names are the ones Steward wrote and
// score_c.py reads (ts, session_id, tool_use_id, group, matched). There is no
// field for the command, cwd, prompt or transcript path, on purpose.
type Row struct {
	V           int       `json:"v"`
	TS          string    `json:"ts"`
	Harness     string    `json:"harness"`
	SessionID   string    `json:"session_id"`
	AgentID     *string   `json:"agent_id"`
	ToolUseID   *string   `json:"tool_use_id"`
	Group       string    `json:"group"`
	Matched     []string  `json:"matched"`
	Warned      []string  `json:"warned"`
	Holdout     []string  `json:"holdout"`
	Rules       []RuleRef `json:"rules"`
	RuleVersion string    `json:"rule_version"`
	Mora        string    `json:"mora"`
	Note        string    `json:"note"`
}

// NewRow starts a row for one action. agent_id and tool_use_id are JSON null
// when empty, as Steward wrote them. group is always the hash group.
func NewRow(now time.Time, harness string, a Action) Row {
	row := Row{
		V:         1,
		TS:        now.UTC().Format(TSLayout),
		Harness:   harness,
		SessionID: a.SessionID,
		Group:     Group(a.SessionID, a.AgentID),
		Matched:   []string{},
		Warned:    []string{},
		Holdout:   []string{},
		Rules:     []RuleRef{},
	}
	if a.AgentID != "" {
		agent := a.AgentID
		row.AgentID = &agent
	}
	if a.ToolUseID != "" {
		id := a.ToolUseID
		row.ToolUseID = &id
	}
	return row
}

// Append writes one row to the log at path. The caller swallows the error;
// the hook must never fail because of its log.
func Append(path string, row Row) error {
	for _, s := range []*[]string{&row.Matched, &row.Warned, &row.Holdout} {
		if *s == nil {
			*s = []string{}
		}
	}
	if row.Rules == nil {
		row.Rules = []RuleRef{}
	}
	body, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return appendLine(path, append(body, '\n'))
}

// appendLine creates the directory at 0700 and the file at 0600, then adds the
// line with one O_APPEND write so concurrent hooks never interleave rows.
func appendLine(path string, line []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	_, err = f.Write(line)
	return err
}
