package mora

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const protectedSyncReceiptFlag = "--mora-app-receipt"

var protectedSyncExecutable = os.Executable
var protectedSyncRunOpen = func(ctx context.Context, args ...string) error { return realRun(ctx, "/usr/bin/open", args...) }

// protectedSyncReceipt crosses the LaunchServices boundary because open -W does
// not propagate the launched app's exit status. It lives only in StateDir.
type protectedSyncReceipt struct {
	Token       string `json:"token"`
	Source      string `json:"source"`
	CompletedAt string `json:"completed_at"`
	Error       string `json:"error,omitempty"`
}

func protectedSyncSource(source string) bool {
	return source == "imessage" || source == "applecalendar"
}
func protectedSyncReceiptPath(cfg Config, token string) string {
	return filepath.Join(cfg.StateDir, "protected-sync", token+".json")
}

func newProtectedSyncToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func writeProtectedSyncReceipt(cfg Config, r protectedSyncReceipt) error {
	if len(r.Token) != 32 || r.Source == "" {
		return fmt.Errorf("invalid protected sync receipt")
	}
	if err := os.MkdirAll(filepath.Dir(protectedSyncReceiptPath(cfg, r.Token)), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicio.Write(protectedSyncReceiptPath(cfg, r.Token), b, 0o600)
}

func readProtectedSyncReceipt(cfg Config, token, source string) (protectedSyncReceipt, error) {
	path := protectedSyncReceiptPath(cfg, token)
	b, err := os.ReadFile(path)
	if err != nil {
		return protectedSyncReceipt{}, fmt.Errorf("protected sync did not return a receipt: %w", err)
	}
	_ = os.Remove(path)
	var r protectedSyncReceipt
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("invalid protected sync receipt: %w", err)
	}
	if r.Token != token || r.Source != source || r.CompletedAt == "" {
		return r, fmt.Errorf("protected sync receipt did not match requested source")
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		return r, err
	}
	return r, nil
}

func protectedSyncApp() (string, bool) {
	if runtimeGOOS() != "darwin" {
		return "", false
	}
	exe, err := protectedSyncExecutable()
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return moraAppRoot(exe)
}

func relayProtectedSync(ctx context.Context, cfg Config, source string) error {
	app, ok := protectedSyncApp()
	if !ok {
		return errProtectedSyncDirect
	}
	token, err := newProtectedSyncToken()
	if err != nil {
		return err
	}
	if err := protectedSyncRunOpen(ctx, "-n", "-W", "-a", app, "--args", "sync", source, protectedSyncReceiptFlag, token); err != nil {
		return fmt.Errorf("launching Mora.app for protected sync: %w", err)
	}
	r, err := readProtectedSyncReceipt(cfg, token, source)
	if err != nil {
		return err
	}
	if r.Error != "" {
		return fmt.Errorf("Mora.app %s sync failed: %s", source, r.Error)
	}
	return nil
}

var errProtectedSyncDirect = fmt.Errorf("protected sync should run directly")

func protectedSyncReceiptArg(args []string) (token string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] != protectedSyncReceiptFlag {
			rest = append(rest, args[i])
			continue
		}
		if i+1 >= len(args) || token != "" {
			return "", nil, fmt.Errorf("invalid %s", protectedSyncReceiptFlag)
		}
		token = args[i+1]
		i++
	}
	if token != "" && (len(token) != 32 || strings.Trim(token, "0123456789abcdef") != "") {
		return "", nil, fmt.Errorf("invalid %s", protectedSyncReceiptFlag)
	}
	return token, rest, nil
}

func realRun(ctx context.Context, name string, args ...string) error {
	_, err := realExec(ctx, "", name, args...)
	return err
}

var protectedSyncNow = time.Now
