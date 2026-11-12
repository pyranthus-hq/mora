package mora

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

// Antigravity hooks (docs: agy-customizations/docs/hooks.md). The hook payload
// is camelCase JSON on stdin; SessionStart and PreInvocation answer with
// {"injectSteps":[{"systemMessage":{"systemMessage":"...","metadata":{"user_markdown":"..."}}}]}.
// The model sees only systemMessage; user_markdown is shown to the person.
// A hook with nothing to say answers {}.

const (
	antigravityHookKey      = "mora"
	antigravityClient       = "antigravity"
	antigravitySeenFileName = "antigravity-recall-seen.json"
)

type antigravityHookEnvelope struct {
	InjectSteps []antigravityInjectStep `json:"injectSteps"`
}

type antigravityInjectStep struct {
	SystemMessage antigravitySystemMessage `json:"systemMessage"`
}

type antigravitySystemMessage struct {
	SystemMessage string                     `json:"systemMessage"`
	Metadata      *antigravitySystemMetadata `json:"metadata,omitempty"`
}

type antigravitySystemMetadata struct {
	UserMarkdown string `json:"user_markdown"`
}

// writeAntigravityEmpty is the Antigravity answer for "inject nothing".
func writeAntigravityEmpty(w io.Writer) error {
	_, err := io.WriteString(w, "{}\n")
	return err
}

// writeAntigravityHookOutput injects context for the model. banner, when not
// empty, is the markdown line shown to the person.
func writeAntigravityHookOutput(w io.Writer, context, banner string) error {
	step := antigravitySystemMessage{SystemMessage: context}
	if banner != "" {
		step.Metadata = &antigravitySystemMetadata{UserMarkdown: banner}
	}
	return json.NewEncoder(w).Encode(antigravityHookEnvelope{InjectSteps: []antigravityInjectStep{{SystemMessage: step}}})
}

// hookCaller is the caller label written to the given log.
func hookCaller(antigravity bool) string {
	if antigravity {
		return antigravityClient
	}
	return "claude-code"
}

// antigravityRecallSeen reports whether this conversation already got recall
// for this exact user input, and records it when not. PreInvocation fires
// before every model call, so one user message that triggers several tool
// steps would otherwise inject the same notes each time. A state-dir failure
// means "not seen": recall must never fail closed on bookkeeping.
func antigravityRecallSeen(cfg Config, conversationID, input string) bool {
	if conversationID == "" || cfg.StateDir == "" {
		return false
	}
	sum := sha256.Sum256([]byte(input))
	key := hex.EncodeToString(sum[:8])
	path := filepath.Join(cfg.StateDir, antigravitySeenFileName)
	seen := map[string]string{}
	if body, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(body, &seen)
	}
	if seen[conversationID] == key {
		return true
	}
	if len(seen) > 50 {
		seen = map[string]string{}
	}
	seen[conversationID] = key
	if body, err := json.Marshal(seen); err == nil {
		_ = atomicio.Write(path, body, 0o600)
	}
	return false
}

// antigravityHooksPath is --hooks-file, or ~/.gemini/config/hooks.json, the
// global customization root the Antigravity docs name.
//
// Under WSL the Linux home is the wrong place: Antigravity is a Windows program
// and reads the Windows profile's file, so there is no default and --hooks-file
// (a /mnt/<drive>/... path) is required.
func antigravityHooksPath(ctx context.Context, override string) (string, error) {
	if override != "" {
		return filepath.Abs(normalizeWSLWindowsPath(override))
	}
	if wslHostOS != "windows" && wslDistro() != "" {
		return "", errors.New("running under WSL: Antigravity is a Windows program and does not read this Linux home. " +
			"Pass --hooks-file with the Windows file, for example --hooks-file /mnt/c/Users/<name>/.gemini/config/hooks.json")
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return "", err
	}
	home := cfg.HomeDir()
	if home == "" {
		return "", errors.New("cannot resolve a home directory for Antigravity hooks; pass --hooks-file")
	}
	return filepath.Join(home, ".gemini", "config", "hooks.json"), nil
}

// antigravityHookCommand quotes exe for the shell Antigravity uses: sh -c on
// Unix, cmd /c on Windows.
//
// When distro is not empty the file is a Windows program's, and mora lives in
// that WSL distro, so cmd /c runs it through wsl.exe.
func antigravityHookCommand(exe, sub, distro string, extra ...string) string {
	if distro != "" {
		q := func(s string) string {
			if strings.ContainsAny(s, " \t\"") {
				return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
			}
			return s
		}
		parts := append([]string{"wsl.exe", "-d", q(distro), "-e", q(exe), "hook", sub, "--client", antigravityClient}, extra...)
		return strings.Join(parts, " ")
	}
	q := "'" + strings.ReplaceAll(exe, "'", `'\''`) + "'"
	if runtime.GOOS == "windows" {
		q = `"` + exe + `"`
	}
	parts := append([]string{q, "hook", sub, "--client", antigravityClient}, extra...)
	return strings.Join(parts, " ")
}

