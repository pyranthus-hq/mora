package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const Marker = "#mora-managed"

type Command struct {
	Type    string                     `json:"type,omitempty"`
	Command string                     `json:"command,omitempty"`
	Timeout int                        `json:"timeout,omitempty"`
	Extra   map[string]json.RawMessage `json:"-"`
}
type Group struct {
	Matcher string                     `json:"matcher,omitempty"`
	Hooks   []Command                  `json:"hooks"`
	Extra   map[string]json.RawMessage `json:"-"`
}

func (h *Command) UnmarshalJSON(body []byte) error {
	type known Command
	var k known
	if err := json.Unmarshal(body, &k); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}
	delete(raw, "type")
	delete(raw, "command")
	delete(raw, "timeout")
	*h = Command(k)
	h.Extra = raw
	return nil
}
func (h Command) MarshalJSON() ([]byte, error) {
	raw := cloneRawMessages(h.Extra)
	if h.Type != "" {
		b, err := json.Marshal(h.Type)
		if err != nil {
			return nil, err
		}
		raw["type"] = b
	}
	if h.Command != "" {
		b, err := json.Marshal(h.Command)
		if err != nil {
			return nil, err
		}
		raw["command"] = b
	}
	if h.Timeout != 0 {
		b, err := json.Marshal(h.Timeout)
		if err != nil {
			return nil, err
		}
		raw["timeout"] = b
	}
	return json.Marshal(raw)
}
func (g *Group) UnmarshalJSON(body []byte) error {
	type known Group
	var k known
	if err := json.Unmarshal(body, &k); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}
	delete(raw, "matcher")
	delete(raw, "hooks")
	*g = Group(k)
	g.Extra = raw
	return nil
}
func (g Group) MarshalJSON() ([]byte, error) {
	raw := cloneRawMessages(g.Extra)
	if g.Matcher != "" {
		b, err := json.Marshal(g.Matcher)
		if err != nil {
			return nil, err
		}
		raw["matcher"] = b
	}
	b, err := json.Marshal(g.Hooks)
	if err != nil {
		return nil, err
	}
	raw["hooks"] = b
	return json.Marshal(raw)
}
func cloneRawMessages(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in)+3)
	for k, v := range in {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}
func SettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}
func Load(path string) (map[string]any, map[string][]Group, error) {
	settings := map[string]any{}
	body, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, lerr := os.Lstat(path); lerr == nil {
			return nil, nil, fmt.Errorf("refusing to modify %s: it is a broken symlink: fix or remove it first", path)
		} else if !errors.Is(lerr, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("reading Claude settings %s: %w", path, lerr)
		}
	case err != nil:
		return nil, nil, fmt.Errorf("reading Claude settings %s: %w", path, err)
	default:
		if err := json.Unmarshal(body, &settings); err != nil {
			return nil, nil, fmt.Errorf("refusing to modify %s: not valid JSON (%v): fix it or back it up first", path, err)
		}
	}
	groups := map[string][]Group{}
	if raw, ok := settings["hooks"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			if err := json.Unmarshal(b, &groups); err != nil {
				return nil, nil, fmt.Errorf("malformed Claude settings hooks: %w", err)
			}
		} else {
			return nil, nil, fmt.Errorf("malformed Claude settings hooks: %w", err)
		}
	}
	return settings, groups, nil
}
func Write(path string, settings map[string]any) error {
	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(path, append(body, '\n'), 0o600)
}
func upsert(groups map[string][]Group, event, sub string, def Command) {
	existing := groups[event]
	if idx := find(existing, sub); idx >= 0 {
		gi, hi := split(idx)
		existing[gi].Hooks[hi] = def
		groups[event] = existing
		return
	}
	groups[event] = append(existing, Group{Hooks: []Command{def}})
}
func find(groups []Group, sub string) int {
	marker := Marker + ":" + sub
	for gi, g := range groups {
		for hi, h := range g.Hooks {
			if strings.Contains(h.Command, marker) {
				return gi<<16 | hi
			}
		}
	}
	return -1
}
func split(idx int) (int, int) { return idx >> 16, idx & 0xffff }
func Install(path, exe string, threshold float64) error {
	settings, groups, err := Load(path)
	if err != nil {
		return err
	}
	upsert(groups, "SessionStart", "session-start", Command{Type: "command", Command: shellQuote(exe) + " hook session-start " + Marker + ":session-start", Timeout: 15})
	recall := shellQuote(exe) + " hook recall"
	if threshold != 0 {
		recall += " --threshold " + strconv.FormatFloat(threshold, 'g', -1, 64)
	}
	recall += " " + Marker + ":recall"
	upsert(groups, "UserPromptSubmit", "recall", Command{Type: "command", Command: recall, Timeout: 10})
	settings["hooks"] = groups
	return Write(path, settings)
}

