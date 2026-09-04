package mora

// companion.go is the CLI surface of the device registry (graph node N11).
//
// The registry itself lives in internal/companion, which is a leaf package: it
// owns the record file, the secrets, the constant-time comparison and the
// receipts, and it knows nothing about flags, Config or stdout. This file is the
// thin half — it resolves the Config, renders, and translates registry errors
// into the CLI's coded-error vocabulary.
//
// The four verbs are the whole human loop around a paired phone:
//
//	mora companion pair     print a QR-able pairing payload with a one-time code
//	mora companion list     show every device and the credential it authenticates with
//	mora companion revoke   end one device's access
//	mora companion status   counts, host identity, and whether a window is open
//	mora companion serve    the narrow loopback listener a paired phone reads
//	mora companion expose   the exact commands that publish that listener to the tailnet
//
// `pair` prints a secret. It is the only command here that does, it says so in
// the human output, and the JSON form carries it because a QR renderer has to
// read it from somewhere. Nothing in this file logs a payload.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/companion"
)

// companionSchemaVersion is the MAJOR version of the four CLI payloads below.
// It is separate from companion.SchemaVersion, which versions the WIRE contract
// a phone decodes: the CLI receipt shapes and the device-facing schemas are
// free to move at different speeds, and pinning them together would force a
// phone release for a change to a terminal receipt.
const companionSchemaVersion = 1

const (
	schemaCompanionPair   = "mora.companion.pair"
	schemaCompanionList   = "mora.companion.list"
	schemaCompanionRevoke = "mora.companion.revoke"
	schemaCompanionStatus = "mora.companion.status"
	schemaCompanionExpose = "mora.companion.expose"
)

const companionUsage = "usage: mora companion <pair|list|revoke|status|serve|expose>"

func cmdCompanion(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return newCodedError(errCodeUsageMissingArgument, nil, "%s", companionUsage)
	}
	switch args[0] {
	case "pair":
		return cmdCompanionPair(ctx, args[1:], stdout, stderr)
	case "list":
		return cmdCompanionList(ctx, args[1:], stdout)
	case "revoke":
		return cmdCompanionRevoke(ctx, args[1:], stdout, stderr)
	case "status":
		return cmdCompanionStatus(ctx, args[1:], stdout)
	case "serve":
		return cmdCompanionServe(ctx, args[1:], stdout)
	case "expose":
		return cmdCompanionExpose(ctx, args[1:], stdout)
	default:
		return newCodedError(errCodeUsageUnknownValue, nil,
			"unknown companion subcommand %q — %s", args[0], companionUsage)
	}
}

// companionRegistry builds a registry over the resolved Config. The clock comes
// from the Config rather than time.Now so a test that pinned one sees pinned
// timestamps in receipts and pairing expiries, exactly as the operation-activity
// records do.
func companionRegistry(ctx context.Context) (*companion.Registry, Config, error) {
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return nil, Config{}, err
	}
	reg := companion.NewRegistry(cfg.ConfigDir, cfg.StateDir,
		companion.WithClock(func() time.Time { return cfg.OperationClock() }))
	return reg, cfg, nil
}

// ---------------------------------------------------------------------------
// pair
// ---------------------------------------------------------------------------

// companionPairPayload is the `pair` receipt.
//
// PairingCode is a live one-time secret. It is in the payload because the whole
// point of the command is to hand it to a QR renderer, and it is the reason the
// human rendering below carries a warning line rather than printing it bare.
type companionPairPayload struct {
	DeviceID        string `json:"device_id"`
	Label           string `json:"label"`
	Platform        string `json:"platform"`
	Endpoint        string `json:"endpoint"`
	PairingCode     string `json:"pairing_code"`
	ExpiresAt       string `json:"expires_at"`
	HostFingerprint string `json:"host_fingerprint"`
}

