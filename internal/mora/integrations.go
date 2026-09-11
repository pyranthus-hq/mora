package mora

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	hookspkg "github.com/pyranthus-hq/mora/internal/hooks"
	"github.com/pyranthus-hq/mora/internal/integrations"
)

const (
	schemaIntegrationsList       = "mora.integrations.list"
	schemaIntegrationsConnect    = "mora.integrations.connect"
	schemaIntegrationsDisconnect = "mora.integrations.disconnect"
	integrationsUsage            = "usage: mora integrations list [--binary <abs>] --json | connect --client <claude|codex|cursor|claude-desktop> --binary <abs> [--json] | disconnect --client <name> [--json]"
)

type integrationsListPayload struct {
	Binary  string                `json:"binary"`
	Clients []integrations.Status `json:"clients"`
}

func integrationSeams(cfg Config) integrations.Seams {
	return integrations.DefaultSeams(cfg.HomeDir())
}

func cmdIntegrations(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) < 1 {
		return newCodedError(errCodeUsageUnknownValue, nil, "%s", integrationsUsage)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	seams := integrationSeams(cfg)
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("integrations list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		binary := fs.String("binary", "", "binary to compare registrations against (default: this executable)")
		jsonOut := fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
		}
		if *binary != "" && !filepath.IsAbs(*binary) {
			return newCodedError(errCodeUsageUnknownValue, nil, "--binary must be an absolute path")
		}
		if *binary == "" {
			exe, err := os.Executable()
			if err != nil {
				return newCodedError(errCodeInternalUnexpected, err, "resolve executable: %v", err)
			}
			*binary = exe
		}
		if fs.NArg() != 0 {
			return newCodedError(errCodeUsageUnknownValue, nil, "%s", integrationsUsage)
		}
		list, err := integrations.List(seams, *binary)
		if err != nil {
			return newCodedError(errCodeInternalUnexpected, err, "integrations list: %v", err)
		}
		if *jsonOut {
			return emitReceipt(stdout, schemaIntegrationsList, 1, integrationsListPayload{Binary: *binary, Clients: list})
		}
		var summaries []string
		for _, row := range list {
			state := "not detected"
			switch {
			case row.Registered && row.MatchesBinary:
				state = "connected"
			case row.Registered:
				state = "registered to another binary"
			case row.Detected:
				state = "detected, not connected"
			}
			summaries = append(summaries, row.Client+": "+state)
		}
		fmt.Fprintln(stdout, strings.Join(summaries, "; "))
		return nil
	case "connect", "disconnect":
		fs := flag.NewFlagSet("integrations "+args[0], flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		clientName := fs.String("client", "", "claude, codex, cursor, or claude-desktop")
		binary := fs.String("binary", "", "absolute path to the mora binary the client should run")
		jsonOut := fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
		}
		if fs.NArg() != 0 || (*binary != "" && !filepath.IsAbs(*binary)) {
			return newCodedError(errCodeUsageUnknownValue, nil, "%s", integrationsUsage)
		}
		client, ok := integrations.ParseClient(*clientName)
		if !ok {
			return newCodedError(errCodeUsageUnknownValue, nil, "%s (unknown client %q)", integrationsUsage, *clientName)
		}
		if args[0] == "connect" {
			if !filepath.IsAbs(*binary) {
				return newCodedError(errCodeUsageUnknownValue, nil, "--binary must be an absolute path, got %q", *binary)
			}
			receipt, err := integrations.Connect(seams, client, *binary)
			if err != nil {
				return newCodedError(errCodeInternalUnexpected, err, "integrations connect %s: %v", client, err)
			}
			if client == integrations.Claude {
				settings := filepath.Join(cfg.HomeDir(), ".claude", "settings.json")
				if err := hookspkg.Install(settings, *binary, hookRecallDefaultThreshold); err != nil {
					return newCodedError(errCodeInternalUnexpected, err, "install Claude hooks: %v", err)
				}
				receipt.Hook = "installed"
			}
			if *jsonOut {
				return emitReceipt(stdout, schemaIntegrationsConnect, 1, receipt)
			}
			fmt.Fprintf(stdout, "registered mora with %s at %s\n", client, receipt.ConfigPath)
			return nil
		}
		receipt, err := integrations.Disconnect(seams, client)
		if err != nil {
			return newCodedError(errCodeInternalUnexpected, err, "integrations disconnect %s: %v", client, err)
		}
		if client == integrations.Claude {
			settings := filepath.Join(cfg.HomeDir(), ".claude", "settings.json")
			if _, statErr := os.Stat(settings); statErr == nil {
				if err := hookspkg.Uninstall(settings); err != nil {
					return newCodedError(errCodeInternalUnexpected, err, "remove Claude hooks: %v", err)
				}
			} else if !os.IsNotExist(statErr) {
				return newCodedError(errCodeInternalUnexpected, statErr, "read Claude hooks: %v", statErr)
			}
			receipt.Hook = "not_installed"
		}
		if *jsonOut {
			return emitReceipt(stdout, schemaIntegrationsDisconnect, 1, receipt)
		}
		fmt.Fprintf(stdout, "removed mora from %s at %s\n", client, receipt.ConfigPath)
		return nil
	default:
		return newCodedError(errCodeUsageUnknownValue, nil, "%s", integrationsUsage)
	}
}
