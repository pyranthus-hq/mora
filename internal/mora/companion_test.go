package mora

// These are the CLI-level witnesses for graph node N11. The registry's own
// security properties — no secret persisted, constant-time comparison, mode
// hardening, lock behavior — are proved in internal/companion/registry_test.go
// against the package that owns them. What is proved here is the half the CLI
// owns: the envelope on every payload, the projection that must not carry a
// credential, the refusals, and the idempotence a script depends on.

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeCompanion decodes one `--json` document and pins its envelope. Every
// test goes through it, so a payload that lost its schema keys fails in the
// place that reads it rather than somewhere downstream.
func decodeCompanion(t *testing.T, out, wantSchema string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output does not decode as one JSON document: %v\n%s", err, out)
	}
	if got, _ := doc["schema"].(string); got != wantSchema {
		t.Fatalf("schema = %q, want %q", got, wantSchema)
	}
	if got, _ := doc["schema_version"].(float64); got != float64(companionSchemaVersion) {
		t.Fatalf("schema_version = %v, want %d", doc["schema_version"], companionSchemaVersion)
	}
	return doc
}

func TestCompanionPairEmitsAPairingPayloadAndRegistersAPendingDevice(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	doc := decodeCompanion(t, run(t, "companion", "pair", "--label", "Adit's phone", "--json"), schemaCompanionPair)

	deviceID, _ := doc["device_id"].(string)
	if !strings.HasPrefix(deviceID, "dev_") {
		t.Fatalf("device_id = %q, want a dev_ identifier", deviceID)
	}
	if code, _ := doc["pairing_code"].(string); code == "" {
		t.Fatal("pair emitted no pairing code; there is nothing for a phone to scan")
	}
	if host, _ := doc["host_fingerprint"].(string); !strings.HasPrefix(host, "sha256:") {
		t.Fatalf("host_fingerprint = %v, want a sha256: fingerprint", doc["host_fingerprint"])
	}
	// The default endpoint is the loopback listener, because that is the only
	// address a phone can reach before N12's Tailscale publication exists.
	if endpoint, _ := doc["endpoint"].(string); !strings.HasPrefix(endpoint, "http://127.0.0.1:") {
		t.Fatalf("endpoint = %v, want the loopback listener", doc["endpoint"])
	}
	if expires, _ := doc["expires_at"].(string); expires == "" {
		t.Fatal("the pairing window has no end")
	}

	list := decodeCompanion(t, run(t, "companion", "list", "--json"), schemaCompanionList)
	devices, _ := list["devices"].([]any)
	if len(devices) != 1 {
		t.Fatalf("after one pair, list holds %d devices", len(devices))
	}
	first, _ := devices[0].(map[string]any)
	if state, _ := first["state"].(string); state != "pending" {
		t.Fatalf("state after pair = %v, want pending", first["state"])
	}

	// The human rendering must warn that the code is a secret. This is the one
	// command in the group that prints one.
	human := run(t, "companion", "pair")
	if !strings.Contains(human, "one-time secret") {
		t.Fatalf("pair prints a live secret with no warning:\n%s", human)
	}
	if strings.Contains(human, "\x1b[") {
		t.Fatal("pair emitted ANSI to a non-terminal writer")
	}
}

func TestCompanionListProjectsDevicesWithoutCredentials(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	empty := decodeCompanion(t, run(t, "companion", "list", "--json"), schemaCompanionList)
	if devices, ok := empty["devices"].([]any); !ok || len(devices) != 0 {
		t.Fatalf("an empty registry must still carry an empty devices array, got %v", empty["devices"])
	}
	if human := run(t, "companion", "list"); !strings.Contains(human, "mora companion pair") {
		t.Fatalf("the empty list does not name the command that fixes it:\n%s", human)
	}

	pair := decodeCompanion(t, run(t, "companion", "pair", "--label", "phone", "--json"), schemaCompanionPair)
	code, _ := pair["pairing_code"].(string)

	list := decodeCompanion(t, run(t, "companion", "list", "--json"), schemaCompanionList)
	devices, _ := list["devices"].([]any)
	first, _ := devices[0].(map[string]any)
	// A list is the output most likely to end up in a screenshot or a bug
	// report, so the live pairing code must not survive into any field of it.
	// The check walks the DECODED document rather than the raw text: a
	// substring assertion over stdout keeps passing after the shape changes.
	for key, value := range first {
		if text, ok := value.(string); ok && strings.Contains(text, code) {
			t.Fatalf("the device projection leaks the live pairing code in %q", key)
		}
	}
	for _, forbidden := range []string{"pairing_code", "pairing_code_fingerprint", "pairing_expires_at", "public_key"} {
		if _, present := first[forbidden]; present {
			t.Fatalf("the device projection carries %q, which is storage state and not the published wire shape", forbidden)
		}
	}
	if schema, _ := first["schema"].(string); schema != "mora.companion.device" {
		t.Fatalf("device element schema = %v, want the published device schema", first["schema"])
	}
}

