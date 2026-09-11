package mora

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	ingestpkg "github.com/pyranthus-hq/mora/internal/ingest"
	"github.com/pyranthus-hq/mora/internal/integrations"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func setupConnectorSteps(cfg Config, check doctorCheckReport, clients []integrations.Status) []setupStep {
	steps := make([]setupStep, 0, 3)

	readable := setupStep{ID: "connector_readable", State: "pending", Next: "mora doctor check imessage-access --json"}
	switch {
	case check.OK:
		readable.State = "verified"
		readable.Evidence = "the Messages database is readable (Full Disk Access granted)"
		readable.Next = ""
	case !check.PlatformSupported:
		readable.Evidence = "iMessage is macOS-only; use Calendar or a folder"
	default:
		readable.Evidence = "iMessage access check reported " + check.Reason
	}
	steps = append(steps, readable)

	ingest := setupStep{ID: "connector_ingest", State: "pending", Next: "mora connect imessage --json --progress",
		Evidence: "no source has completed a read yet"}
	if n, latest := completedReads(cfg); n > 0 {
		ingest.State = "verified"
		ingest.Next = ""
		ingest.Evidence = fmt.Sprintf("%d source(s) have completed a read; latest %s", n, latest)
	}
	steps = append(steps, ingest)

	mcp := setupStep{ID: "mcp_registration", State: "pending", Evidence: "no AI client is registered to this Mora binary",
		Next: "mora integrations connect --client <claude|codex|cursor|claude-desktop> --binary <path> --json"}
	var registered []string
	for _, c := range clients {
		if c.Registered && c.MatchesBinary {
			registered = append(registered, c.Client)
		}
	}
	if len(registered) > 0 {
		sort.Strings(registered)
		mcp.State = "verified"
		mcp.Next = ""
		mcp.Evidence = "registered: " + strings.Join(registered, ", ")
	}
	steps = append(steps, mcp)
	return steps
}

// completedReads counts enabled sources whose sync status carries a
// last_success_at and names the most recent one as type:name at time.
func completedReads(cfg Config) (int, string) {
	sources := loadSourcesOrEmpty(cfg)
	count := 0
	var latestAt time.Time
	latest := ""
	for _, s := range sources {
		if !s.IsEnabled() {
			continue
		}
		// Status filenames use provider prefixes, which can differ from source types.
		prefix := s.Type
		switch s.Type {
		case "gmail", "calendar":
			prefix = "google"
		case "applecalendar":
			prefix = "applecal"
		}
		st, err := memory.LoadStatus(ingestpkg.StatusPath(cfg, prefix, s.Name))
		if err != nil || st == nil || st.LastSuccessAt == "" {
			continue
		}
		count++
		at, perr := time.Parse(time.RFC3339, st.LastSuccessAt)
		if perr != nil {
			at = time.Time{}
		}
		if latest == "" || at.After(latestAt) {
			latestAt = at
			latest = fmt.Sprintf("%s:%s at %s", s.Type, s.Name, st.LastSuccessAt)
		}
	}
	return count, latest
}

func setupIntegrationClients(cfg Config) []integrations.Status {
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	list, err := integrations.List(integrations.DefaultSeams(cfg.HomeDir()), exe)
	if err != nil {
		return nil
	}
	return list
}
