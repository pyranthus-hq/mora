// Package integrations reads the one Mora entry in an AI client's
// MCP configuration. It never surfaces any other key of those files.
package integrations

import (
	"os"
	"path/filepath"
)

type Client string

const (
	Claude        Client = "claude"
	Codex         Client = "codex"
	Cursor        Client = "cursor"
	ClaudeDesktop Client = "claude-desktop"
)

func Clients() []Client { return []Client{Claude, Codex, Cursor, ClaudeDesktop} }

func ParseClient(name string) (Client, bool) {
	for _, c := range Clients() {
		if string(c) == name {
			return c, true
		}
	}
	return "", false
}

type Seams struct {
	Home       string
	Stat       func(string) (os.FileInfo, error)
	ReadFile   func(string) ([]byte, error)
	WriteFile  func(path string, data []byte, mode os.FileMode) error
	HookStatus func(settingsPath string) (installed bool, supported bool)
}

func DefaultSeams(home string) Seams {
	return Seams{
		Home:       home,
		Stat:       os.Stat,
		ReadFile:   os.ReadFile,
		WriteFile:  writeAtomic,
		HookStatus: func(string) (bool, bool) { return false, true },
	}
}

// writeAtomic writes beside the target and renames, keeping the requested mode.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mora-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

type Status struct {
	Client        string   `json:"client"`
	Detected      bool     `json:"detected"`
	ConfigPath    string   `json:"config_path"`
	Registered    bool     `json:"registered"`
	Command       string   `json:"command,omitempty"`
	Args          []string `json:"args,omitempty"`
	MatchesBinary bool     `json:"matches_binary"`
	Hook          string   `json:"hook"`
}

func ConfigPath(home string, c Client) string {
	switch c {
	case Claude:
		return filepath.Join(home, ".claude.json")
	case Codex:
		return filepath.Join(home, ".codex", "config.toml")
	case Cursor:
		return filepath.Join(home, ".cursor", "mcp.json")
	case ClaudeDesktop:
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	}
	return ""
}

func settingsPath(home string) string { return filepath.Join(home, ".claude", "settings.json") }

func isDir(seams Seams, path string) bool {
	info, err := seams.Stat(path)
	return err == nil && info.IsDir()
}

// detected is home-scoped on purpose: the contract corpus runs in a temp
// home and must never encode what is installed on the machine that froze it.
func detected(seams Seams, c Client) bool {
	switch c {
	case Claude:
		return isDir(seams, filepath.Join(seams.Home, ".claude"))
	case Codex:
		return isDir(seams, filepath.Join(seams.Home, ".codex"))
	case Cursor:
		return isDir(seams, filepath.Join(seams.Home, ".cursor"))
	case ClaudeDesktop:
		return isDir(seams, filepath.Join(seams.Home, "Library", "Application Support", "Claude"))
	}
	return false
}

// entry is the one thing this package reads out of a client config.
type entry struct {
	Command string
	Args    []string
}

func readEntry(seams Seams, c Client) (*entry, error) {
	path := ConfigPath(seams.Home, c)
	body, err := seams.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if c == Codex {
		return readCodexEntry(body)
	}
	return readJSONEntry(body)
}

func List(seams Seams, binary string) ([]Status, error) {
	binary = filepath.Clean(binary)
	rows := make([]Status, 0, 4)
	for _, c := range Clients() {
		row := Status{Client: string(c), Detected: detected(seams, c), ConfigPath: ConfigPath(seams.Home, c), Hook: "not_supported"}
		e, err := readEntry(seams, c)
		if err != nil {
			row.Hook = "unknown"
		} else if e != nil {
			row.Registered = true
			row.Command = e.Command
			row.Args = e.Args
			row.MatchesBinary = binary != "" && filepath.Clean(e.Command) == binary
		}
		if c == Claude && err == nil {
			installed, supported := seams.HookStatus(settingsPath(seams.Home))
			switch {
			case !supported:
				row.Hook = "not_supported"
			case installed:
				row.Hook = "installed"
			default:
				row.Hook = "not_installed"
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
