// Package sessions reads Claude Code and Codex transcripts, strictly read
// only. It streams each file, hands typed records to a Visitor and keeps none
// of the raw text.
package sessions

import (
	"os"
	"reflect"
	"time"
)

const (
	HarnessClaude = "claude"
	HarnessCodex  = "codex"
)

// Call is one shell command an agent issued. Command is raw text: parse or
// match it, never print, log or store it.
type Call struct {
	Harness, SessionID, AgentID, ToolUseID, MessageID string
	TS, FileFirst                                     time.Time
	Command                                           string
}

// Denial is one Claude tool_result with a denial signal.
type Denial struct {
	SessionID, AgentID, ToolUseID, MessageID string
	TS, FileFirst                            time.Time
	Kind                                     string // toolDenialKind, or "interrupted" from the Bash flag
	Tool                                     string // joined from the tool_use in the same file
	Subagent                                 bool
	Shutdown                                 bool
	NextOrigin                               string // "" when no later prompt in this file
}

// Options selects what Scan reads.
type Options struct {
	Home        string              // cfg.HomeDir(), so test sandboxes hold
	Getenv      func(string) string // nil means os.Getenv
	Since       time.Time           // zero means everything on disk
	OnlySession string              // read only this session
	Workers     int                 // 0 means min(GOMAXPROCS, 8)
}

func (o Options) getenv(key string) string {
	if o.Getenv == nil {
		return os.Getenv(key)
	}
	return o.Getenv(key)
}

// Visitor receives records. Scan serializes the calls, in no defined order.
type Visitor interface {
	Call(Call)
	Denial(Denial)
}

// Stats holds counts only. Every field must stay an int, because add sums them
// by position.
type Stats struct {
	FilesRead               int `json:"files_read"`
	FilesSkippedOld         int `json:"files_skipped_old"`
	EmptyFiles              int `json:"empty_files"`
	UnreadableFiles         int `json:"unreadable_files"`
	SymlinksSkipped         int `json:"symlinks_skipped"`
	Lines                   int `json:"lines"`
	UnknownRecords          int `json:"unknown_records"`
	MalformedLines          int `json:"malformed_lines"`
	PartialLastLines        int `json:"partial_last_lines"`
	OversizeLines           int `json:"oversize_lines"`
	BadTimestamps           int `json:"bad_timestamps"`
	CallsWithoutID          int `json:"calls_without_id"`
	DenialsWithoutCall      int `json:"denials_without_call"`
	CodexCommandsUnreadable int `json:"codex_commands_unreadable"`
	CodexTurnInterrupts     int `json:"codex_turn_interrupts"`
	CodexPatchesDeclined    int `json:"codex_patches_declined"`
	InterruptedBetweenCalls int `json:"interrupted_between_calls"`
	UntypedRejections       int `json:"untyped_rejections"`
	ClaudeSessions          int `json:"claude_sessions"`
	ClaudeSubagentFiles     int `json:"claude_subagent_files"`
	CodexSessions           int `json:"codex_sessions"`
}

func (s *Stats) add(o Stats) {
	a, b := reflect.ValueOf(s).Elem(), reflect.ValueOf(o)
	for i := range a.NumField() {
		a.Field(i).SetInt(a.Field(i).Int() + b.Field(i).Int())
	}
}

func parseTS(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}
