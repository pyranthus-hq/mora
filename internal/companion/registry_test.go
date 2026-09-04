package companion

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// testRegistry returns a registry over two fresh temp roots with a pinned
// clock. The clock starts at a fixed instant and only moves when a test moves
// it, so a pairing window closes because the test said so and never because the
// suite was slow.
func testRegistry(t *testing.T) (*Registry, *time.Time, string, string) {
	t.Helper()
	configDir, stateDir := t.TempDir(), t.TempDir()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	clock := &now
	reg := NewRegistry(configDir, stateDir, WithClock(func() time.Time { return *clock }))
	return reg, clock, configDir, stateDir
}

// pairAndConfirm drives the whole happy path and returns the minted token.
func pairAndConfirm(t *testing.T, reg *Registry, label string) (string, Device) {
	t.Helper()
	payload, err := reg.Pair(label, PlatformIOS, "http://127.0.0.1:7777")
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	c := NewPairingConfirmation()
	c.DeviceID = payload.DeviceID
	c.PairingCode = payload.PairingCode
	c.Label = label
	c.Platform = PlatformIOS
	c.PublicKey = testPublicKey
	c.ConfirmedAt = payload.ExpiresAt
	token, dev, err := reg.Confirm(c)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return token, dev
}

// testPublicKey is 32 fixed bytes in the "<alg>:<base64>" form
// validatePublicKey accepts. The registry stores the key and does not interpret
// it, so a constant is enough.
var testPublicKey = "ed25519:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

func TestRegistryPairConfirmAuthenticateRoundTrip(t *testing.T) {
	reg, _, _, _ := testRegistry(t)

	payload, err := reg.Pair("Adit's phone", PlatformIOS, "http://127.0.0.1:7777")
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	if err := payload.Validate(); err != nil {
		t.Fatalf("pair payload does not satisfy the published schema: %v", err)
	}
	if !strings.HasPrefix(payload.DeviceID, PrefixDevice) {
		t.Fatalf("device id %q does not carry the published prefix", payload.DeviceID)
	}

	before, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].State != DevicePending {
		t.Fatalf("after pair, want one pending device, got %+v", before)
	}
	if before[0].TokenFingerprint != "" {
		t.Fatal("a pending device must not name a credential it has not been issued")
	}

	c := NewPairingConfirmation()
	c.DeviceID = payload.DeviceID
	c.PairingCode = payload.PairingCode
	c.Label = "Adit's phone"
	c.Platform = PlatformIOS
	c.PublicKey = testPublicKey
	c.ConfirmedAt = payload.ExpiresAt

	token, dev, err := reg.Confirm(c)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if token == "" {
		t.Fatal("confirm minted no token")
	}
	if dev.State != DeviceActive {
		t.Fatalf("confirmed device state = %q, want active", dev.State)
	}
	if dev.TokenFingerprint != Fingerprint(token) {
		t.Fatal("the projected fingerprint is not the fingerprint of the minted token")
	}

	got, err := reg.Authenticate(token)
	if err != nil {
		t.Fatalf("authenticate with the minted token: %v", err)
	}
	if got.DeviceID != payload.DeviceID {
		t.Fatalf("authenticate resolved %q, want %q", got.DeviceID, payload.DeviceID)
	}
}

