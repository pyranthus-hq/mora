package mora

// CLI witnesses for graph node N22.
//
// The listener's half of the boundary — the Host allowlist, the loopback-peer
// requirement, the startup validation — is proved in internal/companion, against
// the package that owns it. What is proved here is what `expose` promises: the
// exact commands, the refusals, and that a command whose whole job is to print
// never prints a secret.
//
// Every hostname in this file is a documentation placeholder. A real tailnet
// name is a fact about the maintainer's network and does not belong in a
// repository.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/companion"
)

// exampleNode is the RFC 2606 .example TLD, so it can never resolve to anything
// real, and it has the shape of a MagicDNS name so the port-elision rules below
// are exercised against something realistic.
const exampleNode = "node.tailnet-example.example"

// activateOneDevice takes a device all the way to active and returns its live
// token, which the secret tests then look for in expose's output.
//
// It goes through the registry rather than the CLI because there is no CLI verb
// that confirms a pairing: confirmation is the phone's half of the exchange.
func activateOneDevice(t *testing.T) string {
	t.Helper()
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	reg := companion.NewRegistry(cfg.ConfigDir, cfg.StateDir,
		companion.WithClock(func() time.Time { return cfg.OperationClock() }))
	payload, err := reg.Pair("phone", companion.PlatformIOS, "http://127.0.0.1:7778")
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	confirmation := companion.NewPairingConfirmation()
	confirmation.DeviceID = payload.DeviceID
	confirmation.PairingCode = payload.PairingCode
	confirmation.Label = "phone"
	confirmation.Platform = companion.PlatformIOS
	confirmation.PublicKey = "ed25519:" + strings.Repeat("A", 43) + "="
	confirmation.ConfirmedAt = payload.ExpiresAt
	token, _, err := reg.Confirm(confirmation)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return token
}

// decodeExpose decodes the receipt and pins its envelope.
func decodeExpose(t *testing.T, out string) companionExposePayload {
	t.Helper()
	decodeCompanion(t, out, schemaCompanionExpose)
	var got companionExposePayload
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("expose payload does not decode: %v\n%s", err, out)
	}
	return got
}

