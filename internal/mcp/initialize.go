package mcp

const (
	ProtocolVersion               = "2024-11-05"
	ServerName                    = "mora"
	ConfigUnavailableInstructions = "Mora could not load its configuration, so tools are unavailable and no mutation will be attempted. Fix config.toml and reconnect."
)

// InitializeResult constructs the canonical MCP initialization handshake.
func InitializeResult(buildVersion, writePolicy string, configAvailable bool) map[string]any {
	instructions := ConfigUnavailableInstructions
	if configAvailable {
		instructions = InstructionsFor(writePolicy)
	}
	return map[string]any{"protocolVersion": ProtocolVersion, "serverInfo": map[string]string{"name": ServerName, "version": buildVersion}, "capabilities": map[string]any{"tools": map[string]any{}}, "instructions": instructions}
}
