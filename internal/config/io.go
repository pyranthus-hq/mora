package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/genericutil"
)

// Default resolves platform defaults and the isolated MORA_CONFIG_DIR root.
func Default() Config {
	if dir := os.Getenv("MORA_CONFIG_DIR"); dir != "" {
		return Config{VaultDir: filepath.Join(dir, "vault"), ConfigDir: dir, DataDir: filepath.Join(dir, "data"), StateDir: filepath.Join(dir, "state")}
	}
	home, _ := os.UserHomeDir()
	return Config{VaultDir: filepath.Join(home, "vault", "mora"), ConfigDir: filepath.Join(home, ".config", "mora"), DataDir: filepath.Join(home, ".local", "share", "mora"), StateDir: filepath.Join(home, ".local", "state", "mora")}
}

func splitCommaList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ParseValue decodes the small TOML scalar grammar used by Mora's config file.
func ParseValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, `"`) {
		for i := 1; i < len(raw); i++ {
			switch raw[i] {
			case '\\':
				i++
			case '"':
				if v, err := strconv.Unquote(raw[:i+1]); err == nil {
					return v
				}
				return strings.Trim(raw[:i+1], `"`)
			}
		}
		return strings.Trim(raw, `"`)
	}
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw)
}

// ApplyEnv layers runtime-only environment overrides over durable configuration.
func ApplyEnv(cfg Config) (Config, error) {
	if v := os.Getenv("MORA_VAULT"); v != "" {
		if strings.TrimSpace(v) == "" {
			return cfg, fmt.Errorf("MORA_VAULT is set but blank; unset it or set an absolute vault path")
		}
		p := genericutil.ExpandHome(v)
		if !filepath.IsAbs(p) {
			return cfg, fmt.Errorf("MORA_VAULT=%q is not an absolute path; a relative vault depends on the process working directory (services and schedules run elsewhere), so it is refused", v)
		}
		cfg.ApplyVaultOverride(p)
	}
	return cfg, nil
}

// ParseMCPWritePolicy validates and canonicalizes a durable MCP policy.
func ParseMCPWritePolicy(raw string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(raw))
	switch p {
	case "open", "propose", "readonly":
		return p, nil
	default:
		return "", fmt.Errorf("invalid mcp_write_policy %q (want open, propose, or readonly)", raw)
	}
}

// ParseUpdatePolicy validates and canonicalizes a durable update policy.
func ParseUpdatePolicy(raw string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(raw))
	switch p {
	case "auto", "notify", "off":
		return p, nil
	default:
		return "", fmt.Errorf("unknown update policy %q (want auto, notify, or off)", raw)
	}
}

// Load resolves defaults, config.toml, then runtime environment overrides.
func Load() (Config, error) {
	cfg := Default()
	path := filepath.Join(cfg.ConfigDir, "config.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ApplyEnv(cfg)
		}
		return cfg, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := genericutil.ExpandHome(ParseValue(parts[1]))
		switch key {
		case "vault_dir":
			cfg.VaultDir = val
		case "data_dir":
			cfg.DataDir = val
		case "state_dir":
			cfg.StateDir = val
		case "embedder":
			cfg.Embedder = val
		case "context":
			cfg.ContextProfile = val
		case "self_emails":
			cfg.SelfEmails = splitCommaList(val)
		case "mmr":
			cfg.MMR = val == "true" || val == "1"
		case "mcp_write_policy":
			p, e := ParseMCPWritePolicy(val)
			if e != nil {
				return cfg, e
			}
			cfg.MCPWritePolicy = p
		case "update_policy":
			p, e := ParseUpdatePolicy(val)
			if e != nil {
				return cfg, e
			}
			cfg.UpdatePolicy = p
		}
	}
	return ApplyEnv(cfg)
}

// Write updates Mora-owned settings atomically while preserving unknown lines.
func Write(cfg Config) error {
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(cfg.ConfigDir, "config.toml")
	mmr := ""
	if cfg.MMR {
		mmr = "true"
	}
	owned := []struct{ key, val string }{{"vault_dir", cfg.PersistVaultDir()}, {"data_dir", cfg.DataDir}, {"state_dir", cfg.StateDir}, {"embedder", cfg.Embedder}, {"context", cfg.ContextProfile}, {"mmr", mmr}, {"mcp_write_policy", cfg.MCPWritePolicy}, {"update_policy", cfg.UpdatePolicy}}
	ownedVal := func(key string) (string, bool) {
		for _, kv := range owned {
			if kv.key == key {
				return kv.val, true
			}
		}
		return "", false
	}
	var existing []string
	if b, err := os.ReadFile(path); err == nil {
		existing = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(existing) == 1 && existing[0] == "" {
			existing = nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	written := map[string]bool{}
	var out []string
	for _, line := range existing {
		trim := strings.TrimSpace(line)
		key := ""
		if trim != "" && !strings.HasPrefix(trim, "#") {
			if parts := strings.SplitN(trim, "=", 2); len(parts) == 2 {
				key = strings.TrimSpace(parts[0])
			}
		}
		val, owns := ownedVal(key)
		if !owns {
			out = append(out, line)
			continue
		}
		if written[key] {
			continue
		}
		written[key] = true
		if val == "" {
			if key == "embedder" || key == "context" || key == "mmr" || key == "mcp_write_policy" || key == "update_policy" {
				continue
			}
			out = append(out, line)
			continue
		}
		out = append(out, fmt.Sprintf("%s = %q", key, val))
	}
	for _, kv := range owned {
		if kv.val == "" || written[kv.key] {
			continue
		}
		out = append(out, fmt.Sprintf("%s = %q", kv.key, kv.val))
	}
	return atomicio.Write(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}
