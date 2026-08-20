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
	upsert(groups, "SessionStart", "session-start", Command{Type: "command", Command: exe + " hook session-start " + Marker + ":session-start", Timeout: 15})
	recall := exe + " hook recall"
	if threshold != 0 {
		recall += " --threshold " + strconv.FormatFloat(threshold, 'g', -1, 64)
	}
	recall += " " + Marker + ":recall"
	upsert(groups, "UserPromptSubmit", "recall", Command{Type: "command", Command: recall, Timeout: 10})
	settings["hooks"] = groups
	return Write(path, settings)
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