// TestRegistryPersistsNoSecrets is the claim registry.go's package comment
// makes, checked against the actual bytes: neither the one-time code nor the
// bearer token may appear anywhere under ConfigDir or StateDir.
func TestRegistryPersistsNoSecrets(t *testing.T) {
	reg, _, configDir, stateDir := testRegistry(t)

	payload, err := reg.Pair("phone", PlatformIOS, "http://127.0.0.1:7777")
	if err != nil {
		t.Fatal(err)
	}
	// The code is checked while it is still live, before it is spent: a
	// registry that wrote the code and erased it on confirm would still have
	// written it.
	assertAbsentFromTree(t, configDir, "pairing code", payload.PairingCode)
	assertAbsentFromTree(t, stateDir, "pairing code", payload.PairingCode)

	c := NewPairingConfirmation()
	c.DeviceID = payload.DeviceID
	c.PairingCode = payload.PairingCode
	c.Label = "phone"
	c.Platform = PlatformIOS
	c.PublicKey = testPublicKey
	c.ConfirmedAt = payload.ExpiresAt
	token, _, err := reg.Confirm(c)
	if err != nil {
		t.Fatal(err)
	}

	assertAbsentFromTree(t, configDir, "bearer token", token)
	assertAbsentFromTree(t, stateDir, "bearer token", token)
	assertAbsentFromTree(t, configDir, "pairing code", payload.PairingCode)

	// The fingerprint IS expected on disk — that is what makes authentication
	// possible without storing the credential — so its presence is asserted
	// too. Otherwise this test would pass just as well against a registry that
	// persisted nothing at all.
	body, err := os.ReadFile(filepath.Join(configDir, "companion", "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(Fingerprint(token))) {
		t.Fatal("devices.json does not carry the token fingerprint; nothing could authenticate")
	}
}

func assertAbsentFromTree(t *testing.T, root, what, secret string) {
	t.Helper()
	if secret == "" {
		t.Fatalf("%s is empty; the check would pass vacuously", what)
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("%s is stored in plaintext at %s", what, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRevokeEndsAuthenticationAndIsIdempotent(t *testing.T) {
	reg, _, _, stateDir := testRegistry(t)
	token, dev := pairAndConfirm(t, reg, "phone")

	revoked, changed, err := reg.Revoke(dev.DeviceID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !changed {
		t.Fatal("the first revoke reported no change")
	}
	if revoked.State != DeviceRevoked || revoked.RevokedAt == "" {
		t.Fatalf("revoked device = %+v, want state revoked with a timestamp", revoked)
	}
	if err := revoked.Validate(); err != nil {
		t.Fatalf("revoked device does not satisfy the published schema: %v", err)
	}

	// The credential must stop working, and it must stop working with the
	// reason that tells an operator this was a device they knew about.
	_, err = reg.Authenticate(token)
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("authenticate after revoke returned %v, want an *AuthError", err)
	}
	if authErr.Reason != ReasonRevokedDevice {
		t.Fatalf("reason = %q, want %q", authErr.Reason, ReasonRevokedDevice)
	}

	again, changed, err := reg.Revoke(dev.DeviceID)
	if err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if changed {
		t.Fatal("the second revoke claimed to change something")
	}
	if again.RevokedAt != revoked.RevokedAt {
		t.Fatal("the second revoke moved the revocation time")
	}

	// A repeat writes no receipt: the audit trail records revocations, not
	// attempts to repeat one.
	if got := len(receiptFiles(t, stateDir, "revoked")); got != 1 {
		t.Fatalf("revocation receipts = %d, want exactly 1", got)
	}

	if _, _, err := reg.Revoke("dev_missing"); !errors.Is(err, ErrNoSuchDevice) {
		t.Fatalf("revoking an unknown device returned %v, want ErrNoSuchDevice", err)
	}
}

func TestRegistryConfirmRejectsWrongCodeExpiredCodeAndReplay(t *testing.T) {
	t.Run("a wrong code does not burn somebody else's pending pairing", func(t *testing.T) {
		reg, clock, _, _ := testRegistry(t)
		payload, err := reg.Pair("phone", PlatformIOS, "http://127.0.0.1:7777")
		if err != nil {
			t.Fatal(err)
		}
		*clock = clock.Add(PairingTTL + time.Second)
		wrong := confirmationFor(payload)
		wrong.PairingCode = "NOTTHERIGHTCODE"
		if _, _, err := reg.Confirm(wrong); !errors.Is(err, ErrPairingCode) {
			t.Fatalf("a wrong late code returned %v, want ErrPairingCode", err)
		}
		// The expiry burn fires only for a code that MATCHED. Otherwise anyone
		// who can reach the listener kills a pairing by guessing.
		*clock = clock.Add(-PairingTTL - time.Second)
		if _, _, err := reg.Confirm(confirmationFor(payload)); err != nil {
			t.Fatalf("a wrong guess burned the real code: %v", err)
		}
	})

	t.Run("wrong code", func(t *testing.T) {
		reg, _, _, _ := testRegistry(t)
		payload, err := reg.Pair("phone", PlatformIOS, "http://127.0.0.1:7777")
		if err != nil {
			t.Fatal(err)
		}
		c := confirmationFor(payload)
		c.PairingCode = "NOTTHERIGHTCODE"
		if _, _, err := reg.Confirm(c); !errors.Is(err, ErrPairingCode) {
			t.Fatalf("confirm with a wrong code returned %v, want ErrPairingCode", err)
		}
		// A failed attempt must leave the window open; otherwise one typo from
		// anybody who can reach the listener denies the real device its pairing.
		devices, err := reg.List()
		if err != nil {
			t.Fatal(err)
		}
		if devices[0].State != DevicePending {
			t.Fatalf("state after a wrong code = %q, want pending", devices[0].State)
		}
	})

	t.Run("expired code", func(t *testing.T) {
		reg, clock, _, _ := testRegistry(t)
		payload, err := reg.Pair("phone", PlatformIOS, "http://127.0.0.1:7777")
		if err != nil {
			t.Fatal(err)
		}
		*clock = clock.Add(PairingTTL + time.Second)
		if _, _, err := reg.Confirm(confirmationFor(payload)); !errors.Is(err, ErrPairingExpired) {
			t.Fatalf("confirm after the TTL returned %v, want ErrPairingExpired", err)
		}
		// The late code is BURNED, not merely refused: a second attempt no
		// longer has a code to present.
		if _, _, err := reg.Confirm(confirmationFor(payload)); !errors.Is(err, ErrNotPending) {
			t.Fatalf("second confirm after expiry returned %v, want ErrNotPending", err)
		}
		// Turning the clock back must not revive it. On a laptop the clock is
		// user-controlled, so a TTL enforced only by comparison would mean
		// yesterday's photographed QR code works again tomorrow.
		*clock = clock.Add(-2 * PairingTTL)
		if _, _, err := reg.Confirm(confirmationFor(payload)); !errors.Is(err, ErrNotPending) {
			t.Fatalf("a rolled-back clock revived an expired code: %v", err)
		}
		devices, err := reg.List()
		if err != nil {
			t.Fatal(err)
		}
		if devices[0].State != DevicePending {
			t.Fatalf("burning the code changed the stored state to %q", devices[0].State)
		}
	})

	t.Run("replay", func(t *testing.T) {
		reg, _, _, _ := testRegistry(t)
		payload, err := reg.Pair("phone", PlatformIOS, "http://127.0.0.1:7777")
		if err != nil {
			t.Fatal(err)
		}
		c := confirmationFor(payload)
		first, _, err := reg.Confirm(c)
		if err != nil {
			t.Fatal(err)
		}
		second, _, err := reg.Confirm(c)
		if !errors.Is(err, ErrNotPending) {
			t.Fatalf("replayed confirmation returned %v, want ErrNotPending", err)
		}
		if second != "" {
			t.Fatal("a replayed confirmation minted a second token")
		}
		if _, err := reg.Authenticate(first); err != nil {
			t.Fatalf("the first token stopped working after a replay: %v", err)
		}
	})

	t.Run("unknown device", func(t *testing.T) {
		reg, _, _, _ := testRegistry(t)
		c := NewPairingConfirmation()
		c.DeviceID = "dev_nothinghere"
		c.PairingCode = "ANYTHING"
		c.Label = "phone"
		c.Platform = PlatformIOS
		c.PublicKey = testPublicKey
		c.ConfirmedAt = "2026-09-03T12:00:00Z"
		if _, _, err := reg.Confirm(c); !errors.Is(err, ErrNoSuchDevice) {
			t.Fatalf("confirm for an unknown device returned %v, want ErrNoSuchDevice", err)
		}
	})
}

func confirmationFor(payload PairingPayload) PairingConfirmation {
	c := NewPairingConfirmation()
	c.DeviceID = payload.DeviceID
	c.PairingCode = payload.PairingCode
	c.Label = "phone"
	c.Platform = PlatformIOS
	c.PublicKey = testPublicKey
	c.ConfirmedAt = payload.ExpiresAt
	return c
}

func TestRegistryAuthenticateSeparatesRevokedFromUnknown(t *testing.T) {
	reg, _, _, _ := testRegistry(t)
	live, _ := pairAndConfirm(t, reg, "keeper")
	doomed, doomedDev := pairAndConfirm(t, reg, "stolen")
	if _, _, err := reg.Revoke(doomedDev.DeviceID); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		token string
		want  RejectReason
	}{
		{"revoked token", doomed, ReasonRevokedDevice},
		{"never issued", "NEVERISSUEDTOKEN", ReasonUnknownDevice},
		{"empty token", "", ReasonUnknownDevice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reg.Authenticate(tc.token)
			var authErr *AuthError
			if !errors.As(err, &authErr) {
				t.Fatalf("err = %v, want an *AuthError", err)
			}
			if authErr.Reason != tc.want {
				t.Fatalf("reason = %q, want %q", authErr.Reason, tc.want)
			}
		})
	}

	// Revoking one device must not disturb another.
	if _, err := reg.Authenticate(live); err != nil {
		t.Fatalf("the surviving device stopped authenticating: %v", err)
	}
}

// TestRegistryAuthenticateComparesInConstantTime is a source-level witness, not
// a timing measurement. A wall-clock timing assertion over SHA-256 comparisons
// is noise on a loaded CI box and would be quarantined within a week; what can
// be asserted deterministically are the four properties that make the
// comparison constant time in the first place.
//
//  1. subtle.ConstantTimeCompare rather than ==.
//  2. No exit out of the loop once a match is found, so the running time does
//     not depend on WHICH device matched.
//  3. No branch inside the loop at all — a `continue` past a credential-less
//     device makes the cost depend on how many devices carry one.
//  4. No return before the loop that depends on the TOKEN. This is the one the
//     first round missed: `if token == "" { return }` answered "was that even a
//     token?" for free, before any comparison ran.
func TestRegistryAuthenticateComparesInConstantTime(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "registry.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if ok && d.Name.Name == "Authenticate" && d.Recv != nil {
			fn = d
		}
	}
	if fn == nil {
		t.Fatal("Authenticate not found in registry.go")
	}

	// The parameter's own name, so renaming it cannot quietly retire claim 4.
	if len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) != 1 {
		t.Fatalf("Authenticate takes %d parameter groups; the witness assumes one named parameter", len(fn.Type.Params.List))
	}
	param := fn.Type.Params.List[0].Names[0].Name

	var loop *ast.RangeStmt
	loops := 0
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if rng, ok := node.(*ast.RangeStmt); ok {
			loops++
			loop = rng
		}
		return true
	})
	if loops != 1 {
		t.Fatalf("Authenticate has %d range loops; the witness assumes exactly one", loops)
	}

	// Claim 4: nothing before the loop may return, and the check is on the
	// STATEMENT list rather than on a nested walk, so a return buried in an
	// `if` above the loop is caught too.
	for _, stmt := range fn.Body.List {
		if stmt.Pos() >= loop.Pos() {
			break
		}
		// A return that cannot see the token cannot leak anything about it:
		// failing to load the registry, or failing to read entropy, costs the
		// same for every caller. A statement that does BOTH — reads the token
		// and can return — is the shape being forbidden.
		var returns, readsToken bool
		ast.Inspect(stmt, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.ReturnStmt:
				returns = true
			case *ast.Ident:
				if n.Name == param {
					readsToken = true
				}
			}
			return true
		})
		if returns && readsToken {
			t.Fatalf("a statement above Authenticate's comparison loop both reads %q and can return; "+
				"an exit that depends on the token answers a question about it before any comparison runs",
				param)
		}
	}

	// Claims 1, 2 and 3, all inside the loop.
	var constantTime, branched bool
	ast.Inspect(loop.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "subtle" && sel.Sel.Name == "ConstantTimeCompare" {
					constantTime = true
				}
			}
		case *ast.ReturnStmt, *ast.BranchStmt:
			branched = true
		}
		return true
	})
	if !constantTime {
		t.Fatal("Authenticate does not compare fingerprints with subtle.ConstantTimeCompare")
	}
	if branched {
		t.Fatal("Authenticate branches out of its comparison loop; the time taken now depends on which device matched")
	}
}

func TestRegistryHardensDirectoryAndFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not the access-control mechanism on Windows")
	}
	reg, _, configDir, stateDir := testRegistry(t)
	pairAndConfirm(t, reg, "phone")

	dir := filepath.Join(configDir, "companion")
	assertMode(t, dir, secretDirMode)
	assertMode(t, filepath.Join(dir, "devices.json"), secretFileMode)
	assertMode(t, filepath.Join(dir, "host.key"), secretFileMode)
	assertMode(t, filepath.Join(stateDir, "companion", "receipts"), secretDirMode)
	for _, path := range receiptFiles(t, stateDir, "") {
		assertMode(t, path, secretFileMode)
	}

	// Loosening the modes by hand must not survive the next operation: the
	// registry re-asserts them on every open, because a restore from an archive
	// or a stray chmod -R is exactly how a 0600 file quietly becomes 0644.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "devices.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.List(); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Pair("second", PlatformIOS, "http://127.0.0.1:7777"); err != nil {
		t.Fatal(err)
	}
	assertMode(t, dir, secretDirMode)
	assertMode(t, filepath.Join(dir, "devices.json"), secretFileMode)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}

func TestRegistryWritesReceiptsToStateDirWithoutSecrets(t *testing.T) {
	reg, _, configDir, stateDir := testRegistry(t)
	payload, err := reg.Pair("phone", PlatformIOS, "http://127.0.0.1:7777")
	if err != nil {
		t.Fatal(err)
	}
	token, dev, err := reg.Confirm(confirmationFor(payload))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Revoke(dev.DeviceID); err != nil {
		t.Fatal(err)
	}

	// Receipts live in StateDir, never in the credential directory.
	if got := receiptFiles(t, configDir, ""); len(got) != 0 {
		t.Fatalf("receipts written into ConfigDir: %v", got)
	}
	for _, event := range []string{"paired", "confirmed", "revoked"} {
		if got := len(receiptFiles(t, stateDir, event)); got != 1 {
			t.Fatalf("%s receipts = %d, want 1", event, got)
		}
	}

	for _, path := range receiptFiles(t, stateDir, "") {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var row struct {
			Schema        string `json:"schema"`
			SchemaVersion int    `json:"schema_version"`
			Event         string `json:"event"`
			DeviceID      string `json:"device_id"`
			At            string `json:"at"`
		}
		if err := json.Unmarshal(body, &row); err != nil {
			t.Fatalf("%s does not decode: %v", path, err)
		}
		if row.Schema != schemaRegistryReceipt || row.SchemaVersion != SchemaVersion {
			t.Fatalf("%s carries %s/%d, want %s/%d", path, row.Schema, row.SchemaVersion, schemaRegistryReceipt, SchemaVersion)
		}
		if row.DeviceID != dev.DeviceID || row.At == "" {
			t.Fatalf("%s does not name the device and the moment: %+v", path, row)
		}
		if bytes.Contains(body, []byte(token)) || bytes.Contains(body, []byte(payload.PairingCode)) {
			t.Fatalf("%s carries a credential", path)
		}
	}
}