func TestCompanionRevokeEmitsVersionedReceiptAndIsIdempotent(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	pair := decodeCompanion(t, run(t, "companion", "pair", "--json"), schemaCompanionPair)
	deviceID, _ := pair["device_id"].(string)

	first := decodeCompanion(t, run(t, "companion", "revoke", deviceID, "--json"), schemaCompanionRevoke)
	if changed, _ := first["changed"].(bool); !changed {
		t.Fatal("the first revoke reported no change")
	}
	if state, _ := first["state"].(string); state != "revoked" {
		t.Fatalf("state = %v, want revoked", first["state"])
	}
	if at, _ := first["revoked_at"].(string); at == "" {
		t.Fatal("the receipt does not say when access ended")
	}

	// Re-revoking is a success with changed=false, not an error: an operator
	// acting on a suspicion should never be punished for running it twice.
	second := decodeCompanion(t, run(t, "companion", "revoke", deviceID, "--json"), schemaCompanionRevoke)
	if changed, _ := second["changed"].(bool); changed {
		t.Fatal("the second revoke claimed to change something")
	}
	if first["revoked_at"] != second["revoked_at"] {
		t.Fatal("the second revoke moved the revocation time")
	}

	human := run(t, "companion", "revoke", deviceID)
	if !strings.Contains(human, "already revoked") {
		t.Fatalf("the repeat rendering does not say nothing changed:\n%s", human)
	}

	list := decodeCompanion(t, run(t, "companion", "list", "--json"), schemaCompanionList)
	devices, _ := list["devices"].([]any)
	entry, _ := devices[0].(map[string]any)
	if state, _ := entry["state"].(string); state != "revoked" {
		t.Fatalf("the revocation did not persist; list shows %v", entry["state"])
	}
}

func TestCompanionStatusReportsCountsAndHostIdentity(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	empty := decodeCompanion(t, run(t, "companion", "status", "--json"), schemaCompanionStatus)
	if open, _ := empty["pairing_open"].(bool); open {
		t.Fatal("an empty registry reports an open pairing window")
	}
	// The host fingerprint is deliberately NOT here. It is random per install,
	// so a status document carrying it could never be frozen against a golden,
	// and the moment it is needed is the pairing moment, where `pair` prints it
	// for the human comparing two screens.
	if _, present := empty["host_fingerprint"]; present {
		t.Fatal("status carries the per-install host fingerprint; that belongs to pair")
	}

	// The identity a phone pins must not move between two pairings, or every
	// paired device would read a rename as a host substitution.
	first := decodeCompanion(t, run(t, "companion", "pair", "--json"), schemaCompanionPair)
	second := decodeCompanion(t, run(t, "companion", "pair", "--json"), schemaCompanionPair)
	if first["host_fingerprint"] != second["host_fingerprint"] {
		t.Fatal("the host fingerprint changed between two pairings")
	}

	open := decodeCompanion(t, run(t, "companion", "status", "--json"), schemaCompanionStatus)
	if isOpen, _ := open["pairing_open"].(bool); !isOpen {
		t.Fatal("status does not report the window a fresh pairing opened")
	}
	if pending, _ := open["pending"].(float64); pending != 2 {
		t.Fatalf("pending = %v, want 2", open["pending"])
	}

	// A status line is screenshot material: it must not carry the home
	// directory it was read from.
	human := run(t, "companion", "status")
	if strings.Contains(human, "/.config/") || strings.Contains(human, "/.local/") {
		t.Fatalf("status printed a filesystem path:\n%s", human)
	}
}

func TestCompanionRefusesUnknownSubcommandAndDashLedDeviceID(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"bare group", []string{"companion"}, "pair|list|revoke|status"},
		{"unknown subcommand", []string{"companion", "purge"}, "unknown companion subcommand"},
		{"dash-led device id", []string{"companion", "revoke", "--json"}, "is a flag, not the device id"},
		{"revoke with no id", []string{"companion", "revoke"}, "usage: mora companion revoke"},
		{"unknown flag", []string{"companion", "list", "--bogus"}, "bogus"},
		{"stray positional", []string{"companion", "status", "extra"}, "unexpected argument"},
		{"unknown platform", []string{"companion", "pair", "--platform", "android"}, "platform"},
	} {
		subRun(t, tc.name, func(t *testing.T) {
			stdout, _, err := runSplit(t, tc.args...)
			if err == nil {
				t.Fatalf("%v was accepted\nstdout: %s", tc.args, stdout)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
			if strings.TrimSpace(stdout) != "" {
				t.Fatalf("a refused command still wrote to stdout:\n%s", stdout)
			}
		})
	}

	// `companion revoke --json` is the case the dash-led guard exists for: it
	// reads as a flag to a human and as a device id to a naive parser, and the
	// registry must never have been touched.
	list := decodeCompanion(t, run(t, "companion", "list", "--json"), schemaCompanionList)
	if devices, _ := list["devices"].([]any); len(devices) != 0 {
		t.Fatalf("a refused command registered %d device(s)", len(devices))
	}
}

func TestCompanionErrorsNameTheNextCommand(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	_, _, err := runSplit(t, "companion", "revoke", "dev_neverissued", "--json")
	if err == nil {
		t.Fatal("revoking an unknown device succeeded")
	}
	if !strings.Contains(err.Error(), "mora companion list") {
		t.Fatalf("the error does not name the command that shows the real ids: %v", err)
	}
}
