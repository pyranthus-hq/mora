// Package protectedsync owns the token-bound LaunchServices receipt protocol for FDA-protected connectors.
package protectedsync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const ReceiptFlag = "--mora-app-receipt"

var ErrDirect = errors.New("protected sync should run directly")

type Receipt struct {
	Token       string `json:"token"`
	Source      string `json:"source"`
	Items       int    `json:"items"`
	CompletedAt string `json:"completed_at"`
	Error       string `json:"error,omitempty"`
}
type Options struct {
	StateDir, Source, GOOS string
	Executable             func() (string, error)
	EvalSymlinks           func(string) (string, error)
	AppRoot                func(string) (string, bool)
	RunOpen                func(context.Context, ...string) error
	Rand                   io.Reader
}

func IsSource(source string) bool { return source == "imessage" || source == "applecalendar" }
func Path(stateDir, token string) string {
	return filepath.Join(stateDir, "protected-sync", token+".json")
}
func NewToken(reader io.Reader) (string, error) {
	if reader == nil {
		reader = rand.Reader
	}
	var b [16]byte
	if _, err := io.ReadFull(reader, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func WriteReceipt(stateDir string, r Receipt) error {
	if len(r.Token) != 32 || r.Source == "" {
		return fmt.Errorf("invalid protected sync receipt")
	}
	if err := os.MkdirAll(filepath.Dir(Path(stateDir, r.Token)), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicio.Write(Path(stateDir, r.Token), b, 0o600)
}
func ReadReceipt(stateDir, token, source string) (Receipt, error) {
	return ReadReceiptAfter(stateDir, token, source, time.Time{})
}

// ReadReceiptAfter consumes one token-bound receipt and, when minCompletedAt is
// set, rejects a receipt produced before the caller launched the app. The token is
// removed before validation so stale or malformed content cannot be replayed.
func ReadReceiptAfter(stateDir, token, source string, minCompletedAt time.Time) (Receipt, error) {
	path := Path(stateDir, token)
	b, err := os.ReadFile(path)
	if err != nil {
		return Receipt{}, fmt.Errorf("protected sync did not return a receipt: %w", err)
	}
	_ = os.Remove(path)
	var r Receipt
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("invalid protected sync receipt: %w", err)
	}
	if r.Token != token || r.Source != source || r.CompletedAt == "" {
		return r, fmt.Errorf("protected sync receipt did not match requested source")
	}
	if !minCompletedAt.IsZero() {
		completedAt, parseErr := time.Parse(time.RFC3339, r.CompletedAt)
		if parseErr != nil || completedAt.Before(minCompletedAt.UTC().Truncate(time.Second)) {
			return r, fmt.Errorf("protected sync receipt is older than invocation launch time")
		}
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		return r, err
	}
	return r, nil
}
func Relay(ctx context.Context, o Options) (Receipt, error) {
	if o.GOOS != "darwin" {
		return Receipt{}, ErrDirect
	}
	exe, err := o.Executable()
	if err != nil {
		return Receipt{}, ErrDirect
	}
	eval := o.EvalSymlinks
	if eval == nil {
		eval = filepath.EvalSymlinks
	}
	if resolved, err := eval(exe); err == nil {
		exe = resolved
	}
	app, ok := o.AppRoot(exe)
	if !ok {
		return Receipt{}, ErrDirect
	}
	token, err := NewToken(o.Rand)
	if err != nil {
		return Receipt{}, err
	}
	if err := o.RunOpen(ctx, "-n", "-W", "-a", app, "--args", "sync", o.Source, ReceiptFlag, token); err != nil {
		return Receipt{}, fmt.Errorf("launching Mora.app for protected sync: %w", err)
	}
	r, err := ReadReceipt(o.StateDir, token, o.Source)
	if err != nil {
		return Receipt{}, err
	}
	if r.Error != "" {
		return r, fmt.Errorf("Mora.app %s sync failed: %s", o.Source, r.Error)
	}
	return r, nil
}
func ParseArgs(args []string) (token string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] != ReceiptFlag {
			rest = append(rest, args[i])
			continue
		}
		if i+1 >= len(args) || token != "" {
			return "", nil, fmt.Errorf("invalid %s", ReceiptFlag)
		}
		token = args[i+1]
		i++
	}
	if token != "" && (len(token) != 32 || strings.Trim(token, "0123456789abcdef") != "") {
		return "", nil, fmt.Errorf("invalid %s", ReceiptFlag)
	}
	return token, rest, nil
}
