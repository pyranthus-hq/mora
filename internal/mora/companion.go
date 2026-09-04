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
)

const companionUsage = "usage: mora companion <pair|list|revoke|status>"

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
		*endpoint = fmt.Sprintf("http://127.0.0.1:%d", envPortOr(defaultHTTPPort))
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
