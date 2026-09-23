package mora

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/integrations"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func stepByID(t *testing.T, steps []setupStep, id string) setupStep {
	t.Helper()
	for _, s := range steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no step %q in %+v", id, steps)
	return setupStep{}
}

func TestSetupConnectorStepsPendingOnFreshVault(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	check := doctorCheckReport{Check: "imessage-access", PlatformSupported: true, Reason: "chat_db_missing"}
	steps := setupConnectorSteps(cfg, check, []integrations.Status{{Client: "claude", Detected: true}})
	if got := stepByID(t, steps, "connector_readable"); got.State != "pending" || !strings.Contains(got.Evidence, "chat_db_missing") {
		t.Fatalf("connector_readable: %+v", got)
	}
	if got := stepByID(t, steps, "connector_ingest"); got.State != "pending" || got.Next != "mora connect imessage --json --progress" {
		t.Fatalf("connector_ingest: %+v", got)
	}
	if got := stepByID(t, steps, "mcp_registration"); got.State != "pending" || !strings.Contains(got.Next, "integrations connect") {
		t.Fatalf("mcp_registration: %+v", got)
	}
}

func TestSetupConnectorStepsVerifiedAfterARead(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("enabling iMessage requires macOS")
	}
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	run(t, "connectors", "enable", "imessage")
	sources, err := loadSources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	for _, s := range sources {
		if s.Type == "imessage" {
			name = s.Name
		}
	}
	if name == "" {
		t.Fatal("connectors enable imessage must register a source")
	}
	statusPath := imessageStatusPath(cfg, name)
	if err := memory.SaveStatus(statusPath, &memory.SyncStatus{Source: name, LastSuccessAt: "2026-09-10T00:00:00Z", ItemCount: 2}); err != nil {
		t.Fatal(err)
	}
	check := doctorCheckReport{Check: "imessage-access", OK: true, PlatformSupported: true, ChatDBPresent: true, Readable: true}
	clients := []integrations.Status{{Client: "codex", Detected: true, Registered: true, MatchesBinary: true}}
	steps := setupConnectorSteps(cfg, check, clients)
	if got := stepByID(t, steps, "connector_readable"); got.State != "verified" {
		t.Fatalf("connector_readable: %+v", got)
	}
	if got := stepByID(t, steps, "connector_ingest"); got.State != "verified" || !strings.Contains(got.Evidence, "imessage:"+name) {
		t.Fatalf("connector_ingest: %+v", got)
	}
	if got := stepByID(t, steps, "mcp_registration"); got.State != "verified" || !strings.Contains(got.Evidence, "codex") {
		t.Fatalf("mcp_registration: %+v", got)
	}
}

func TestSetupStatusJSONCarriesSixSteps(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	stdout, _, err := runSplit(t, "setup", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Steps           []setupStep `json:"steps"`
		RemainingChecks []string    `json:"remaining_checks"`
		Schema          string      `json:"schema"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if doc.Schema != "mora.setup.status" || len(doc.Steps) != 6 {
		t.Fatalf("want 6 steps, got %d: %+v", len(doc.Steps), doc.Steps)
	}
	for _, id := range []string{"local_layout", "committed_index", "credential_storage", "connector_readable", "connector_ingest", "mcp_registration"} {
		stepByID(t, doc.Steps, id)
	}
	for _, remaining := range doc.RemainingChecks {
		if strings.Contains(remaining, "connector") || strings.Contains(remaining, "MCP registration") {
			t.Fatalf("remaining_checks still lists a check this task implements: %q", remaining)
		}
	}
}

func TestSetupConnectorStepsUnsupportedAndMismatchedClients(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	steps := setupConnectorSteps(cfg, doctorCheckReport{}, []integrations.Status{
		{Client: "codex", Registered: true},
		{Client: "claude", MatchesBinary: true},
	})
	if len(steps) != 3 {
		t.Fatalf("steps: %+v", steps)
	}
	for i, id := range []string{"connector_readable", "connector_ingest", "mcp_registration"} {
		if steps[i].ID != id || steps[i].State != "pending" {
			t.Fatalf("step %d: %+v", i, steps[i])
		}
	}
	if steps[0].Evidence != "iMessage is macOS-only; use Calendar or a folder" || steps[0].Next != "mora doctor check imessage-access --json" {
		t.Fatalf("readability: %+v", steps[0])
	}
	steps = setupConnectorSteps(cfg, doctorCheckReport{}, []integrations.Status{
		{Client: "codex", Registered: true, MatchesBinary: true},
		{Client: "claude", Registered: true, MatchesBinary: true},
	})
	if got := steps[2]; got.State != "verified" || got.Evidence != "registered: claude, codex" || got.Next != "" {
		t.Fatalf("registration: %+v", got)
	}
}

func TestSetupConnectorStepsCountsEnabledProviderReads(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	sources := []Source{
		{Type: "gmail", Name: "gmail"},
		{Type: "calendar", Name: "calendar"},
		{Type: "applecalendar", Name: "applecalendar"},
		{Type: "filesystem", Name: "disabled", Enabled: &disabled},
		{Type: "filesystem", Name: "never"},
		{Type: "filesystem", Name: "missing"},
		{Type: "filesystem", Name: "corrupt"},
	}
	if err := saveSources(cfg, sources); err != nil {
		t.Fatal(err)
	}
	for i, source := range sources[:5] {
		at := "2026-09-10T00:00:00Z"
		if i == 2 {
			at = "2026-09-10T02:00:00Z"
		}
		if i == 3 {
			at = "2026-09-11T00:00:00Z"
		}
		if i == 4 {
			at = ""
		}
		if err := memory.SaveStatus(syncStatusPathFor(cfg, source), &memory.SyncStatus{Source: source.Name, LastSuccessAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(syncStatusPathFor(cfg, sources[6]), []byte("invalid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	steps := setupConnectorSteps(cfg, doctorCheckReport{}, nil)
	if got := stepByID(t, steps, "connector_ingest"); got.State != "verified" || got.Next != "" || got.Evidence != "3 source(s) have completed a read; latest applecalendar:applecalendar at 2026-09-10T02:00:00Z" {
		t.Fatalf("ingest: %+v", got)
	}
}