// antigravityWindowsFile reports whether path is on a Windows drive mounted in
// this WSL distro (/mnt/<letter>/...), and names the distro to run mora in.
func antigravityWindowsFile(path string) (distro string, ok bool) {
	d := wslDistro()
	if wslHostOS == "windows" || d == "" {
		return "", false
	}
	rel, err := filepath.Rel(wslMountRoot, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	if len(first) != 1 || !isASCIILetter(first[0]) {
		return "", false
	}
	return d, true
}

func antigravityHookSpec(exe, distro string, threshold float64) map[string]any {
	recall := []string{}
	if threshold != 0 {
		recall = append(recall, "--threshold", strconv.FormatFloat(threshold, 'g', -1, 64))
	}
	return map[string]any{
		"SessionStart": []any{map[string]any{"type": "command", "command": antigravityHookCommand(exe, "session-start", distro), "timeout": 15}},
		// PreInvocation is the per-user-turn hook that carries lastUserInput.
		"PreInvocation": []any{map[string]any{"type": "command", "command": antigravityHookCommand(exe, "recall", distro, recall...), "timeout": 10}},
	}
}

// marshalPlain is json.MarshalIndent without HTML escaping, so another hook's
// "&&" is written back as it was read.
func marshalPlain(v any, indent bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// readAntigravityHooks loads hooks.json as top-level raw entries so every
// other hook ("orca-status" and the like) is written back untouched.
func readAntigravityHooks(path string) (map[string]json.RawMessage, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, fmt.Errorf("refusing to modify %s: it must be a JSON object", path)
	}
	return top, nil
}

// writeAntigravityHooks writes through a symlink to its target, keeps the
// file's mode (0600 for a new file) and does not re-escape other hooks' text.
func writeAntigravityHooks(path string, top map[string]json.RawMessage) error {
	mode := os.FileMode(0o600)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
	}
	body, err := marshalPlain(top, true)
	if err != nil {
		return err
	}
	return atomicio.Write(path, body, mode)
}

func hookInstallAntigravity(ctx context.Context, stdout io.Writer, hooksFile string, threshold float64) error {
	exe, err := hookExecutable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	path, err := antigravityHooksPath(ctx, hooksFile)
	if err != nil {
		return err
	}
	top, err := readAntigravityHooks(path)
	if err != nil {
		return err
	}
	distro, _ := antigravityWindowsFile(path)
	spec, err := marshalPlain(antigravityHookSpec(exe, distro, threshold), false)
	if err != nil {
		return err
	}
	spec = bytes.TrimSpace(spec)
	top[antigravityHookKey] = spec
	if err := writeAntigravityHooks(path, top); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "installed mora Antigravity hooks: SessionStart, PreInvocation (%s)\n", path)
	return nil
}

func hookUninstallAntigravity(ctx context.Context, stdout io.Writer, hooksFile string) error {
	path, err := antigravityHooksPath(ctx, hooksFile)
	if err != nil {
		return err
	}
	top, err := readAntigravityHooks(path)
	if err != nil {
		return err
	}
	if _, ok := top[antigravityHookKey]; !ok {
		fmt.Fprintln(stdout, "mora Antigravity hooks are not installed")
		return nil
	}
	delete(top, antigravityHookKey)
	if err := writeAntigravityHooks(path, top); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "uninstalled mora Antigravity hooks")
	return nil
}

// antigravityHookStates reads which Mora events hooks.json carries.
func antigravityHookStates(path string) (events []string, err error) {
	top, err := readAntigravityHooks(path)
	if err != nil {
		return nil, err
	}
	raw, ok := top[antigravityHookKey]
	if !ok {
		return nil, nil
	}
	var spec map[string]json.RawMessage
	if json.Unmarshal(raw, &spec) != nil {
		return nil, nil
	}
	for _, ev := range []string{"SessionStart", "PreInvocation"} {
		if _, ok := spec[ev]; ok {
			events = append(events, ev)
		}
	}
	return events, nil
}

func hookStatusAntigravity(ctx context.Context, stdout io.Writer, hooksFile string, jsonOut bool) error {
	path, err := antigravityHooksPath(ctx, hooksFile)
	if err != nil {
		return err
	}
	events, err := antigravityHookStates(path)
	if err != nil {
		return err
	}
	if events == nil {
		events = []string{}
	}
	if jsonOut {
		state := func(ev string) string {
			for _, have := range events {
				if have == ev {
					return "installed"
				}
			}
			return "not installed"
		}
		bound := ""
		if cfg, cerr := loadConfigFor(ctx); cerr == nil {
			if wd, werr := os.Getwd(); werr == nil {
				bound, _, _, _ = lookupProject(cfg, wd)
			}
		}
		// The v1 fields use Claude's vocabulary: UserPromptSubmit is the hook that
		// carries each prompt, which for Antigravity is PreInvocation. Mora
		// installs no PreToolUse hook and no session-end hook for Antigravity.
		return emitReceipt(stdout, "mora.hook.status", 1, hookStatusPayload{
			Installed: len(events) > 0, Path: path,
			Harnesses:  []hookStatusHarness{{Name: antigravityClient, Installed: len(events) > 0, Events: events}},
			PreToolUse: "not installed",
			Global:     hookLevelStatus{SessionStart: state("SessionStart"), UserPromptSubmit: state("PreInvocation"), SessionEnd: "not installed"},
			BoundScope: bound,
		})
	}
	for _, ev := range []string{"SessionStart", "PreInvocation"} {
		state := "not installed"
		for _, have := range events {
			if have == ev {
				state = "installed"
			}
		}
		fmt.Fprintf(stdout, "%s: %s\n", ev, state)
	}
	fmt.Fprintf(stdout, "File: %s\n", path)
	return nil
}
