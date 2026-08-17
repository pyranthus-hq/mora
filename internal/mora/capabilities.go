package mora

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
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

// capabilitiesMCP publishes the MCP surface. Plan 01-07 Task 1 chose option-a:
// MCP tool RESULTS carry no schema/schema_version, because the envelope costs a
// measured +30 tokens per object payload and write_memory's worst-case row has
// exactly 30 tokens of headroom against its T0 ceiling. `schemas` here is
// therefore the ONLY place an MCP consumer learns a tool payload's version.
type capabilitiesMCP struct {
	WritePolicy string               `json:"write_policy"`
	Tools       []string             `json:"tools"`
	Schemas     []capabilitiesSchema `json:"schemas"`
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

// capabilitiesSchemas derives the published CLI schema list from the embedded
// command registry, so a schema can never appear in the registry and be missing
// here. Exempt rows carry no payload and contribute nothing. moraExtraSchemas
// covers the documents a command emits that the registry does not name as a
// path of its own.
var moraExtraSchemas = []string{
	// `doctor --pulse` is a FLAG on the doctor path, not a registry row, but it
	// emits its own document with its own shape.
	"mora.doctor.pulse",
}

func capabilitiesSchemas() ([]capabilitiesSchema, error) {
	var registry capabilitiesRegistry
	if err := json.Unmarshal(embeddedCLICommandRegistry, &registry); err != nil {
		return nil, fmt.Errorf("parse embedded CLI command registry: %w", err)
	}
	seen := map[string]bool{}
	for _, command := range registry.Commands {
		if command.JSONContract == "exempt" || command.Payload == "" {
			continue
		}
		seen[command.Payload] = true
	}
	for _, name := range moraExtraSchemas {
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	schemas := make([]capabilitiesSchema, 0, len(names))
	for _, name := range names {
		schemas = append(schemas, capabilitiesSchema{Name: name, Version: 1})
	}
	return schemas, nil
}

// capabilitiesMCPSchemas names one payload schema per MCP tool. The names are
// published here and NOT inside the tool results themselves (Task 1, option-a).
func capabilitiesMCPSchemas() []capabilitiesSchema {
	tools := mcpToolNames()
	schemas := make([]capabilitiesSchema, 0, len(tools))
	for _, tool := range tools {
		schemas = append(schemas, capabilitiesSchema{Name: "mora.mcp." + tool, Version: 1})
	}
	sort.Slice(schemas, func(i, j int) bool { return schemas[i].Name < schemas[j].Name })
	return schemas
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
	schemas, err := capabilitiesSchemas()
	if err != nil {
		return err
	}
	payload := capabilitiesPayload{
		MoraVersion: BuildVersion,
		Commands:    commands,
		Connectors:  capabilitiesConnectors(),
		Schemas:     schemas,
		MCP: capabilitiesMCP{
			WritePolicy: cfg.mcpWritePolicy(),
			Tools:       mcpToolNames(),
			Schemas:     capabilitiesMCPSchemas(),
		},
		Features: capabilitiesFeatures{Repair: featureUnsupported, DeepLink: featureUnsupported},
	}
	if *jsonOut {
		return emitReceipt(stdout, "mora.capabilities", 1, payload)
	}
	fmt.Fprintf(stdout, "Mora %s capabilities: %d connector(s), %d MCP tool(s).\n", payload.MoraVersion, len(payload.Connectors), len(payload.MCP.Tools))
	return nil
}