// receiptFiles lists receipt files under root, optionally filtered by event.
func receiptFiles(t *testing.T, root, event string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Base(filepath.Dir(path)) != "receipts" {
			return nil
		}
		if event != "" && !strings.Contains(d.Name(), "-"+event+"-") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRegistryConcurrentPairKeepsEveryDevice drives the lost-update case the
// lock exists for: two pairings racing over one record file, each of which read
// a file that did not yet contain the other.
func TestRegistryConcurrentPairKeepsEveryDevice(t *testing.T) {
	reg, _, _, _ := testRegistry(t)
	const n = 8

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = reg.Pair("phone", PlatformIOS, "http://127.0.0.1:7777")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("pair %d: %v", i, err)
		}
	}

	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != n {
		t.Fatalf("registry holds %d devices after %d concurrent pairings; the lost-update guard did not hold", len(devices), n)
	}
	seen := map[string]bool{}
	for _, d := range devices {
		if seen[d.DeviceID] {
			t.Fatalf("duplicate device id %q", d.DeviceID)
		}
		seen[d.DeviceID] = true
	}
}

func TestRegistryRejectsBadPairInput(t *testing.T) {
	reg, _, _, _ := testRegistry(t)
	for _, tc := range []struct {
		name     string
		label    string
		platform Platform
		endpoint string
	}{
		{"empty label", "", PlatformIOS, "http://127.0.0.1:7777"},
		{"unknown platform", "phone", Platform("android"), "http://127.0.0.1:7777"},
		{"cleartext non-loopback endpoint", "phone", PlatformIOS, "http://evil.example/"},
		{"userinfo endpoint", "phone", PlatformIOS, "http://127.0.0.1:@evil.example/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := reg.Pair(tc.label, tc.platform, tc.endpoint); err == nil {
				t.Fatal("pair accepted input the published schema refuses")
			}
			devices, err := reg.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(devices) != 0 {
				t.Fatalf("a refused pairing still wrote %d device(s)", len(devices))
			}
		})
	}
}

