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

// capabilitiesConnectorFeatures is the per-connector tri-state block. It is a
// separate type from capabilitiesFeatures because `incremental_sync` is a
// per-connector property with no top-level analogue; sharing one struct and
// hiding the field behind `omitempty` would make the top-level document's shape
// depend on a value rather than on the schema.
type capabilitiesConnectorFeatures struct {
	Repair          string `json:"repair"`
	DeepLink        string `json:"deep_link"`
	IncrementalSync string `json:"incremental_sync"`
}

// capabilitiesCommand mirrors one row of eval/cli-command-registry.json. The
// JSON tags are the registry's own key names, so the section is decoded from the
// embedded registry rather than transformed into a second vocabulary that could
// drift from the first.
//
// `reason` is emitted ALWAYS, empty on non-exempt rows, rather than with
// `omitempty`. The compatibility gate walks arrays index-wise, so a document
// whose elements carry different key sets can report a false removal when the
// array reorders. A uniform key set makes that impossible.
type capabilitiesCommand struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	Platform     string `json:"platform"`
	JSONContract string `json:"json_contract"`
	Payload      string `json:"payload"`
	Reason       string `json:"reason"`
}

type capabilitiesConnector struct {
	Type      string                        `json:"type"`
	Name      string                        `json:"name"`
	NeedsAuth bool                          `json:"needs_auth"`
	Ingesting bool                          `json:"ingesting"`
	Label     string                        `json:"label"`
	Upcoming  bool                          `json:"upcoming"`
	Features  capabilitiesConnectorFeatures `json:"features"`
}

// capabilitiesErrorCode mirrors one row of eval/error-code-registry.json. Like
// capabilitiesCommand, every field is emitted always — `error_class` is empty on
// the codes outside the connector class rather than absent.
type capabilitiesErrorCode struct {
	Code       string `json:"code"`
	Class      string `json:"class"`
	ErrorClass string `json:"error_class"`
	ExitCode   int    `json:"exit_code"`
	Retryable  bool   `json:"retryable"`
	Meaning    string `json:"meaning"`
}

// capabilitiesExitCode publishes one allocated process exit code. `source` and
// `witness` from the registry row are deliberately NOT republished: they name
// Go files and test functions, which are not part of a consumer contract.
type capabilitiesExitCode struct {
	Code    int    `json:"code"`
	Status  string `json:"status"`
	Meaning string `json:"meaning"`
}

