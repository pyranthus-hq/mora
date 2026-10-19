package google

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// authHistoryFile is the append-only JSONL ledger of auth/reauth events, one
// event per line. It lives alongside the token files (under ~/.config/mora) so a
// reauth is visible even though tokens last weeks and a successful refresh is
// otherwise silent.
const authHistoryFile = "auth-history.jsonl"

// AuthEvent records a single completed Google auth/reauth for an account.
type AuthEvent struct {
	Account string    `json:"account"`
	At      time.Time `json:"at"`
}

// RecordAuth appends one AuthEvent line to <dir>/auth-history.jsonl. The ledger
// is append-only (JSONL): one event per line, so a later LastAuth scan can pick
// the most recent. Best-effort callers may ignore the error.
func RecordAuth(dir, account string, at time.Time) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	line, err := json.Marshal(AuthEvent{Account: account, At: at})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(filepath.Join(dir, authHistoryFile),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return err
	}
	return nil
}

// LastAuth scans <dir>/auth-history.jsonl and returns the latest At for account.
// An empty account matches every account (latest across all). A missing history
// file is NOT an error: it returns (zero, false, nil). The bool is false when no
// matching event exists.
func LastAuth(dir, account string) (time.Time, bool, error) {
	return lastAuth(dir, account, false)
}

// LastAuthStrict has LastAuth's missing-file and account semantics, but reports
// malformed or incomplete ledger rows instead of silently treating them as
// absent history. Diagnostics must not infer a legacy credential from corruption.
// Errors contain no ledger content.
func LastAuthStrict(dir, account string) (time.Time, bool, error) {
	return lastAuth(dir, account, true)
}

func lastAuth(dir, account string, strict bool) (time.Time, bool, error) {
	f, err := os.Open(filepath.Join(dir, authHistoryFile))
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	defer f.Close()

	var (
		latest time.Time
		found  bool
	)
	sc := bufio.NewScanner(f)
	// Auth lines are tiny, but raise the buffer so a stray long line can't abort
	// the scan and silently hide newer events.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNumber := 0
	for sc.Scan() {
		lineNumber++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev AuthEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			if strict {
				return time.Time{}, false, fmt.Errorf("invalid auth history row %d", lineNumber)
			}
			// Legacy callers tolerate a corrupt/partial line.
			continue
		}
		if strict && (ev.Account == "" || ev.At.IsZero()) {
			return time.Time{}, false, fmt.Errorf("incomplete auth history row %d", lineNumber)
		}
		if account != "" && ev.Account != account {
			continue
		}
		if !found || ev.At.After(latest) {
			latest = ev.At
			found = true
		}
	}
	if err := sc.Err(); err != nil {
		return time.Time{}, false, err
	}
	return latest, found, nil
}