func TestRegistryStatusReportsCountsAndPairingWindow(t *testing.T) {
	reg, clock, _, _ := testRegistry(t)

	empty, err := reg.Status()
	if err != nil {
		t.Fatal(err)
	}
	if empty.Active+empty.Pending+empty.Revoked != 0 || empty.PairingOpen {
		t.Fatalf("empty registry status = %+v", empty)
	}

	payload, err := reg.Pair("pending phone", PlatformIOS, "http://127.0.0.1:7777")
	if err != nil {
		t.Fatal(err)
	}
	open, err := reg.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !open.PairingOpen || open.Pending != 1 || open.NextPairingExpiry != payload.ExpiresAt {
		t.Fatalf("status with a live code = %+v", open)
	}

	// The host fingerprint lives on PairingPayload, not on Status: it is the
	// value a phone pins at the pairing moment, and it must not move.
	second, err := reg.Pair("another phone", PlatformIOS, "http://127.0.0.1:7777")
	if err != nil {
		t.Fatal(err)
	}
	if second.HostFingerprint != payload.HostFingerprint {
		t.Fatal("the host fingerprint changed between two pairings")
	}
	if err := validateFingerprint("host_fingerprint", payload.HostFingerprint); err != nil {
		t.Fatalf("host fingerprint: %v", err)
	}

	*clock = clock.Add(PairingTTL + time.Second)
	closed, err := reg.Status()
	if err != nil {
		t.Fatal(err)
	}
	if closed.PairingOpen || closed.NextPairingExpiry != "" {
		t.Fatalf("status after the TTL = %+v, want a closed window", closed)
	}
	if closed.Pending != 2 {
		t.Fatal("an expired code silently changed a device's stored state")
	}
}

func TestRegistryMarkSeenRecordsLastSeen(t *testing.T) {
	reg, clock, _, _ := testRegistry(t)
	token, dev := pairAndConfirm(t, reg, "phone")

	// Authenticate is a read: it must not move last_seen_at on its own, or a
	// listener would serialize every request behind one file write.
	if _, err := reg.Authenticate(token); err != nil {
		t.Fatal(err)
	}
	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].LastSeenAt != "" {
		t.Fatal("Authenticate wrote last_seen_at")
	}

	*clock = clock.Add(time.Hour)
	if err := reg.MarkSeen(dev.DeviceID); err != nil {
		t.Fatal(err)
	}
	devices, err = reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].LastSeenAt != "2026-09-03T13:00:00Z" {
		t.Fatalf("last_seen_at = %q", devices[0].LastSeenAt)
	}
	if err := devices[0].Validate(); err != nil {
		t.Fatalf("device with last_seen_at fails the published schema: %v", err)
	}
	if err := reg.MarkSeen("dev_missing"); !errors.Is(err, ErrNoSuchDevice) {
		t.Fatalf("MarkSeen for an unknown device returned %v", err)
	}
}