// InstallProjectRecall omits the unscoped brief and binds recall to one directory.
func InstallProjectRecall(path, exe, scope, projectDir string, threshold float64) error {
	if !filepath.IsAbs(projectDir) || filepath.Clean(path) != filepath.Join(projectDir, ".claude", "settings.local.json") {
		return errors.New("project recall settings must be in the project directory")
	}
	path, err := ProjectSettingsPath(projectDir)
	if err != nil {
		return err
	}
	projectDir = filepath.Dir(filepath.Dir(path))
	settings, groups, err := Load(path)
	if err != nil {
		return err
	}
	if settings == nil || groups == nil {
		return errors.New("project Claude settings and hooks must be JSON objects")
	}
	managedRecall := 0
	for event, eventGroups := range groups {
		for _, group := range eventGroups {
			for _, hook := range group.Hooks {
				if !strings.Contains(hook.Command, Marker+":") {
					continue
				}
				if event != "UserPromptSubmit" || !strings.Contains(hook.Command, Marker+":recall") {
					return errors.New("remove other managed Mora hooks before installing project recall")
				}
				managedRecall++
			}
		}
	}
	if managedRecall > 1 {
		return errors.New("remove duplicate managed Mora recall hooks before installing project recall")
	}
	recall := shellQuote(exe) + " hook recall --scope " + shellQuote(scope) + " --project-dir " + shellQuote(projectDir)
	if threshold != 0 {
		recall += " --threshold " + strconv.FormatFloat(threshold, 'g', -1, 64)
	}
	recall += " " + Marker + ":recall"
	upsert(groups, "UserPromptSubmit", "recall", Command{Type: "command", Command: recall, Timeout: 10})
	settings["hooks"] = groups
	return Write(path, settings)
}

// ProjectSettingsPath refuses symlinks instead of following or replacing user configuration.
func ProjectSettingsPath(projectDir string) (string, error) {
	root, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, ".claude")
	local := filepath.Join(dir, "settings.local.json")
	for _, path := range []string{dir, filepath.Join(dir, "settings.json"), local} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("project hooks do not support symlinked Claude settings: %s", path)
		}
		if (path == dir && !info.IsDir()) || (path != dir && !info.Mode().IsRegular()) {
			return "", fmt.Errorf("unsupported project Claude settings entry: %s", path)
		}
	}
	return local, nil
}
func Uninstall(path string) error {
	settings, groups, err := Load(path)
	if err != nil {
		return err
	}
	for event, eventGroups := range groups {
		var kept []Group
		for _, group := range eventGroups {
			var commands []Command
			for _, command := range group.Hooks {
				if !strings.Contains(command.Command, Marker+":") {
					commands = append(commands, command)
				}
			}
			if len(commands) > 0 {
				group.Hooks = commands
				kept = append(kept, group)
			}
		}
		if len(kept) == 0 {
			delete(groups, event)
		} else {
			groups[event] = kept
		}
	}
	if len(groups) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = groups
	}
	return Write(path, settings)
}
func Status(path string) (string, string, error) {
	_, groups, err := Load(path)
	if err != nil {
		return "", "", err
	}
	status := func(event, sub string) string {
		if find(groups[event], sub) >= 0 {
			return "installed"
		}
		return "not installed"
	}
	return status("SessionStart", "session-start"), status("UserPromptSubmit", "recall"), nil
}

// shellQuote makes an executable path one literal POSIX shell word.
func shellQuote(exe string) string {
	return "'" + strings.ReplaceAll(exe, "'", "'\\''") + "'"
}
