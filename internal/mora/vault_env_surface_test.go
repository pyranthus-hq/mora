package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// vault_env_surface_test.go is the per-surface integration pin for the
// MORA_VAULT runtime override (issue #217, follow-up to #216 review finding F3).
//
// vault_env_test.go pins `loadConfig` — the ONE function that layers the env
// override on top of config.toml. That is necessary but not sufficient: it
// proves the resolver is right, never that each runtime surface actually goes
// through it. All four surfaces (doctor, brief, mcp, serve) reach vault
// resolution via loadConfig today, but that was verified only by a static
// trace, so a later entry point that stats cfg.VaultDir from defaultConfig, or
// caches a Config captured before the override, would ship green.
//
// So each test here seeds a DISTINCT marker inside BOTH vaults — the persisted
// one config.toml names, and the one MORA_VAULT exports — drives the surface
// end to end, and asserts it read the ENV marker and NOT the persisted one. Two
// live markers, never one: a surface that reads the wrong vault fails on the
// positive assertion, and a surface that reads NEITHER (an empty result that
// would satisfy a bare "persisted marker absent" check) fails too.
//
// The marker is always something resolved from cfg.VaultDir on the READ path.
// It is deliberately never a search hit: the index lives under DataDir (dbPath =
// cfg.DataDir/index.db) and does not move with the vault, so a marker already
// committed to the index is returned whichever vault the surface picked — the
// assertion would false-pass. (Search can reach the vault when it rebuilds a
// missing or stale index, but staking the pin on that makes the discriminator
// conditional on index state, which is exactly the coupling to avoid.) The
// markers used instead are unconditional reads under cfg.VaultDir:
// list_memory walks the vault's markdown files (allMemoryFiles →
// memoriesRoot(cfg)), the brief cache is a file read, and the git-sync probe is
// a stat.

// twoVaults sets up the fixture every test in this file shares: a temp HOME
// with an initialized install (so config.toml carries a real persisted
// vault_dir), plus a second vault directory exported as MORA_VAULT. It returns
// both paths so the caller can seed each with its own marker.
//
// Order matters: `init` runs with MORA_VAULT unset (withTempHome clears it), so
// config.toml persists the default HOME vault and the env vault is genuinely a
// runtime-only override — the exact production shape, not a config that already
// agrees with the environment.
func twoVaults(t *testing.T) (persisted, env string) {
	t.Helper()
	// LEGACY env harness on purpose: this file pins MORA_VAULT env-override
	// semantics, which an injected root deliberately ignores.
	withTempHomeSetenv(t)
	run(t, "init")
	persisted = mustConfig(t).VaultDir

	env = filepath.Join(t.TempDir(), "env-vault")
	if err := os.MkdirAll(env, 0o700); err != nil {
		t.Fatalf("mkdir env vault: %v", err)
	}
	t.Setenv("MORA_VAULT", env)
	if got := mustConfig(t).VaultDir; got != env {
		t.Fatalf("fixture broken: MORA_VAULT %q did not take effect, cfg.VaultDir=%q", env, got)
	}
	return persisted, env
}

// seedVaultMemory writes one memory markdown file straight into vault's
// memories tree. It deliberately bypasses `mora write`: that path also upserts
// the index, and the index is bound to ONE vault identity (vaultid.go), so
// seeding two vaults through it would trip a rebuild or a rebuild block and
// couple this test to machinery it is not about. writeMemory only needs
// cfg.VaultDir, and the surfaces under test read these files back directly.
func seedVaultMemory(t *testing.T, vault, id, title string) {
	t.Helper()
	m := Memory{
		ID:        id,
		Scope:     "global",
		Type:      "insight",
		Title:     title,
		Source:    "test",
		CreatedAt: "2026-06-08T09:00:00Z",
		Text:      title + " body",
	}
	if err := writeMemory(Config{VaultDir: vault}, m); err != nil {
		t.Fatalf("seed memory in %s: %v", vault, err)
	}
}

// assertReadEnvVault is the shared verdict: the surface output must carry the
// env vault's marker and must not carry the persisted vault's.
func assertReadEnvVault(t *testing.T, surface, out, envMarker, persistedMarker string) {
	t.Helper()
	if !strings.Contains(out, envMarker) {
		t.Fatalf("%s did not read the MORA_VAULT vault: want marker %q in output\n--- got ---\n%s", surface, envMarker, out)
	}
	if strings.Contains(out, persistedMarker) {
		t.Fatalf("%s read config.toml's vault instead of MORA_VAULT: found marker %q in output\n--- got ---\n%s", surface, persistedMarker, out)
	}
}

