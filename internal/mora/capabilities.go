package mora

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
)

const (
	featureSupported   = "supported"
	featureUnsupported = "unsupported"
	featurePlanned     = "planned"
)

// Repair and deep links become supported in Phases 3 and 5 respectively. Keep
// the tri-state fields stable so those phases change values, not this schema.
type capabilitiesFeatures struct {
	Repair   string `json:"repair"`
	DeepLink string `json:"deep_link"`
}

type capabilitiesCommand struct {
	Path         string `json:"path"`
	JSONContract string `json:"json_contract"`
	Payload      string `json:"payload"`
}

type capabilitiesConnector struct {
	Type      string               `json:"type"`
	Name      string               `json:"name"`
	NeedsAuth bool                 `json:"needs_auth"`
	Ingesting bool                 `json:"ingesting"`
	Features  capabilitiesFeatures `json:"features"`
}

type capabilitiesSchema struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

type capabilitiesMCP struct {
	WritePolicy string   `json:"write_policy"`
	Tools       []string `json:"tools"`
}

type capabilitiesPayload struct {
	MoraVersion string                  `json:"mora_version"`
	Commands    []capabilitiesCommand   `json:"commands"`
	Connectors  []capabilitiesConnector `json:"connectors"`
	Schemas     []capabilitiesSchema    `json:"schemas"`
	MCP         capabilitiesMCP         `json:"mcp"`
	Features    capabilitiesFeatures    `json:"features"`
}

type capabilitiesRegistry struct {
	Commands []capabilitiesCommand `json:"commands"`
}

//go:embed eval/cli-command-registry.json
var embeddedCLICommandRegistry []byte

func capabilitiesCommands() ([]capabilitiesCommand, error) {
	var registry capabilitiesRegistry
	if err := json.Unmarshal(embeddedCLICommandRegistry, &registry); err != nil {
		return nil, fmt.Errorf("parse embedded CLI command registry: %w", err)
	}
	commands := make([]capabilitiesCommand, 0, 1)
	for _, command := range registry.Commands {
		if command.Path == "lint" {
			commands = append(commands, command)
		}
	}
	return commands, nil
}

func capabilitiesConnectors() []capabilitiesConnector {
	connectors := make([]capabilitiesConnector, 0, len(connectorCatalog))
	for _, connector := range connectorCatalog {
		connectors = append(connectors, capabilitiesConnector{
			Type:      connector.Type,
			Name:      connector.DisplayName,
			NeedsAuth: connector.NeedsAuth,
			Ingesting: connector.Ingesting,
			Features: capabilitiesFeatures{
				Repair:   featureUnsupported,
				DeepLink: featureUnsupported,
			},
		})
	}
	// gdrive is deliberately absent because it is a no-op stub outside connectorCatalog.
	return connectors
}

func cmdCapabilities(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("capabilities", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newMoraError(errCodeUsageUnknownValue, "usage", nil, "unexpected argument %q", fs.Arg(0))
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	commands, err := capabilitiesCommands()
	if err != nil {
		return err
	}
	payload := capabilitiesPayload{
		MoraVersion: BuildVersion,
		Commands:    commands,
		Connectors:  capabilitiesConnectors(),
		Schemas: []capabilitiesSchema{
			{Name: "mora.lint.report", Version: 1},
			{Name: "mora.capabilities", Version: 1},
		},
		MCP: capabilitiesMCP{
			WritePolicy: cfg.mcpWritePolicy(),
			Tools:       mcpToolNames(),
		},
		Features: capabilitiesFeatures{Repair: featureUnsupported, DeepLink: featureUnsupported},
	}
	if *jsonOut {
		return emitReceipt(stdout, "mora.capabilities", 1, payload)
	}
	fmt.Fprintf(stdout, "Mora %s capabilities: %d connector(s), %d MCP tool(s).\n", payload.MoraVersion, len(payload.Connectors), len(payload.MCP.Tools))
	return nil
}