func TestCompanionExposeRefusesUntilADeviceIsActive(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	// Empty registry.
	stdout, _, err := runSplit(t, "companion", "expose", "--json")
	if err == nil {
		t.Fatalf("expose published with no devices at all\nstdout: %s", stdout)
	}
	if !strings.Contains(err.Error(), "mora companion pair") {
		t.Fatalf("the refusal does not name the command that fixes it: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("a refused expose still wrote to stdout:\n%s", stdout)
	}

	// A PENDING device is still not a device that can authenticate. This is the
	// case a count of "paired" devices would get wrong: `companion pair` has
	// run, a code is live, and the listener would still answer 401 to everything
	// the phone sent.
	run(t, "companion", "pair", "--json")
	stdout, _, err = runSplit(t, "companion", "expose", "--json")
	if err == nil {
		t.Fatalf("expose published for a pending device\nstdout: %s", stdout)
	}
	if !strings.Contains(err.Error(), "no active paired device") {
		t.Fatalf("the refusal does not say the device is not active: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("a refused expose still wrote to stdout:\n%s", stdout)
	}

	// And it prints once a device is genuinely usable.
	activateOneDevice(t)
	got := decodeExpose(t, run(t, "companion", "expose", "--hostname", exampleNode, "--json"))
	if got.ActiveDevices != 1 {
		t.Fatalf("active_devices = %d, want 1", got.ActiveDevices)
	}
}

func TestCompanionExposePrintsTheExactTailscaleCommands(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	activateOneDevice(t)

	// Serve omits the port from the URL — and therefore from the Host header it
	// forwards — exactly when the port is the scheme's default. The allow-host
	// value has to match what actually arrives at the backend, so every
	// combination is pinned rather than the common one.
	for _, tc := range []struct {
		name      string
		args      []string
		serve     string
		off       string
		allowHost string
		publicURL string
	}{
		{
			name:      "https on the default port drops it from Host",
			args:      []string{"--hostname", exampleNode},
			serve:     "tailscale serve --bg --https=443 http://127.0.0.1:7778",
			off:       "tailscale serve --https=443 off",
			allowHost: exampleNode,
			publicURL: "https://" + exampleNode + "/",
		},
		{
			name:      "https on a non-default port keeps it",
			args:      []string{"--hostname", exampleNode, "--tailnet-port", "8443"},
			serve:     "tailscale serve --bg --https=8443 http://127.0.0.1:7778",
			off:       "tailscale serve --https=8443 off",
			allowHost: exampleNode + ":8443",
			publicURL: "https://" + exampleNode + ":8443/",
		},
		{
			name:      "plaintext on the default port drops it",
			args:      []string{"--hostname", exampleNode, "--plaintext"},
			serve:     "tailscale serve --bg --http=80 http://127.0.0.1:7778",
			off:       "tailscale serve --http=80 off",
			allowHost: exampleNode,
			publicURL: "http://" + exampleNode + "/",
		},
		{
			name:      "plaintext on a non-default port keeps it",
			args:      []string{"--hostname", exampleNode, "--plaintext", "--tailnet-port", "8080"},
			serve:     "tailscale serve --bg --http=8080 http://127.0.0.1:7778",
			off:       "tailscale serve --http=8080 off",
			allowHost: exampleNode + ":8080",
			publicURL: "http://" + exampleNode + ":8080/",
		},
		{
			name:      "a different listener port moves the proxy target",
			args:      []string{"--hostname", exampleNode, "--port", "9999"},
			serve:     "tailscale serve --bg --https=443 http://127.0.0.1:9999",
			off:       "tailscale serve --https=443 off",
			allowHost: exampleNode,
			publicURL: "https://" + exampleNode + "/",
		},
	} {
		subRun(t, tc.name, func(t *testing.T) {
			got := decodeExpose(t, run(t, append([]string{"companion", "expose", "--json"}, tc.args...)...))
			if line := shellLine(got.ServeCommand); line != tc.serve {
				t.Fatalf("serve_command = %q, want %q", line, tc.serve)
			}
			if line := shellLine(got.OffCommand); line != tc.off {
				t.Fatalf("off_command = %q, want %q", line, tc.off)
			}
			if line := shellLine(got.ResetCommand); line != "tailscale serve reset" {
				t.Fatalf("reset_command = %q", line)
			}
			if got.AllowHost != tc.allowHost {
				t.Fatalf("allow_host = %q, want %q", got.AllowHost, tc.allowHost)
			}
			if got.PublicURL != tc.publicURL {
				t.Fatalf("public_url = %q, want %q", got.PublicURL, tc.publicURL)
			}
			// The value expose prints must be one the listener will actually
			// accept, or the operator debugs a 403 against a phone.
			if _, err := companion.CheckAllowHost(got.AllowHost); err != nil {
				t.Fatalf("expose printed an --allow-host its own listener refuses: %v", err)
			}
			if !strings.Contains(shellLine(got.ListenCommand), "--allow-host "+tc.allowHost) {
				t.Fatalf("listen_command does not carry the allow-host: %q", shellLine(got.ListenCommand))
			}
			// Funnel is stated, and no command that enables it is ever printed.
			if got.Funnel != "off" {
				t.Fatalf("funnel = %q, want off", got.Funnel)
			}
			for _, argv := range [][]string{got.ServeCommand, got.OffCommand, got.ResetCommand, got.ListenCommand} {
				if strings.Contains(shellLine(argv), "funnel") {
					t.Fatalf("expose printed a Funnel command: %q", shellLine(argv))
				}
			}
		})
	}
}

func TestCompanionExposeWithoutAHostnamePrintsAPlaceholderTheListenerRefuses(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	activateOneDevice(t)

	got := decodeExpose(t, run(t, "companion", "expose", "--json"))
	if got.HostnameKnown {
		t.Fatal("hostname_known is true when no hostname was supplied")
	}
	if got.Hostname != hostnamePlaceholder {
		t.Fatalf("hostname = %q, want the placeholder", got.Hostname)
	}
	// The placeholder must fail CLOSED. A command pasted without editing has to
	// die at startup with a named error, not start a listener whose allowed host
	// nothing on the network can send.
	if _, err := companion.CheckAllowHost(got.AllowHost); err == nil {
		t.Fatal("the unedited placeholder is accepted as an --allow-host value")
	}

	human := run(t, "companion", "expose")
	if !strings.Contains(human, "tailscale status --json") {
		t.Fatalf("the human output does not say how to find the node name:\n%s", human)
	}
}

func TestCompanionExposeNeverPrintsACredential(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	token := activateOneDevice(t)

	pair := decodeCompanion(t, run(t, "companion", "pair", "--json"), schemaCompanionPair)
	code, _ := pair["pairing_code"].(string)
	if token == "" || code == "" {
		t.Fatal("the test did not produce the secrets it is supposed to look for")
	}

	for _, out := range []string{
		run(t, "companion", "expose", "--hostname", exampleNode),
		run(t, "companion", "expose", "--hostname", exampleNode, "--json"),
	} {
		if strings.Contains(out, token) {
			t.Fatal("expose printed a live device token")
		}
		if strings.Contains(out, code) {
			t.Fatal("expose printed a live pairing code")
		}
		// Nor a fingerprint of either: a fingerprint is not a secret, but it is
		// a per-install value, and expose reads counts only.
		if strings.Contains(out, "sha256:") {
			t.Fatalf("expose printed a fingerprint:\n%s", out)
		}
	}
}

func TestCompanionExposeRefusesArgumentsThatWouldPrintABrokenCommand(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	activateOneDevice(t)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"port zero is not a configuration", []string{"--port", "0"}, "not configured"},
		{"port out of range", []string{"--port", "70000"}, "between 1 and 65535"},
		{"negative port", []string{"--port", "-1"}, "between 1 and 65535"},
		{"tailnet port out of range", []string{"--tailnet-port", "70000"}, "between 1 and 65535"},
		{"hostname with a scheme", []string{"--hostname", "https://" + exampleNode}, "invalid --hostname"},
		{"hostname with a path", []string{"--hostname", exampleNode + "/v1"}, "invalid --hostname"},
		{"hostname with a wildcard", []string{"--hostname", "*." + exampleNode}, "invalid --hostname"},
		{"hostname list", []string{"--hostname", exampleNode + "," + exampleNode}, "invalid --hostname"},
		{"unedited placeholder", []string{"--hostname", hostnamePlaceholder}, "invalid --hostname"},
		{"loopback is already allowed", []string{"--hostname", "127.0.0.1"}, "already accepted"},
		{"unknown flag", []string{"--bogus"}, "bogus"},
		{"stray positional", []string{"extra"}, "unexpected argument"},
	} {
		subRun(t, tc.name, func(t *testing.T) {
			stdout, _, err := runSplit(t, append([]string{"companion", "expose"}, tc.args...)...)
			if err == nil {
				t.Fatalf("expose accepted %v\nstdout: %s", tc.args, stdout)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
			if strings.TrimSpace(stdout) != "" {
				t.Fatalf("a refused expose still wrote to stdout:\n%s", stdout)
			}
		})
	}
}

func TestCompanionExposeDoesNotRunTailscale(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	activateOneDevice(t)

	// A source-level witness, because the property is "this command execs
	// nothing", and a behavioral assertion would only prove that tailscale was
	// absent from PATH on the machine that ran the test.
	src, err := os.ReadFile("companion.go")
	if err != nil {
		t.Fatalf("read companion.go: %v", err)
	}
	body := funcBody(t, string(src), "func cmdCompanionExpose(")
	for _, forbidden := range []string{"exec.", "os/exec", "exec.Command", "exec.LookPath"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("cmdCompanionExpose reaches for %q; expose prints commands, it does not run them", forbidden)
		}
	}
	if strings.Contains(string(src), `"os/exec"`) {
		t.Fatal("companion.go imports os/exec")
	}
}
