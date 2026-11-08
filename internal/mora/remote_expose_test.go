package mora

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
)

const remoteExposeExampleHost = "node.tailnet-example.example"

func TestBuildRemoteExposeCommandsIsStringOnly(t *testing.T) {
	got := buildRemoteExposeCommands(7780, remoteExposeExampleHost)
	if !got.ExecutesNothing {
		t.Fatal("executes_nothing must be true")
	}
	if got.Backend != "http://127.0.0.1:7780/mcp" {
		t.Fatalf("backend = %q", got.Backend)
	}
	if got.AllowHost != remoteExposeExampleHost || !got.HostnameKnown {
		t.Fatalf("hostname fields = %+v", got)
	}
	wantListen := []string{"mora", "mcp", "serve-http", "--port", "7780", "--allow-host", remoteExposeExampleHost}
	if strings.Join(got.ListenCommand, " ") != strings.Join(wantListen, " ") {
		t.Fatalf("listen = %v", got.ListenCommand)
	}
	wantFunnel := []string{"tailscale", "funnel", "--bg", "--https=443", "http://127.0.0.1:7780"}
	if strings.Join(got.FunnelCommand, " ") != strings.Join(wantFunnel, " ") {
		t.Fatalf("funnel = %v", got.FunnelCommand)
	}
	wantCF := []string{"cloudflared", "tunnel", "--url", "http://127.0.0.1:7780"}
	if strings.Join(got.CloudflareCommand, " ") != strings.Join(wantCF, " ") {
		t.Fatalf("cloudflare = %v", got.CloudflareCommand)
	}
	wantOff := []string{"tailscale", "funnel", "off"}
	if strings.Join(got.FunnelOffCommand, " ") != strings.Join(wantOff, " ") {
		t.Fatalf("off = %v", got.FunnelOffCommand)
	}

	placeholder := buildRemoteExposeCommands(9001, "")
	if placeholder.HostnameKnown || placeholder.Hostname != hostnamePlaceholder {
		t.Fatalf("placeholder hostname = %+v", placeholder)
	}
	if placeholder.ListenerPort != 9001 {
		t.Fatalf("port = %d", placeholder.ListenerPort)
	}
}

func TestRemoteExposePrintsTunnelCommands(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	human := run(t, "remote", "expose", "--hostname", remoteExposeExampleHost, "--port", "7780")
	for _, want := range []string{
		"executes\tnothing",
		"mora' 'mcp' 'serve-http'",
		"'--allow-host' '" + remoteExposeExampleHost + "'",
		"'tailscale' 'funnel' '--bg' '--https=443'",
		"'cloudflared' 'tunnel' '--url'",
		"'tailscale' 'funnel' 'off'",
		"No active agent profiles yet",
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("human output missing %q:\n%s", want, human)
		}
	}

	raw := run(t, "remote", "expose", "--hostname", remoteExposeExampleHost, "--json")
	var got remoteExposePayload
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatal(err)
	}
	if top["schema"] != schemaRemoteExpose || top["schema_version"] != float64(1) {
		t.Fatalf("envelope = schema %v v%v", top["schema"], top["schema_version"])
	}
	if !got.ExecutesNothing {
		t.Fatal("json executes_nothing=false")
	}
	if strings.Join(got.FunnelCommand, " ") != "tailscale funnel --bg --https=443 http://127.0.0.1:7780" {
		t.Fatalf("funnel command = %v", got.FunnelCommand)
	}
}

func TestRemoteExposeDoesNotRunTunnels(t *testing.T) {
	// Source-level witness: the property is "this command execs nothing".
	// A behavioral assertion would only prove tunnel binaries were absent
	// from PATH on the machine that ran the test.
	src, err := os.ReadFile("remote.go")
	if err != nil {
		t.Fatalf("read remote.go: %v", err)
	}
	body := funcBody(t, string(src), "func cmdRemoteExpose(")
	for _, forbidden := range []string{"exec.", "os/exec", "exec.Command", "exec.LookPath", "Start(", "Run(", "CombinedOutput("} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("cmdRemoteExpose reaches for %q; expose prints commands, it does not run them", forbidden)
		}
	}
	if strings.Contains(string(src), `"os/exec"`) {
		t.Fatal("remote.go imports os/exec")
	}
	buildBody := funcBody(t, string(src), "func buildRemoteExposeCommands(")
	for _, forbidden := range []string{"exec.", "os/exec", "exec.Command", "os.StartProcess"} {
		if strings.Contains(buildBody, forbidden) {
			t.Fatalf("buildRemoteExposeCommands reaches for %q", forbidden)
		}
	}
}

func TestRemoteExposePrintsOnlyOneCommandPerLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the printed lines are POSIX shell lines; there is no /bin/sh here")
	}
	got := buildRemoteExposeCommands(7780, remoteExposeExampleHost)
	for _, argv := range [][]string{got.ListenCommand, got.FunnelCommand, got.CloudflareCommand, got.FunnelOffCommand} {
		assertOneShellCommand(t, argv)
	}
	// Injection in the hostname must stay one argument, not a second command.
	assertOneShellCommand(t, []string{"tailscale", "funnel", "--bg", "--https=443", "http://node.example;id/"})
	assertOneShellCommand(t, buildRemoteExposeCommands(7780, "a b'c;$(id)`id`").ListenCommand)
}

func TestRemoteExposeRefusesBadPort(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	var stdout, stderr strings.Builder
	err := Run(testCtx(t), []string{"remote", "expose", "--port", "0"}, &stdout, &stderr, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "invalid --port") {
		t.Fatalf("err = %v stdout=%s", err, stdout.String())
	}
}