// TestDoctorSurfaceHonorsVaultEnv pins `mora doctor` (doctor.go, cmdDoctor).
//
// doctor reports no memory text, so the marker is the vault file it does report
// on: `<vault>/.git`, which drives git_sync_configured. That field is the
// zero-egress disclosure — a doctor probing the WRONG vault tells the user their
// memories stay on the device while the vault they are actually using pushes to
// a remote, or the reverse. Both directions are asserted, so neither a hardcoded
// true nor a hardcoded false can pass.
func TestDoctorSurfaceHonorsVaultEnv(t *testing.T) {
	for _, tc := range []struct {
		name        string
		markEnv     bool
		wantGitSync bool
	}{
		{"marker in the env vault", true, true},
		{"marker in the persisted vault only", false, false},
	} {
		subRun(t, tc.name, func(t *testing.T) {
			persisted, env := twoVaults(t)
			marked := persisted
			if tc.markEnv {
				marked = env
			}
			if err := os.MkdirAll(filepath.Join(marked, ".git"), 0o700); err != nil {
				t.Fatalf("seed .git marker: %v", err)
			}

			var rep doctorReport
			out := run(t, "doctor", "--json")
			if err := json.Unmarshal([]byte(out), &rep); err != nil {
				t.Fatalf("doctor --json: %v\n%s", err, out)
			}
			if rep.GitSyncConfigured != tc.wantGitSync {
				t.Fatalf("doctor read the wrong vault: git_sync_configured=%v, want %v (marker seeded in %s; MORA_VAULT=%s)",
					rep.GitSyncConfigured, tc.wantGitSync, marked, env)
			}
		})
	}
}

// TestBriefSurfaceHonorsVaultEnv pins `mora brief` (mora.go, cmdBrief).
//
// The marker is the persisted daily brief the CLI prints VERBATIM:
// resolveBrief reads <vault>/briefs/<date>-brief.md, so a brief served from
// config.toml's vault is yesterday's context from the wrong corpus, silently.
// The clock is pinned so the seeded date always reads as fresh.
func TestBriefSurfaceHonorsVaultEnv(t *testing.T) {
	pinBriefClock(t)
	persisted, env := twoVaults(t)

	const envMarker = "ENV-VAULT-BRIEF-MARKER"
	const persistedMarker = "PERSISTED-VAULT-BRIEF-MARKER"
	date := briefFixedNow.UTC().Format("2006-01-02")
	seedBriefFile(t, Config{VaultDir: env}, date, "# "+envMarker+"\n")
	seedBriefFile(t, Config{VaultDir: persisted}, date, "# "+persistedMarker+"\n")

	out := run(t, "brief", "--json")
	var got briefResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("brief --json: %v\n%s", err, out)
	}
	if got.Generated {
		t.Fatalf("brief regenerated instead of reading the seeded cache; the marker assertion would be vacuous:\n%s", out)
	}
	assertReadEnvVault(t, "mora brief --json", got.Body, envMarker, persistedMarker)
}

// TestMCPSurfaceHonorsVaultEnv pins the stdio MCP server (mcp.go, callMCPTool).
//
// This is the surface an agent session actually runs on, and it is long-lived:
// it resolves the config once per tool call, so an override read at the wrong
// moment shows up here and nowhere else. list_memory walks the vault's markdown
// files, so the marker is a seeded memory title.
func TestMCPSurfaceHonorsVaultEnv(t *testing.T) {
	persisted, env := twoVaults(t)

	const envMarker = "ENV-VAULT-MEMORY-MARKER"
	const persistedMarker = "PERSISTED-VAULT-MEMORY-MARKER"
	seedVaultMemory(t, env, "env-marker", envMarker)
	seedVaultMemory(t, persisted, "persisted-marker", persistedMarker)

	text, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_memory","arguments":{}}}`)
	if isErr {
		t.Fatalf("list_memory returned isError; text=%s", text)
	}
	assertReadEnvVault(t, "mcp list_memory", text, envMarker, persistedMarker)
}