func cmdCompanionPair(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("companion pair", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	label := fs.String("label", "phone", "human label for the device")
	platform := fs.String("platform", string(companion.PlatformIOS), "device family: ios, macos or other")
	endpoint := fs.String("endpoint", "", "URL the phone reaches this Mac on (default the loopback listener)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"usage: mora companion pair [--label NAME] [--platform ios|macos|other] [--endpoint URL] (unexpected argument %q)", fs.Arg(0))
	}
	if *endpoint == "" {
		// The default is the COMPANION listener's port, not the generic
		// loopback API's: a phone that follows this endpoint must land on
		// `mora companion serve`, which is the only server that accepts the
		// token pairing is about to mint. Publishing a reachable-off-the-Mac
		// endpoint is N22's job, not this default's.
		*endpoint = fmt.Sprintf("http://%s:%d", companion.LoopbackHost, defaultCompanionPort)
	}

	reg, _, err := companionRegistry(ctx)
	if err != nil {
		return err
	}
	payload, err := reg.Pair(*label, companion.Platform(*platform), *endpoint)
	if err != nil && !warnUnaudited(stderr, "pair", err) {
		return companionError("pair", err)
	}

	out := companionPairPayload{
		DeviceID:        payload.DeviceID,
		Label:           *label,
		Platform:        *platform,
		Endpoint:        payload.Endpoint,
		PairingCode:     payload.PairingCode,
		ExpiresAt:       payload.ExpiresAt,
		HostFingerprint: payload.HostFingerprint,
	}
	if *jsonOut {
		return emitReceipt(stdout, schemaCompanionPair, companionSchemaVersion, out)
	}
	fmt.Fprintf(stdout, "device\t%s\n", out.DeviceID)
	fmt.Fprintf(stdout, "label\t%s\n", out.Label)
	fmt.Fprintf(stdout, "endpoint\t%s\n", out.Endpoint)
	fmt.Fprintf(stdout, "host\t%s\n", out.HostFingerprint)
	fmt.Fprintf(stdout, "expires\t%s\n", out.ExpiresAt)
	fmt.Fprintf(stdout, "code\t%s\n", out.PairingCode)
	fmt.Fprintln(stdout, "This code is a one-time secret. Anyone who reads it can pair a device until it expires.")
	return nil
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

// companionListPayload carries the array under a key rather than at the top
// level, the shape Plan 01-07 fixed for every list-shaped result.
type companionListPayload struct {
	Devices []companion.Device `json:"devices"`
}

func cmdCompanionList(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("companion list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"usage: mora companion list [--json] (unexpected argument %q)", fs.Arg(0))
	}

	reg, _, err := companionRegistry(ctx)
	if err != nil {
		return err
	}
	devices, err := reg.List()
	if err != nil {
		return companionError("list", err)
	}
	if *jsonOut {
		return emitReceipt(stdout, schemaCompanionList, companionSchemaVersion,
			companionListPayload{Devices: devices})
	}
	if len(devices) == 0 {
		fmt.Fprintln(stdout, "No paired devices. Run `mora companion pair` to start one.")
		return nil
	}
	for _, d := range devices {
		seen := d.LastSeenAt
		if seen == "" {
			seen = "never"
		}
		credential := d.TokenFingerprint
		if credential == "" {
			credential = "none"
		}
		// The fingerprint identifies the credential without carrying it, which
		// is what makes this line safe in a screenshot.
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\n",
			d.DeviceID, d.Label, d.Platform, d.State, seen, credential)
	}
	return nil
}

// ---------------------------------------------------------------------------
// revoke
// ---------------------------------------------------------------------------

// companionRevokePayload is the `revoke` receipt. Changed distinguishes "this
// call ended the device's access" from "it was already revoked", so a script can
// be idempotent without treating the repeat as a failure.
type companionRevokePayload struct {
	DeviceID  string `json:"device_id"`
	State     string `json:"state"`
	RevokedAt string `json:"revoked_at"`
	Changed   bool   `json:"changed"`
}

func cmdCompanionRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	// The positional is read before the flag set because the device id comes
	// first, and it is guarded because a mistyped flag landing in an id slot is
	// the bug class refuseDashLedPositional exists to close.
	if len(args) == 0 {
		return newCodedError(errCodeUsageMissingArgument, nil, "usage: mora companion revoke <device-id> [--json]")
	}
	if err := refuseDashLedPositional("mora companion revoke", "device id", args[0]); err != nil {
		return err
	}
	deviceID := args[0]

	fs := flag.NewFlagSet("companion revoke", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"usage: mora companion revoke <device-id> [--json] (unexpected argument %q)", fs.Arg(0))
	}

	reg, _, err := companionRegistry(ctx)
	if err != nil {
		return err
	}
	// Revocation takes no confirmation flag on purpose. It is the SAFE
	// direction — it removes access, it never destroys a memory — and the moment
	// an operator wants it is the moment friction is most expensive.
	dev, changed, err := reg.Revoke(deviceID)
	if err != nil && !warnUnaudited(stderr, "revoke", err) {
		return companionError("revoke", err)
	}
	out := companionRevokePayload{
		DeviceID:  dev.DeviceID,
		State:     string(dev.State),
		RevokedAt: dev.RevokedAt,
		Changed:   changed,
	}
	if *jsonOut {
		return emitReceipt(stdout, schemaCompanionRevoke, companionSchemaVersion, out)
	}
	if changed {
		// A device revoked before it was ever confirmed has no token to
		// invalidate; saying otherwise would describe an event that did not
		// happen.
		if dev.TokenFingerprint == "" {
			fmt.Fprintf(stdout, "Revoked %s (%s). Its pairing code is dead and it was never issued a token.\n", dev.DeviceID, dev.Label)
			return nil
		}
		fmt.Fprintf(stdout, "Revoked %s (%s). Its token no longer authenticates.\n", dev.DeviceID, dev.Label)
		return nil
	}
	fmt.Fprintf(stdout, "%s was already revoked at %s. Nothing changed.\n", dev.DeviceID, dev.RevokedAt)
	return nil
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func cmdCompanionStatus(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("companion status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"usage: mora companion status [--json] (unexpected argument %q)", fs.Arg(0))
	}

	reg, _, err := companionRegistry(ctx)
	if err != nil {
		return err
	}
	status, err := reg.Status()
	if err != nil {
		return companionError("status", err)
	}
	if *jsonOut {
		return emitReceipt(stdout, schemaCompanionStatus, companionSchemaVersion, status)
	}
	fmt.Fprintf(stdout, "active\t%d\n", status.Active)
	fmt.Fprintf(stdout, "pending\t%d\n", status.Pending)
	fmt.Fprintf(stdout, "revoked\t%d\n", status.Revoked)
	if status.PairingOpen {
		fmt.Fprintf(stdout, "pairing\topen until %s\n", status.NextPairingExpiry)
		return nil
	}
	fmt.Fprintln(stdout, "pairing\tclosed")
	return nil
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// warnUnaudited handles the one registry error that is not a failure.
//
// ErrReceiptNotWritten means the change COMMITTED and only its audit row did
// not. The device is paired, or revoked, and the pairing code this command just
// printed is the only copy there will ever be — so the command prints its normal
// output and exits 0, with the warning on stderr where it cannot corrupt a
// `--json` document being piped into something. Exiting non-zero here would tell
// a script the pairing failed when it did not, and a retry would mint a second
// device.
//
// It reports whether it handled the error.
func warnUnaudited(stderr io.Writer, verb string, err error) bool {
	if !errors.Is(err, companion.ErrReceiptNotWritten) {
		return false
	}
	fmt.Fprintf(stderr, "warning: the %s took effect but its audit receipt could not be written (%v); "+
		"the change is real, the local audit trail is incomplete\n", verb, err)
	return true
}

// companionError translates a registry error into the CLI's coded vocabulary.
//
// The unknown-device case is a usage error rather than an internal one: the
// operator mistyped an id, and the actionable next step is a command, so the
// message names it.
func companionError(verb string, err error) error {
	switch {
	case errors.Is(err, companion.ErrNoSuchDevice):
		return newCodedError(errCodeUsageUnknownValue, err,
			"no such device — run `mora companion list` to see the paired devices")
	case errors.Is(err, companion.ErrNotPending):
		return newCodedError(errCodeUsageUnknownValue, err,
			"that device is not awaiting pairing — run `mora companion pair` for a new code")
	case errors.Is(err, companion.ErrLocked):
		return newCodedError(errCodeUsageUnknownValue, err,
			"another mora process is writing the device registry — retry in a moment")
	}
	var schemaErr *companion.Error
	if errors.As(err, &schemaErr) {
		return newCodedError(errCodeUsageUnknownValue, err, "mora companion %s: %v", verb, err)
	}
	return fmt.Errorf("mora companion %s: %w", verb, err)
}

// ---------------------------------------------------------------------------
// expose
// ---------------------------------------------------------------------------

// `mora companion expose` is graph node N22: it publishes nothing, and it says
// exactly how to publish.
//
// # Why it prints commands instead of running them
//
// Tailscale Serve is the user's network, not Mora's. A command that shells out
// to `tailscale` would take a durable action on a tailnet Mora does not own,
// under a daemon Mora cannot see the policy of, and would need to be trusted to
// take that action back. Printing leaves both the decision and the undo with the
// operator, and it keeps this subcommand runnable on a machine with no Tailscale
// installed at all. Nothing here execs anything, and nothing here reads the
// tailnet.
//
// # The Host problem, which is the whole reason this node exists
//
// Serve terminates TLS in tailscaled and reverse-proxies to the loopback
// backend, forwarding the client's Host header VERBATIM. Measured against
// tailscale 1.102.3 on macOS: a request to the published node name arrives at
// the backend with Host set to the node name (and X-Forwarded-Host identical),
// while RemoteAddr is 127.0.0.1. The N12 listener's DNS-rebinding guard requires
// the literal loopback address in Host, so before this node a paired phone
// behind Serve got 403 forbidden_host on every route.
//
// That is why the published `serve` line carries `--allow-host`, and why this
// command computes the value rather than leaving the operator to guess it: the
// exact string depends on the published port, because Serve omits the port from
// Host only when it is the scheme's default.
//
// # What it refuses
//
// No active device: publishing a listener that nothing can authenticate to puts
// a port on the tailnet for no one, and the operator's next step is `pair`, not
// `tailscale`. An unusable listener port: a port of zero is not a configuration,
// and printing a command containing ":0" would be printing a broken command.
//
// # What it never prints
//
// A token, a pairing code, or any device secret. It reads only the registry's
// COUNTS. It also never prints a Funnel command: Funnel publishes to the public
// internet, and nothing in this contract is meant to leave the tailnet.

const (
	// defaultTailnetHTTPSPort is Serve's HTTPS port. Serve omits a default port
	// from the Host header it forwards, which is why the allow-host value below
	// is computed rather than always "name:port".
	defaultTailnetHTTPSPort = 443
	// defaultTailnetHTTPPort is the plaintext equivalent.
	defaultTailnetHTTPPort = 80
	// hostnamePlaceholder stands in when the operator has not supplied the node
	// name. It is deliberately not a valid hostname: a command that still holds
	// it cannot be pasted by accident, and no real tailnet name is ever baked
	// into this binary, its tests or its goldens.
	hostnamePlaceholder = "<your-node>.<your-tailnet>.ts.net"
)

// companionExposePayload is the `expose` receipt.
//
// The commands are argv slices rather than one string because a shell-quoted
// line is a rendering, and a caller that wants to run one should not have to
// unparse it. The human branch joins them.
type companionExposePayload struct {
	ListenerPort  int      `json:"listener_port"`
	Backend       string   `json:"backend"`
	Hostname      string   `json:"hostname"`
	HostnameKnown bool     `json:"hostname_known"`
	Scheme        string   `json:"scheme"`
	TailnetPort   int      `json:"tailnet_port"`
	PublicURL     string   `json:"public_url"`
	AllowHost     string   `json:"allow_host"`
	ActiveDevices int      `json:"active_devices"`
	Funnel        string   `json:"funnel"`
	ListenCommand []string `json:"listen_command"`
	ServeCommand  []string `json:"serve_command"`
	OffCommand    []string `json:"off_command"`
	ResetCommand  []string `json:"reset_command"`
}

func cmdCompanionExpose(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("companion expose", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", defaultCompanionPort, "the loopback port `mora companion serve` listens on")
	hostname := fs.String("hostname", "", "this Mac's tailnet name (`tailscale status --json`, Self.DNSName without the trailing dot)")
	tailnetPort := fs.Int("tailnet-port", 0, "the port to publish on (default 443 for HTTPS, 80 with --plaintext)")
	plaintext := fs.Bool("plaintext", false, "publish over plain HTTP instead of HTTPS, for a tailnet with HTTPS certificates disabled")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"usage: mora companion expose [--port N] [--hostname NAME] [--tailnet-port N] [--plaintext] [--json] (unexpected argument %q)", fs.Arg(0))
	}

	// The listener port is checked before the registry is opened: a command
	// that cannot produce a usable line should not touch the device records at
	// all, and "not configured" is a different sentence from "out of range".
	if *port == 0 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"the companion listener port is not configured — pass --port, or run `mora companion serve` on its default port %d",
			defaultCompanionPort)
	}
	if *port < 0 || *port > 65535 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"invalid --port %d: must be between 1 and 65535", *port)
	}

	scheme := "https"
	publishPort := defaultTailnetHTTPSPort
	if *plaintext {
		scheme = "http"
		publishPort = defaultTailnetHTTPPort
	}
	if *tailnetPort != 0 {
		publishPort = *tailnetPort
	}
	if publishPort < 1 || publishPort > 65535 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"invalid --tailnet-port %d: must be between 1 and 65535", *tailnetPort)
	}

	node := *hostname
	known := node != ""
	if !known {
		node = hostnamePlaceholder
	} else if _, err := companion.CheckAllowHost(node); err != nil {
		// Validated against the LISTENER's rule, not a second one, so this
		// command cannot print an --allow-host argument its own listener would
		// refuse at startup.
		return newCodedError(errCodeUsageUnknownValue, err,
			"invalid --hostname %q: %v", *hostname, err)
	}

	reg, _, err := companionRegistry(ctx)
	if err != nil {
		return err
	}
	status, err := reg.Status()
	if err != nil {
		return companionError("expose", err)
	}
	if status.Active == 0 {
		// Counts only. This branch never reads a token or a code, and the
		// message names the command that fixes it.
		return newCodedError(errCodeDataNotFound, nil,
			"no active paired device (%d pending, %d revoked) — run `mora companion pair` and finish pairing first; "+
				"publishing a listener that nothing can authenticate to puts a port on your tailnet for no one",
			status.Pending, status.Revoked)
	}

	// Serve omits the port from the URL, and therefore from the Host header it
	// forwards, exactly when the port is the scheme's default. The allow-host
	// value has to match what actually arrives, so it is derived from the same
	// condition rather than written twice.
	portIsDefault := (scheme == "https" && publishPort == defaultTailnetHTTPSPort) ||
		(scheme == "http" && publishPort == defaultTailnetHTTPPort)
	authority := node
	if !portIsDefault {
		authority = fmt.Sprintf("%s:%d", node, publishPort)
	}
	backend := fmt.Sprintf("http://%s:%d", companion.LoopbackHost, *port)
	portFlag := fmt.Sprintf("--%s=%d", scheme, publishPort)

	out := companionExposePayload{
		ListenerPort:  *port,
		Backend:       backend,
		Hostname:      node,
		HostnameKnown: known,
		Scheme:        scheme,
		TailnetPort:   publishPort,
		PublicURL:     fmt.Sprintf("%s://%s/", scheme, authority),
		AllowHost:     authority,
		ActiveDevices: status.Active,
		// Stated, not omitted. A reader looking for the Funnel command should
		// find the answer rather than assume the command was forgotten.
		Funnel:        "off",
		ListenCommand: []string{"mora", "companion", "serve", "--port", fmt.Sprint(*port), "--allow-host", authority},
		ServeCommand:  []string{"tailscale", "serve", "--bg", portFlag, backend},
		OffCommand:    []string{"tailscale", "serve", portFlag, "off"},
		ResetCommand:  []string{"tailscale", "serve", "reset"},
	}
	if *jsonOut {
		return emitReceipt(stdout, schemaCompanionExpose, companionSchemaVersion, out)
	}

	fmt.Fprintf(stdout, "listener\t%s\n", out.Backend)
	fmt.Fprintf(stdout, "public\t%s\n", out.PublicURL)
	fmt.Fprintf(stdout, "allow-host\t%s\n", out.AllowHost)
	fmt.Fprintf(stdout, "devices\t%d active\n", out.ActiveDevices)
	fmt.Fprintf(stdout, "funnel\t%s (this command never publishes to the public internet)\n", out.Funnel)
	fmt.Fprintln(stdout)
	if !known {
		fmt.Fprintf(stdout, "Replace %s with this Mac's tailnet name before running anything:\n", hostnamePlaceholder)
		fmt.Fprintln(stdout, "  tailscale status --json    (Self.DNSName, without the trailing dot)")
		fmt.Fprintln(stdout, "Or re-run with --hostname NAME and copy the lines below verbatim.")
		fmt.Fprintln(stdout)
	}
	fmt.Fprintln(stdout, "Start the listener, then publish it. In this order:")
	fmt.Fprintf(stdout, "  %s\n", shellLine(out.ListenCommand))
	fmt.Fprintf(stdout, "  %s\n", shellLine(out.ServeCommand))
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Stop publishing:")
	fmt.Fprintf(stdout, "  %s\n", shellLine(out.OffCommand))
	fmt.Fprintf(stdout, "  %s   (removes every mapping, not just this one)\n", shellLine(out.ResetCommand))
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "--allow-host is what makes this work: Serve forwards the client's Host header unchanged,")
	fmt.Fprintln(stdout, "and the listener refuses any Host but 127.0.0.1 unless you name the published one exactly.")
	fmt.Fprintln(stdout, "The listener still binds 127.0.0.1 only, and still accepts a proxied request only from a")
	fmt.Fprintln(stdout, "loopback peer. Verify the whole boundary with scripts/companion-network-audit.sh.")
	return nil
}

// shellLine renders an argv for a human to paste. It quotes only what a shell
// would otherwise split, so the common case reads as the command it is.
func shellLine(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\"'\\$`") {
			parts = append(parts, strconv.Quote(a))
			continue
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}
