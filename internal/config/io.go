package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/genericutil"
)

// Default resolves platform defaults and the isolated MORA_CONFIG_DIR root.
func Default() Config {
	if dir := os.Getenv("MORA_CONFIG_DIR"); dir != "" {
		return Config{VaultDir: filepath.Join(dir, "vault"), ConfigDir: dir, DataDir: filepath.Join(dir, "data"), StateDir: filepath.Join(dir, "state")}
	}
	return homeRootConfig(homeFromEnv())
}

// homeFromEnv resolves the platform home directory exactly as os.UserHomeDir
// does (USERPROFILE on Windows, HOME elsewhere), so injected roots and env
// resolution can never disagree on layout.
func homeFromEnv() string {
	home, _ := os.UserHomeDir()
	return home
}

// ConfigRootConfig derives the layout an isolated MORA_CONFIG_DIR root produces:
// the vault, data, and state trees live directly under the root and config.toml
// sits in the root itself.
func ConfigRootConfig(dir string) Config {
	return Config{VaultDir: filepath.Join(dir, "vault"), ConfigDir: dir, DataDir: filepath.Join(dir, "data"), StateDir: filepath.Join(dir, "state")}
}

// homeRootConfig derives the default directory layout from a home root. It is
// the single layout source for both env resolution (Default) and context
// injection (WithHomeRoot), so a test that injects a temp home sees byte-for-byte
// the same paths it used to see by exporting HOME.
func homeRootConfig(home string) Config {
	return Config{VaultDir: filepath.Join(home, "vault", "mora"), ConfigDir: filepath.Join(home, ".config", "mora"), DataDir: filepath.Join(home, ".local", "share", "mora"), StateDir: filepath.Join(home, ".local", "state", "mora")}
}

// homeRootKey / operationClockKey carry test-scope isolation through context.
// Values are process-local per call chain: two tests may each inject their own
// root into their own contexts and run concurrently without sharing state.
type homeRootKey struct{}

type operationClockKey struct{}

// WithHomeRoot pins the home directory used to derive default vault/config/
// data/state locations for every LoadFrom call reading this context. The empty
// string is a valid pin (it models a missing/unset HOME). When a root is
// present, process environment resolution (HOME, MORA_CONFIG_DIR, MORA_VAULT)
// is skipped entirely — the same hermeticity guarantee tests used to get from
// blanking those variables, but carried per call chain instead of in
// process-global state, so concurrent tests never share it.
func WithHomeRoot(ctx context.Context, home string) context.Context {
	return context.WithValue(ctx, homeRootKey{}, home)
}

// HomeRootFrom returns the injected home root, if any.
func HomeRootFrom(ctx context.Context) (string, bool) {
	home, ok := ctx.Value(homeRootKey{}).(string)
	return home, ok
}

// configRootKey pins an isolated MORA_CONFIG_DIR-equivalent root.
type configRootKey struct{}

// WithConfigRoot pins the layout MORA_CONFIG_DIR produces (vault/, data/,
// state/ directly under the root, config.toml in the root itself), again
// bypassing all process environment resolution.
func WithConfigRoot(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, configRootKey{}, dir)
}

func configRootFrom(ctx context.Context) (string, bool) {
	dir, ok := ctx.Value(configRootKey{}).(string)
	return dir, ok
}

// WithOperationClock pins a time source for operation-activity records resolved
// under this context. Production callers resolve no clock and read time.Now.
func WithOperationClock(ctx context.Context, now func() time.Time) context.Context {
	return context.WithValue(ctx, operationClockKey{}, now)
}

func operationClockFrom(ctx context.Context) (func() time.Time, bool) {
	fn, ok := ctx.Value(operationClockKey{}).(func() time.Time)
	return fn, ok && fn != nil
}

// embedderPrefKey carries an explicit MORA_EMBEDDER-equivalent preference.
type embedderPrefKey struct{}

