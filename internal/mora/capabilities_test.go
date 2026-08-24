package mora

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// contract: TestCapabilitiesMatchesRegistries is the enforcing test for CON-04's
// "capabilities cannot lie" property. It drives the real `capabilities --json`
// through Run and asserts, section by section and in BOTH directions, that the
// payload equals the live source it claims to describe.
//
// The mutations that must break it:
//
//   - Filter, drop, or reorder any row in the `commands` projection (for example
//     restoring a `Path == "lint"` filter), or stop emitting `kind`, `platform`,
//     `json_contract`, `payload`, or `reason` on a row.
//   - Add, remove, or rename a connectorCatalog entry, or change its
//     DisplayName, NeedsAuth, Ingesting, Label, or Upcoming, without the payload
//     following.
//   - Add or remove an MCP tool from mcpToolRegistry without mcp.tools following.
//   - Hardcode mcp.write_policy instead of reading Config.mcpWritePolicy().
//   - Publish a feature value outside the three tri-states.
//   - Drop a distinct registry payload name from `schemas`.
//
// What it does NOT catch, stated plainly rather than implied: the `commands` and
// `error_codes` sections are read from `go:embed` of the same two files this test
// reads from disk, so a registry edit moves BOTH sides and this test stays green
// by construction. That is the point of embedding — those two sections cannot
// drift — and the tests that DO react to a bogus registry row are
// TestCLIRegistryMatchesProductionDispatch (no dispatch token),
// TestContractEveryPayloadIsVersioned (the 96-row reconciliation), and
// TestContractGoldenCorpusIsFrozen (the payload changed). What this test catches
// in those two sections is a PROJECTION bug: a filtered, truncated, or
// field-dropping copy of a registry row.

// capabilitiesDocument drives the real command and decodes the payload.
func capabilitiesDocument(t *testing.T) capabilitiesPayload {
	t.Helper()
	stdout, stderr, err := runSplit(t, "capabilities", "--json")
	if err != nil {
		t.Fatalf("capabilities --json: %v (stderr: %s)", err, stderr)
	}
	var payload capabilitiesPayload
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("decode capabilities payload: %v", err)
	}
	return payload
}

func capabilitiesTriState(value string) bool {
	return value == featureSupported || value == featureUnsupported || value == featurePlanned
}