func TestRegistrySurvivesRestartAndRefusesAFutureFile(t *testing.T) {
	reg, _, configDir, stateDir := testRegistry(t)
	token, dev := pairAndConfirm(t, reg, "phone")

	// A second Registry value over the same directories is what a second
	// process is: nothing may live only in memory.
	reopened := NewRegistry(configDir, stateDir)
	got, err := reopened.Authenticate(token)
	if err != nil {
		t.Fatalf("authenticate after reopen: %v", err)
	}
	if got.DeviceID != dev.DeviceID {
		t.Fatalf("reopened registry resolved %q", got.DeviceID)
	}

	path := filepath.Join(configDir, "companion", "devices.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(body, []byte(`"version": 1`), []byte(`"version": 99`), 1), secretFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.List(); err == nil || !strings.Contains(err.Error(), "upgrade mora") {
		t.Fatalf("a future record file returned %v; it must refuse and say what to do", err)
	}

	if err := os.WriteFile(path, []byte("{not json"), secretFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.List(); err == nil || !strings.Contains(err.Error(), "pair again") {
		t.Fatalf("a corrupt record file returned %v; it must refuse and say what to do", err)
	}
}

// TestRegistryLockIsMutuallyExclusive is the deterministic exclusion witness.
// It does not race two goroutines and hope: it takes the lock, proves a second
// acquisition cannot succeed while it is held, releases it, and proves the
// second acquisition then can.
func TestRegistryLockIsMutuallyExclusive(t *testing.T) {
	reg, _, _, _ := testRegistry(t)
	if err := reg.ensureDir(); err != nil {
		t.Fatal(err)
	}

	first, err := reg.lock()
	if err != nil {
		t.Fatal(err)
	}

	// A short deadline so the negative case is a test and not a five-second
	// pause. The assertion is that it never succeeds, not that it fails fast.
	blocked, err := acquireLock(reg.lockPath(), secretFileMode, 200*time.Millisecond, 10*time.Millisecond)
	if err == nil {
		blocked.release()
		t.Fatal("two holders took the same lock at the same time")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("a contended lock returned %v, want ErrLocked", err)
	}

	if err := first.release(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireLock(reg.lockPath(), secretFileMode, lockTimeout, lockPoll)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	if err := second.release(); err != nil {
		t.Fatal(err)
	}

	// The lock file itself is never removed. Removing it is what reintroduces
	// the window in which two holders exist, so its survival is the contract,
	// not litter.
	if _, err := os.Stat(reg.lockPath()); err != nil {
		t.Fatalf("the lock file was removed on release: %v", err)
	}
}

// TestRegistryRefusesToWriteAfterItsLockFileIsReplaced drives the check-then-use
// race the previous O_EXCL design could not close, and does it deterministically
// rather than by timing.
//
// The old failure ran: holder A checks that it still holds the lock, a sweeper
// removes the file, holder B creates a fresh one and writes, A writes its stale
// snapshot on top, and B's pairing is gone. Here the removal happens at the
// worst possible moment — inside the mutation, after any check A could have
// made — and A must refuse rather than write.
func TestRegistryRefusesToWriteAfterItsLockFileIsReplaced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows will not unlink a file while a handle is open, so the lock cannot be replaced under its holder")
	}
	reg, _, _, _ := testRegistry(t)
	if _, err := reg.Pair("first", PlatformIOS, "http://127.0.0.1:7777"); err != nil {
		t.Fatal(err)
	}

	err := reg.mutate(func(f *registryFile) (receipt, error) {
		// A sweeper decided this lock was stale and unlinked it; a second
		// writer is now loose on a fresh inode.
		if err := os.Remove(reg.lockPath()); err != nil {
			return receipt{}, err
		}
		stolen, err := acquireLock(reg.lockPath(), secretFileMode, time.Second, 10*time.Millisecond)
		if err != nil {
			return receipt{}, err
		}
		defer stolen.release()

		f.Devices = append(f.Devices, &deviceRecord{
			DeviceID:  "dev_20260903_120000_ffffffff",
			Label:     "smuggled",
			Platform:  PlatformIOS,
			State:     DevicePending,
			CreatedAt: "2026-09-03T12:00:00Z",
		})
		return receipt{Event: "paired", DeviceID: "dev_20260903_120000_ffffffff", At: "2026-09-03T12:00:00Z"}, nil
	})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("a mutation whose lock file was replaced returned %v, want ErrLocked", err)
	}

	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("the refused mutation still wrote: registry holds %d devices", len(devices))
	}
	if got := receiptFiles(t, reg.stateDir, "paired"); len(got) != 1 {
		t.Fatalf("paired receipts = %d, want 1 — the refused mutation left an audit row", len(got))
	}
}

