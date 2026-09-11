package mora

import (
	"context"
	"flag"
	"io"
	"os"
	"strings"
	"time"

	doctorpkg "github.com/pyranthus-hq/mora/internal/doctor"
	"github.com/pyranthus-hq/mora/internal/imessage"
)

const schemaDoctorCheck = "mora.doctor.check"

const doctorCheckUsage = "usage: mora doctor check imessage-access --json"

// doctorCheckReport is one named probe, answered for a machine. It carries the
// observation and the reason, never advice; the app draws the guidance.
type doctorCheckReport struct {
	Check             string `json:"check"`
	OK                bool   `json:"ok"`
	PlatformSupported bool   `json:"platform_supported"`
	ChatDBPresent     bool   `json:"chat_db_present"`
	Readable          bool   `json:"readable"`
	Reason            string `json:"reason"`
	Detail            string `json:"detail,omitempty"`
	ObservedAt        string `json:"observed_at"`
}

func imessageAccessCheck(seams doctorpkg.IMessageSeams, now time.Time) doctorCheckReport {
	rep := doctorCheckReport{Check: "imessage-access", ObservedAt: now.UTC().Format(time.RFC3339)}
	if seams.GOOS() != "darwin" {
		rep.Reason = "unsupported_platform"
		return rep
	}
	rep.PlatformSupported = true
	path := seams.ChatDBPath()
	if _, err := seams.Stat(path); err != nil {
		rep.Reason = "chat_db_missing"
		return rep
	}
	rep.ChatDBPresent = true
	readable, err := seams.ProbeReadable(path)
	if err != nil {
		rep.Detail = err.Error()
	}
	if !readable {
		rep.Reason = "not_readable"
		return rep
	}
	rep.Readable = true
	rep.OK = true
	return rep
}

func imessageAccessSeams(cfg Config) doctorpkg.IMessageSeams {
	return doctorpkg.IMessageSeams{
		GOOS:          runtimeGOOS,
		ChatDBPath:    func() string { return chatDBPath(cfg) },
		Stat:          os.Stat,
		ProbeReadable: imessage.ProbeReadable,
	}
}

func cmdDoctorCheck(ctx context.Context, args []string, stdout io.Writer) error {
	// Go's flag package stops at the first positional argument, so the check
	// name is peeled off first; both supported flag positions parse.
	name := ""
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		name, rest = rest[0], rest[1:]
	}
	fs := flag.NewFlagSet("doctor check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(rest); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil, "%s", doctorCheckUsage)
	}
	if name == "" {
		return newCodedError(errCodeUsageUnknownValue, nil, "%s", doctorCheckUsage)
	}
	if !*jsonOut {
		return newCodedError(errCodeUsageUnknownValue, nil, "%s (the human form is `mora doctor`)", doctorCheckUsage)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	switch name {
	case "imessage-access":
		return emitReceipt(stdout, schemaDoctorCheck, 1, imessageAccessCheck(imessageAccessSeams(cfg), time.Now()))
	default:
		return newCodedError(errCodeUsageUnknownValue, nil, "%s (unknown check %q)", doctorCheckUsage, name)
	}
}