func TestCapabilitiesMatchesRegistries(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	payload := capabilitiesDocument(t)

	t.Run("commands", func(t *testing.T) {
		registry := loadCLIRegistry(t)
		want := map[string]capabilitiesCommand{}
		for _, row := range registry.Commands {
			if _, duplicate := want[row.Path]; duplicate {
				t.Fatalf("registry declares path %q twice", row.Path)
			}
			// A direct conversion rather than a field-by-field literal: it makes
			// the compiler, not this test's author, assert that the payload row
			// and the registry row have exactly the same fields in the same
			// order. Adding a field to one and not the other stops compiling.
			want[row.Path] = capabilitiesCommand(row)
		}
		got := map[string]capabilitiesCommand{}
		for _, command := range payload.Commands {
			if _, duplicate := got[command.Path]; duplicate {
				t.Fatalf("capabilities publishes path %q twice", command.Path)
			}
			got[command.Path] = command
		}
		for path, row := range want {
			published, ok := got[path]
			if !ok {
				t.Errorf("capabilities is missing registry path %q", path)
				continue
			}
			if published != row {
				t.Errorf("capabilities path %q = %+v, registry has %+v", path, published, row)
			}
		}
		for path := range got {
			if _, ok := want[path]; !ok {
				t.Errorf("capabilities publishes path %q, which the registry does not declare", path)
			}
		}
		if !sort.SliceIsSorted(payload.Commands, func(i, j int) bool {
			return payload.Commands[i].Path < payload.Commands[j].Path
		}) {
			t.Error("commands are not sorted by path")
		}
	})

	t.Run("error_codes", func(t *testing.T) {
		registry := loadErrorCodeRegistry(t)
		want := map[string]capabilitiesErrorCode{}
		for _, row := range registry.Codes {
			// Converted, not copied field by field, for the same reason as
			// `commands` above: field correspondence becomes a compile error.
			want[row.Code] = capabilitiesErrorCode(row)
		}
		got := map[string]capabilitiesErrorCode{}
		for _, code := range payload.ErrorCodes {
			got[code.Code] = code
		}
		for code, row := range want {
			published, ok := got[code]
			if !ok {
				t.Errorf("capabilities is missing error code %q", code)
				continue
			}
			if published != row {
				t.Errorf("capabilities error code %q = %+v, registry has %+v", code, published, row)
			}
		}
		for code := range got {
			if _, ok := want[code]; !ok {
				t.Errorf("capabilities publishes error code %q, which the registry does not declare", code)
			}
		}
	})

	t.Run("exit_codes", func(t *testing.T) {
		registry := loadErrorCodeRegistry(t)
		if len(payload.ExitCodes.Allocated) != len(registry.ExitCodes) {
			t.Fatalf("allocated exit codes = %d, registry has %d", len(payload.ExitCodes.Allocated), len(registry.ExitCodes))
		}
		byCode := map[int]capabilitiesExitCode{}
		for _, row := range payload.ExitCodes.Allocated {
			byCode[row.Code] = row
		}
		for _, row := range registry.ExitCodes {
			published, ok := byCode[row.Code]
			if !ok {
				t.Errorf("capabilities is missing exit code %d", row.Code)
				continue
			}
			if published.Status != row.Status || published.Meaning != row.Meaning {
				t.Errorf("exit code %d = %+v, registry has status %q meaning %q", row.Code, published, row.Status, row.Meaning)
			}
		}
		// 3 through 9 are permanently reserved-unused. A caller that only saw the
		// allocated list would assume 3 is next.
		if payload.ExitCodes.Reserved.From != registry.ReservedExitCodes.From ||
			payload.ExitCodes.Reserved.To != registry.ReservedExitCodes.To ||
			payload.ExitCodes.Reserved.Status != registry.ReservedExitCodes.Status ||
			payload.ExitCodes.Reserved.Reason != registry.ReservedExitCodes.Reason {
			t.Errorf("reserved exit codes = %+v, registry has %+v", payload.ExitCodes.Reserved, registry.ReservedExitCodes)
		}
		if payload.ExitCodes.FirstAllocatable != registry.FirstAllocatableExitCode {
			t.Errorf("first_allocatable = %d, registry has %d", payload.ExitCodes.FirstAllocatable, registry.FirstAllocatableExitCode)
		}
		for _, row := range payload.ExitCodes.Allocated {
			if row.Code >= payload.ExitCodes.Reserved.From && row.Code <= payload.ExitCodes.Reserved.To {
				t.Errorf("exit code %d is published as allocated and as reserved", row.Code)
			}
		}
	})

	t.Run("connectors", func(t *testing.T) {
		want := map[string]capabilitiesConnector{}
		for _, connector := range connectorCatalog {
			if connector.Type == "gdrive" {
				t.Fatal("connectorCatalog gained a gdrive row; D-03 keeps it out")
			}
			want[connector.Type] = capabilitiesConnector{
				Type: connector.Type, Name: connector.DisplayName, NeedsAuth: connector.NeedsAuth,
				Ingesting: connector.Ingesting, Label: connector.Label, Upcoming: connector.Upcoming,
				Features: capabilitiesConnectorFeatures{
					Repair:          featureUnsupported,
					DeepLink:        featureUnsupported,
					IncrementalSync: capabilitiesIncrementalSync(connector),
				},
			}
		}
		got := map[string]capabilitiesConnector{}
		for _, connector := range payload.Connectors {
			if connector.Type == "gdrive" {
				t.Fatal("gdrive must stay outside the capabilities payload")
			}
			got[connector.Type] = connector
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("connectors = %+v, catalog has %+v", got, want)
		}
	})

	t.Run("schemas", func(t *testing.T) {
		registry := loadCLIRegistry(t)
		want := map[string]bool{}
		for _, row := range registry.Commands {
			if row.JSONContract == "exempt" || row.Payload == "" {
				continue
			}
			want[row.Payload] = true
		}
		for _, name := range moraExtraSchemas {
			want[name] = true
		}
		got := map[string]bool{}
		for _, schema := range payload.Schemas {
			if schema.Version < 1 {
				t.Errorf("schema %q has version %d", schema.Name, schema.Version)
			}
			got[schema.Name] = true
		}
		for name := range want {
			if !got[name] {
				t.Errorf("capabilities is missing schema %q", name)
			}
		}
		for name := range got {
			if !want[name] {
				t.Errorf("capabilities publishes schema %q, which no registry row names", name)
			}
		}
	})

	t.Run("mcp_tools", func(t *testing.T) {
		want := mcpToolNames()
		if !reflect.DeepEqual(want, payload.MCP.Tools) {
			t.Errorf("mcp.tools = %v, mcpToolNames() = %v", payload.MCP.Tools, want)
		}
		published := map[string]bool{}
		for _, schema := range payload.MCP.Schemas {
			published[schema.Name] = true
		}
		for _, tool := range want {
			if !published["mora.mcp."+tool] {
				t.Errorf("mcp.schemas is missing a version for tool %q", tool)
			}
		}
		if len(published) != len(want) {
			t.Errorf("mcp.schemas has %d entries for %d tools", len(published), len(want))
		}
	})

	t.Run("features_are_tri_state", func(t *testing.T) {
		if payload.Features.Repair != featureUnsupported {
			t.Errorf("features.repair = %q, want %q until Phase 3 lands repair", payload.Features.Repair, featureUnsupported)
		}
		if payload.Features.DeepLink != featureUnsupported {
			t.Errorf("features.deep_link = %q, want %q until Phase 5 lands deep links", payload.Features.DeepLink, featureUnsupported)
		}
		for _, connector := range payload.Connectors {
			for name, value := range map[string]string{
				"repair":           connector.Features.Repair,
				"deep_link":        connector.Features.DeepLink,
				"incremental_sync": connector.Features.IncrementalSync,
			} {
				if !capabilitiesTriState(value) {
					t.Errorf("connector %q feature %s = %q, not one of supported|unsupported|planned", connector.Type, name, value)
				}
			}
			if connector.Features.DeepLink != featureUnsupported {
				t.Errorf("connector %q publishes deep_link %q; no deep-link code exists", connector.Type, connector.Features.DeepLink)
			}
		}
	})
}