// TestRegistryConcurrentPairAndRevokeKeepEveryChange runs the two mutating
// operations against each other. A lock that only serialized like operations
// would pass the pair-only test and still lose a revocation to a concurrent
// pairing, which is the direction that matters: a dropped revocation leaves a
// live credential.
func TestRegistryConcurrentPairAndRevokeKeepEveryChange(t *testing.T) {
	reg, _, _, _ := testRegistry(t)

	const existing = 6
	var doomed []string
	for i := 0; i < existing; i++ {
		_, dev := pairAndConfirm(t, reg, "seed")
		doomed = append(doomed, dev.DeviceID)
	}

	const fresh = 6
	var wg sync.WaitGroup
	pairErrs := make([]error, fresh)
	revokeErrs := make([]error, existing)
	for i := 0; i < fresh; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, pairErrs[i] = reg.Pair("new", PlatformIOS, "http://127.0.0.1:7777")
		}(i)
	}
	for i, id := range doomed {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			_, _, revokeErrs[i] = reg.Revoke(id)
		}(i, id)
	}
	wg.Wait()

	for i, err := range pairErrs {
		if err != nil {
			t.Fatalf("pair %d: %v", i, err)
		}
	}
	for i, err := range revokeErrs {
		if err != nil {
			t.Fatalf("revoke %d: %v", i, err)
		}
	}

	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != existing+fresh {
		t.Fatalf("registry holds %d devices, want %d — a concurrent write was lost", len(devices), existing+fresh)
	}
	state := map[string]DeviceState{}
	for _, d := range devices {
		state[d.DeviceID] = d.State
	}
	for _, id := range doomed {
		if state[id] != DeviceRevoked {
			t.Fatalf("%s is %q after a concurrent revoke; a dropped revocation leaves a live credential", id, state[id])
		}
	}
}

// TestRegistryReceiptFailureNeverActivatesACredential is the ordering witness.
//
// Confirm mints the token, hands its caller the only copy, and marks the device
// active. If the record file committed and the receipt then failed, the caller
// would see an error, discard the token, and leave an ACTIVE device whose
// credential nobody holds. Writing the receipt first makes that impossible.
func TestRegistryReceiptFailureNeverActivatesACredential(t *testing.T) {
	reg, _, _, stateDir := testRegistry(t)
	payload, err := reg.Pair("phone", PlatformIOS, "http://127.0.0.1:7777")
	if err != nil {
		t.Fatal(err)
	}

	// A regular file where the receipts directory belongs: MkdirAll refuses it,
	// so the receipt write fails for a reason no retry can fix.
	if err := os.RemoveAll(filepath.Join(stateDir, "companion", "receipts")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "companion", "receipts"), []byte("not a directory"), secretFileMode); err != nil {
		t.Fatal(err)
	}

	token, _, err := reg.Confirm(confirmationFor(payload))
	if err == nil {
		t.Fatal("Confirm succeeded with an unwritable audit trail")
	}
	if token != "" {
		t.Fatal("Confirm returned a token on a failed call")
	}

	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].State != DevicePending {
		t.Fatalf("state = %q after a failed Confirm; the record file committed ahead of the receipt", devices[0].State)
	}
	if devices[0].TokenFingerprint != "" {
		t.Fatal("a credential was activated that no caller holds")
	}
}

// TestRegistryHardensConfigDirItself pins the parent. A 0700 credential
// directory inside a 0755 ConfigDir still lets another local account list the
// parent, watch `companion` appear, and read the file count and timestamps off
// every pairing and revocation.
func TestRegistryHardensConfigDirItself(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not the access-control mechanism on Windows")
	}
	configDir, stateDir := t.TempDir(), t.TempDir()
	if err := os.Chmod(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(configDir, stateDir)

	if _, err := reg.Pair("phone", PlatformIOS, "http://127.0.0.1:7777"); err != nil {
		t.Fatal(err)
	}
	assertMode(t, configDir, secretDirMode)
	assertMode(t, filepath.Join(configDir, "companion"), secretDirMode)

	// The read path repairs it too. A user who only ever runs `mora companion
	// list` still deserves the mode.
	if err := os.Chmod(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.List(); err != nil {
		t.Fatal(err)
	}
	assertMode(t, configDir, secretDirMode)
}

// TestRegistryRefusesAnOversizeOrOvercrowdedRegistry pins the two read-path
// bounds N12's listener depends on: it loads this file on every request, so an
// unbounded one is a per-request memory amplifier.
func TestRegistryRefusesAnOversizeOrOvercrowdedRegistry(t *testing.T) {
	t.Run("byte cap", func(t *testing.T) {
		reg, _, configDir, _ := testRegistry(t)
		if _, err := reg.Pair("phone", PlatformIOS, "http://127.0.0.1:7777"); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(configDir, "companion", "devices.json")
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), MaxRegistryBytes+1), secretFileMode); err != nil {
			t.Fatal(err)
		}
		_, err := reg.Authenticate("anything")
		if err == nil || !strings.Contains(err.Error(), "over the") {
			t.Fatalf("an oversize registry returned %v; it must refuse before reading the bytes", err)
		}
	})

	t.Run("device cap", func(t *testing.T) {
		reg, _, configDir, _ := testRegistry(t)
		f := &registryFile{Version: registryFileVersion}
		for i := 0; i <= MaxDevices; i++ {
			f.Devices = append(f.Devices, &deviceRecord{
				DeviceID:  fmt.Sprintf("dev_20260903_120000_%08x", i),
				Label:     "crowd",
				Platform:  PlatformIOS,
				State:     DevicePending,
				CreatedAt: "2026-09-03T12:00:00Z",
			})
		}
		body, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(configDir, "companion"), secretDirMode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configDir, "companion", "devices.json"), body, secretFileMode); err != nil {
			t.Fatal(err)
		}
		if _, err := reg.List(); err == nil || !strings.Contains(err.Error(), "over the") {
			t.Fatalf("an overcrowded registry returned %v", err)
		}
	})

	t.Run("pair refuses at the cap", func(t *testing.T) {
		reg, _, _, _ := testRegistry(t)
		for i := 0; i < MaxDevices; i++ {
			if _, err := reg.Pair("phone", PlatformIOS, "http://127.0.0.1:7777"); err != nil {
				t.Fatalf("pair %d: %v", i, err)
			}
		}
		_, err := reg.Pair("one too many", PlatformIOS, "http://127.0.0.1:7777")
		if err == nil || !strings.Contains(err.Error(), "the limit is") {
			t.Fatalf("pairing past the cap returned %v; the bound must hold on the way in too", err)
		}
		devices, err := reg.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(devices) != MaxDevices {
			t.Fatalf("registry holds %d devices, want %d", len(devices), MaxDevices)
		}
	})
}