// WithEmbedderPref pins the embedder preference for configs resolved under this
// context, replacing the process environment as the override channel.
func WithEmbedderPref(ctx context.Context, pref string) context.Context {
	return context.WithValue(ctx, embedderPrefKey{}, pref)
}

func embedderPrefFrom(ctx context.Context) (string, bool) {
	pref, ok := ctx.Value(embedderPrefKey{}).(string)
	return pref, ok
}

// authoredReconcilerKey carries an override for the async authored-write
// reconciliation launcher (see internal/mora's scheduleAuthoredReconciliation).
// The field type lives here so Config can carry it; the production
// implementation is assigned by the mora package.
type authoredReconcilerKey struct{}

// WithAuthoredReconciler pins the reconciler launcher used by configs resolved
// under this context.
func WithAuthoredReconciler(ctx context.Context, fn func(context.Context, Config) error) context.Context {
	return context.WithValue(ctx, authoredReconcilerKey{}, fn)
}

func authoredReconcilerFrom(ctx context.Context) (func(context.Context, Config) error, bool) {
	fn, ok := ctx.Value(authoredReconcilerKey{}).(func(context.Context, Config) error)
	return fn, ok && fn != nil
}

// CarryInjection layers any test-scope injection carried by src onto dst.
// Long-lived servers resolve per-request configs from the REQUEST context,
// which loses the launch context's injected sandbox; this restores it while
// keeping the request's cancellation semantics. A src with no injection
// returns dst unchanged.
func CarryInjection(dst, src context.Context) context.Context {
	if dir, ok := configRootFrom(src); ok {
		dst = WithConfigRoot(dst, dir)
	}
	if home, ok := HomeRootFrom(src); ok {
		dst = WithHomeRoot(dst, home)
	}
	if fn, ok := operationClockFrom(src); ok {
		dst = WithOperationClock(dst, fn)
	}
	if fn, ok := authoredReconcilerFrom(src); ok {
		dst = WithAuthoredReconciler(dst, fn)
	}
	if pref, ok := embedderPrefFrom(src); ok {
		dst = WithEmbedderPref(dst, pref)
	}
	return dst
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
func Load() (Config, error) { return load(context.Background()) }

// LoadFrom is Load with context-carried injection: a home root pinned via
// WithHomeRoot replaces HOME/MORA_CONFIG_DIR resolution and disables process
// environment layering; WithOperationClock / WithAuthoredReconciler pin the
// matching per-config seams. Production callers use Load.
func LoadFrom(ctx context.Context) (Config, error) { return load(ctx) }

func load(ctx context.Context) (Config, error) {
	var cfg Config
	injected := false
	if dir, ok := configRootFrom(ctx); ok {
		// Mirrors Default()'s precedence: an explicit config root beats a home root.
		cfg = ConfigRootConfig(dir)
		injected = true
	} else if home, ok := HomeRootFrom(ctx); ok {
		cfg = homeRootConfig(home)
		cfg.SetHomeDir(home)
		injected = true
	} else {
		cfg = Default()
	}
	if fn, ok := operationClockFrom(ctx); ok {
		cfg.SetOperationClock(fn)
	}
	if fn, ok := authoredReconcilerFrom(ctx); ok {
		cfg.SetAuthoredReconciler(fn)
	}
	if pref, ok := embedderPrefFrom(ctx); ok {
		cfg.SetEmbedderPref(pref)
	}
	return resolve(ctx, cfg, injected)
}

// resolve layers config.toml and — only when NOT context-injected — the process
// environment over the base config. The env skip mirrors what hermetic tests
// used to guarantee by blanking MORA_VAULT: an injected root must never be
// overridden by whatever the invoking shell exports.
func resolve(ctx context.Context, cfg Config, injected bool) (Config, error) {
	path := filepath.Join(cfg.ConfigDir, "config.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if injected {
				return cfg, nil
			}
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
	if injected {
		return cfg, nil
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