// TestCapabilitiesWritePolicyFollowsConfig proves mcp.write_policy is read live
// rather than stamped from a constant, by setting the config to each of the
// three policy values in turn and re-driving the command.
func TestCapabilitiesWritePolicyFollowsConfig(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	for _, policy := range []string{mcpWritePolicyPropose, mcpWritePolicyReadonly, mcpWritePolicyOpen} {
		run(t, "config", "mcp-write-policy", policy)
		if got := capabilitiesDocument(t).MCP.WritePolicy; got != policy {
			t.Errorf("mcp.write_policy = %q after setting config to %q", got, policy)
		}
	}
}

func TestCapabilitiesContract(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	var payload map[string]any
	if err := json.Unmarshal([]byte(run(t, "capabilities", "--json")), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["schema"] != "mora.capabilities" || payload["schema_version"] != float64(1) {
		t.Fatalf("receipt envelope = %#v", payload)
	}
	for _, key := range []string{"commands", "connectors", "schemas"} {
		if values, ok := payload[key].([]any); !ok || values == nil {
			t.Fatalf("%s = %#v, want non-nil array", key, payload[key])
		}
	}
	features, ok := payload["features"].(map[string]any)
	if !ok || features["repair"] != featureUnsupported || features["deep_link"] != featureUnsupported {
		t.Fatalf("features = %#v", payload["features"])
	}
	mcp, ok := payload["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("mcp = %#v", payload["mcp"])
	}
	// Bound to the tool registry rather than to a literal. A hard-coded count
	// reds this test every time a tool is legitimately added — it did, when
	// calendar_events landed — while proving nothing the mcp_tools drift
	// subtest above does not already prove exactly.
	wantTools := len(mcpToolNames())
	if tools, ok := mcp["tools"].([]any); !ok || len(tools) != wantTools {
		t.Fatalf("mcp.tools = %#v, want %d entries", mcp["tools"], wantTools)
	}
	for _, connector := range payload["connectors"].([]any) {
		if connector.(map[string]any)["type"] == "gdrive" {
			t.Fatal("gdrive must stay outside the capabilities payload")
		}
	}
}