// TestRegistryAuthenticateAcceptsNoTokenAsAnOrdinaryMiss pins the behavior half
// of the no-early-exit rule. The empty string and a malformed string are
// ordinary misses, indistinguishable from a well-formed token that simply is
// not registered.
func TestRegistryAuthenticateAcceptsNoTokenAsAnOrdinaryMiss(t *testing.T) {
	reg, _, _, _ := testRegistry(t)
	live, _ := pairAndConfirm(t, reg, "keeper")
	// A pending device carries no fingerprint, so it is the record the loop
	// used to `continue` past.
	if _, err := reg.Pair("pending", PlatformIOS, "http://127.0.0.1:7777"); err != nil {
		t.Fatal(err)
	}

	for _, token := range []string{"", "x", strings.Repeat("A", 512), "sha256:"} {
		_, err := reg.Authenticate(token)
		var authErr *AuthError
		if !errors.As(err, &authErr) {
			t.Fatalf("Authenticate(%q) returned %v, want an *AuthError", token, err)
		}
		if authErr.Reason != ReasonUnknownDevice {
			t.Fatalf("Authenticate(%q) reason = %q, want %q", token, authErr.Reason, ReasonUnknownDevice)
		}
	}

	// The pending device next to it must not have been made matchable by the
	// fixed-width substitution.
	if _, err := reg.Authenticate(live); err != nil {
		t.Fatalf("the real token stopped working: %v", err)
	}
}

// TestRegistryConcurrentHostFingerprintAgrees is the Windows regression: the
// host seed used to be created outside the lock, so N concurrent first pairings
// each wrote a temp file and renamed it onto host.key. POSIX tolerates a rename
// onto an existing path; Windows refuses one onto a path that exists or that
// another process holds open, and CI failed with "Access is denied".
//
// The identity must also AGREE across the racers. A registry that let two of
// them mint different seeds would hand two phones different host fingerprints
// for the same Mac, which is precisely the substitution the fingerprint exists
// to make visible.
func TestRegistryConcurrentHostFingerprintAgrees(t *testing.T) {
	reg, _, configDir, _ := testRegistry(t)
	const n = 8

	var wg sync.WaitGroup
	got := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = reg.HostFingerprint()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("HostFingerprint %d: %v", i, err)
		}
		if got[i] != got[0] {
			t.Fatalf("racer %d minted %s, racer 0 minted %s", i, got[i], got[0])
		}
	}
	if err := validateFingerprint("host_fingerprint", got[0]); err != nil {
		t.Fatal(err)
	}

	// Exactly one seed file, and no temporary left behind.
	entries, err := os.ReadDir(filepath.Join(configDir, "companion"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("a temporary file survived: %s", e.Name())
		}
	}
}

// TestRegistryRefusesACorruptHostSeed pins the choice not to self-heal. Minting
// a fresh seed over a truncated one would rotate the host identity silently, and
// every already-paired phone would read that as a host substitution.
func TestRegistryRefusesACorruptHostSeed(t *testing.T) {
	reg, _, configDir, _ := testRegistry(t)
	first, err := reg.HostFingerprint()
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(configDir, "companion", "host.key")
	if err := os.WriteFile(path, []byte("short"), secretFileMode); err != nil {
		t.Fatal(err)
	}
	_, err = reg.HostFingerprint()
	if err == nil {
		t.Fatal("a truncated host seed was accepted")
	}
	if !strings.Contains(err.Error(), "host identity is corrupt") {
		t.Fatalf("error %q does not say what went wrong", err)
	}
	if _, err := reg.Pair("phone", PlatformIOS, "http://127.0.0.1:7777"); err == nil {
		t.Fatal("pairing succeeded against a corrupt host identity")
	}

	body := make([]byte, tokenEntropyBytes)
	if err := os.WriteFile(path, body, secretFileMode); err != nil {
		t.Fatal(err)
	}
	restored, err := reg.HostFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if restored == first {
		t.Fatal("the fingerprint did not follow the seed")
	}
}