type capabilitiesReservedExitCodes struct {
	From   int    `json:"from"`
	To     int    `json:"to"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// capabilitiesExitCodes publishes the exit-code reality, including the fact that
// 3 through 9 are permanently reserved-unused. A caller that only saw the
// allocated list would reasonably assume 3 is the next code Mora will mint.
type capabilitiesExitCodes struct {
	Allocated        []capabilitiesExitCode        `json:"allocated"`
	Reserved         capabilitiesReservedExitCodes `json:"reserved"`
	FirstAllocatable int                           `json:"first_allocatable"`
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
	ErrorCodes  []capabilitiesErrorCode `json:"error_codes"`
	ExitCodes   capabilitiesExitCodes   `json:"exit_codes"`
	MCP         capabilitiesMCP         `json:"mcp"`
	Features    capabilitiesFeatures    `json:"features"`
}

type capabilitiesRegistry struct {
	Commands []capabilitiesCommand `json:"commands"`
}

// capabilitiesErrorRegistry mirrors eval/error-code-registry.json. The exit-code
// table lives in that file rather than in a second place, so `exit_codes` here
// is derived, not hand-written.
type capabilitiesErrorRegistry struct {
	Codes             []capabilitiesErrorCode       `json:"codes"`
	ExitCodes         []capabilitiesExitCode        `json:"exit_codes"`
	ReservedExitCodes capabilitiesReservedExitCodes `json:"reserved_exit_codes"`
	FirstAllocatable  int                           `json:"first_allocatable_exit_code"`
}

// The registries are the source of truth for what Mora can do, and a shipped
// binary cannot assume they are on disk beside it. Embedding them is what makes
// `capabilities` unable to disagree with them: there is one copy of the data,
// compiled in, and no generated Go table to fall out of date. The cost is that
// the drift test for these two sections is tautological by construction — the
// non-tautological binding is to the connector catalog, the MCP tool list, and
// the live write policy, which come from Go source rather than from these files.
//
//go:embed eval/cli-command-registry.json
var embeddedCLICommandRegistry []byte

//go:embed eval/error-code-registry.json
var embeddedErrorCodeRegistry []byte

func capabilitiesCommands() ([]capabilitiesCommand, error) {
	var registry capabilitiesRegistry
	if err := json.Unmarshal(embeddedCLICommandRegistry, &registry); err != nil {
		return nil, fmt.Errorf("parse embedded CLI command registry: %w", err)
	}
	commands := make([]capabilitiesCommand, 0, len(registry.Commands))
	commands = append(commands, registry.Commands...)
	sort.Slice(commands, func(i, j int) bool { return commands[i].Path < commands[j].Path })
	return commands, nil
}

func capabilitiesErrorRegistryDecoded() (capabilitiesErrorRegistry, error) {
	var registry capabilitiesErrorRegistry
	if err := json.Unmarshal(embeddedErrorCodeRegistry, &registry); err != nil {
		return registry, fmt.Errorf("parse embedded error code registry: %w", err)
	}
	return registry, nil
}

func capabilitiesErrorCodes(registry capabilitiesErrorRegistry) []capabilitiesErrorCode {
	codes := make([]capabilitiesErrorCode, 0, len(registry.Codes))
	codes = append(codes, registry.Codes...)
	sort.Slice(codes, func(i, j int) bool { return codes[i].Code < codes[j].Code })
	return codes
}

func capabilitiesExitCodeTable(registry capabilitiesErrorRegistry) capabilitiesExitCodes {
	allocated := make([]capabilitiesExitCode, 0, len(registry.ExitCodes))
	for _, row := range registry.ExitCodes {
		allocated = append(allocated, capabilitiesExitCode{Code: row.Code, Status: row.Status, Meaning: row.Meaning})
	}
	sort.Slice(allocated, func(i, j int) bool { return allocated[i].Code < allocated[j].Code })
	return capabilitiesExitCodes{
		Allocated:        allocated,
		Reserved:         registry.ReservedExitCodes,
		FirstAllocatable: registry.FirstAllocatable,
	}
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

// Gmail History IDs and Calendar sync tokens are provider-native between-run
// cursors. Other connectors still re-read their configured window/tree and
// therefore remain explicitly unsupported rather than overstating capability.
func capabilitiesIncrementalSync(connector connectorInfo) string {
	switch connector.Type {
	case "gmail", "calendar", "filesystem":
		return featureSupported
	default:
		return featureUnsupported
	}
}

func capabilitiesDeepLink(connector connectorInfo) string {
	switch connector.Type {
	case "gmail", "calendar", "github":
		return featureSupported
	default:
		return featureUnsupported
	}
}

func capabilitiesConnectors() []capabilitiesConnector {
	connectors := make([]capabilitiesConnector, 0, len(connectorCatalog))
	for _, connector := range connectorCatalog {
		connectors = append(connectors, capabilitiesConnector{
			Type:      connector.Type,
			Name:      connector.DisplayName,
			NeedsAuth: connector.NeedsAuth,
			Ingesting: connector.Ingesting,
			Label:     connector.Label,
			Upcoming:  connector.Upcoming,
			Features: capabilitiesConnectorFeatures{
				Repair:          featureUnsupported,
				DeepLink:        capabilitiesDeepLink(connector),
				IncrementalSync: capabilitiesIncrementalSync(connector),
			},
		})
	}
	// gdrive is deliberately absent because it is a no-op stub outside connectorCatalog.
	sort.Slice(connectors, func(i, j int) bool { return connectors[i].Type < connectors[j].Type })
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
	cfg, err := loadConfigFor(ctx)
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
	errorRegistry, err := capabilitiesErrorRegistryDecoded()
	if err != nil {
		return err
	}
	payload := capabilitiesPayload{
		MoraVersion: BuildVersion,
		Commands:    commands,
		Connectors:  capabilitiesConnectors(),
		Schemas:     schemas,
		ErrorCodes:  capabilitiesErrorCodes(errorRegistry),
		ExitCodes:   capabilitiesExitCodeTable(errorRegistry),
		MCP: capabilitiesMCP{
			WritePolicy: configMCPWritePolicy(cfg),
			Tools:       mcpToolNames(),
			Schemas:     capabilitiesMCPSchemas(),
		},
		Features: capabilitiesFeatures{Repair: featureUnsupported, DeepLink: featureSupported},
	}
	if *jsonOut {
		return emitReceipt(stdout, "mora.capabilities", 1, payload)
	}
	// Human output stays a summary on purpose: rendering 131 command rows for a
	// reader is worse than pointing them at --json.
	fmt.Fprintf(stdout, "Mora %s capabilities: %d command(s), %d connector(s), %d schema(s), %d error code(s), %d MCP tool(s).\n",
		payload.MoraVersion, len(payload.Commands), len(payload.Connectors), len(payload.Schemas), len(payload.ErrorCodes), len(payload.MCP.Tools))
	fmt.Fprintf(stdout, "MCP write policy: %s. Repair: %s. Deep links: %s. Run `mora capabilities --json` for the full contract.\n",
		payload.MCP.WritePolicy, payload.Features.Repair, payload.Features.DeepLink)
	return nil
}