// TestServeHTTPSurfaceHonorsVaultEnv pins the loopback HTTP daemon
// (serve_http.go, serveLoopbackHTTP). Same marker as the MCP test, driven over
// a REAL listener and a REAL request so the whole chain is live: the process
// entry point, the token, the host/auth middleware, and the resolution that
// actually selects the vault.
//
// That resolution is the PER-REQUEST callMCPTool, not serveLoopbackHTTP's own
// loadConfig — the latter resolves only the bearer token, which lives under
// ConfigDir and does not move with the vault. (handleHealthz resolves its own
// config as well, but its report — ok/service/version/state — carries no field
// derived from cfg.VaultDir, so there is nothing on that route to discriminate
// with.) So the pin here is end-to-end on the daemon rather than on a
// serve-only resolver, and the long-lived process is the point: the server
// outlives any single resolution, which is where a cached-config bypass would
// hide.
func TestServeHTTPSurfaceHonorsVaultEnv(t *testing.T) {
	persisted, env := twoVaults(t)

	const envMarker = "ENV-VAULT-MEMORY-MARKER"
	const persistedMarker = "PERSISTED-VAULT-MEMORY-MARKER"
	seedVaultMemory(t, env, "env-marker", envMarker)
	seedVaultMemory(t, persisted, "persisted-marker", persistedMarker)

	base, token := startServeHTTP(t)
	body := serveHTTPPost(t, base+"/call", token, `{"name":"list_memory","arguments":{}}`)
	assertReadEnvVault(t, "serve http POST /call list_memory", body, envMarker, persistedMarker)
}

// lockedBuffer is a mutex-guarded bytes.Buffer: the serve harness runs the
// server in a goroutine that writes its startup banner while the test goroutine
// may read the buffer for a failure message, which is a data race on a bare
// bytes.Buffer (and `go test -race` is part of CI).
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// freeLoopbackPort returns a loopback port that was free a moment ago. `mora
// serve http` takes a port, not a listener, so the bind cannot be handed in
// pre-opened; asking the kernel for an ephemeral port and closing it is the
// standard narrowing of the window.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	return port
}

// startServeHTTP runs `mora serve http` through the real Run dispatcher in a
// goroutine, waits for it to answer /healthz, and returns its base URL and
// bearer token. Once it returns, a cleanup is registered that cancels the
// server's context — so the daemon always outlives the request and always dies
// before t.Setenv restores the environment it was started with.
//
// The one flake this harness could have is the bind race: `mora serve http`
// takes a port number, not a pre-opened listener, so the reserved ephemeral port
// is closed before the server re-binds it and something else could take it in
// between. A lost race makes the server exit immediately with a listen error,
// which is unambiguous, so retry on a fresh port instead of failing.
func startServeHTTP(t *testing.T) (base, token string) {
	t.Helper()
	token = strings.TrimSpace(run(t, "serve", "http", "--print-token"))
	if token == "" {
		t.Fatal("serve http --print-token printed no token")
	}

	const attempts = 3
	for attempt := 1; ; attempt++ {
		port := freeLoopbackPort(t)
		t.Setenv("MORA_PORT", strconv.Itoa(port))
		base = "http://127.0.0.1:" + strconv.Itoa(port)

		ctx, cancel := context.WithCancel(context.Background())
		out := &lockedBuffer{}
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, []string{"serve", "http"}, out, out, strings.NewReader(""))
		}()

		err := waitServeReachable(base, done)
		if err == nil {
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Errorf("serve http exited with error: %v\n%s", err, out.String())
					}
				case <-time.After(10 * time.Second):
					t.Errorf("serve http did not shut down after its context was cancelled\n%s", out.String())
				}
			})
			return base, token
		}

		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("serve http attempt %d did not exit after its context was cancelled\n%s", attempt, out.String())
		}
		if attempt == attempts {
			t.Fatalf("serve http never became reachable on %s after %d attempts: %v\n%s", base, attempts, err, out.String())
		}
		t.Logf("serve http attempt %d on %s failed (%v); retrying on a new port", attempt, base, err)
	}
}

// waitServeReachable polls /healthz until the daemon answers, returning an error
// if it exits first or never comes up. It only reports the FAILURE; the caller
// decides whether that is a retry or a fatal. An early-exit error read off done
// is put straight back (the channel is buffered and its writer has already
// finished), so the caller's own drain still completes.
func waitServeReachable(base string, done chan error) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := serveHTTPClient.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		select {
		case serveErr := <-done:
			done <- serveErr
			return fmt.Errorf("exited before it was reachable: %w", serveErr)
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no answer on /healthz before the deadline: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// serveHTTPClient carries an explicit per-request timeout. http.DefaultClient
// has none, so a daemon that accepted the connection and then wedged would hang
// the whole package run instead of failing this one test — the readiness poll's
// nominal deadline only bounds the retry loop, never a single in-flight request.
var serveHTTPClient = &http.Client{Timeout: 20 * time.Second}

// serveHTTPPost issues one authenticated POST and returns the response body,
// failing on anything but 200 so a 401/403 never reads as an empty result.
func serveHTTPPost(t *testing.T, url, token, body string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := serveHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status %d, body %s", url, resp.StatusCode, raw)
	}
	return string(raw)
}
