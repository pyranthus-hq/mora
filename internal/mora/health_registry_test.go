package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// health_registry_test.go — Packet C3's completeness proof.
//
// TestEverySurfaceCarriesHealth is mutation-matrix row 14: drop `health` (or
// the banner) from any ONE registered surface and this test — not a helper-
// level unit test — must go red.
//
// TestSixDayFreezeSurfacesOnEverySurface discharges HEALTH-05 (the six-day
// disk-full incident replay) out of "verify-only": Gate 1's
// TestSixDayFreezeSurfacesWithin24h (incident_replay_test.go) proved the typed
// state, doctor --json/--strict/--pulse, the daily brief, and the meeting
// brief — the surfaces Gate 1 had wired, a STRICT SUBSET of "every affected
// surface." This drives the SAME frozen-source fixture at T0+25h through
// EVERY entry in the derived/explicit registries above.

// mcpSurfaceArgs returns minimal valid arguments for driving one MCP tool
// through its real dispatcher. Chosen so every tool succeeds (no isError) on a
// vault seeded with a "read-target"/"delete-target" memory pair.
func mcpSurfaceArgs(name string) map[string]any {
	switch name {
	case "write_memory":
		return map[string]any{"title": "Surface probe", "text": "surface probe body"}
	case "read_memory":
		return map[string]any{"id": "read-target"}
	case "delete_memory":
		return map[string]any{"id": "delete-target"}
	case "search_memory":
		return map[string]any{"query": "surface"}
	case "context_memory":
		return map[string]any{"query": "surface"}
	case "think":
		return map[string]any{"query": "surface"}
	case "get_entity":
		return map[string]any{"name": "Nonexistent Surface Entity"}
	case "calendar_events":
		return map[string]any{"start": "2026-01-01", "end": "2026-01-02"}
	default: // list_memory, list_entities, digest, brief, meeting_prep
		return map[string]any{}
	}
}

// mcpToolCompactHealth extracts the compact health envelope from an MCP tool's
// native return value, regardless of which of the two shapes (a wrapped
// map[string]any, or MeetingBrief's own Health field) that tool uses.
func mcpToolCompactHealth(v any) (compactHealth, bool) {
	switch tv := v.(type) {
	case MeetingBrief:
		return tv.Health, true
	case map[string]any:
		if h, ok := tv["health"].(compactHealth); ok {
			return h, true
		}
	}
	return compactHealth{}, false
}

// httpSurfaceRequest builds a minimal authorized request for one registered
// HTTP route.
func httpSurfaceRequest(rt httpRoute) *http.Request {
	var req *http.Request
	switch rt.Method + " " + rt.Pattern {
	case "POST /think", "POST /search":
		req = httptest.NewRequest(rt.Method, rt.Pattern, strings.NewReader(`{"q":"surface"}`))
	case "POST /write":
		req = httptest.NewRequest(rt.Method, rt.Pattern, strings.NewReader(`{"title":"HTTP surface","text":"http surface body"}`))
	case "POST /meeting-prep":
		req = httptest.NewRequest(rt.Method, rt.Pattern, strings.NewReader(`{}`))
	case "GET /entity/{name}":
		req = httptest.NewRequest("GET", "/entity/Nonexistent", nil)
	default:
		req = httptest.NewRequest(rt.Method, rt.Pattern, nil)
	}
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Authorization", "Bearer tok")
	return req
}

// bannerWithinFirstLines fails the test unless the health banner appears as
// one of the first two lines — line 0 for surfaces with no header of their
// own (search/list/read/…), line 1 for the digest-shaped ones (brief/pulse),
// right after their "# Mora digest" header (HEALTH-05's "first content line").
func bannerWithinFirstLines(t *testing.T, label, out string) {
	t.Helper()
	lines := strings.SplitN(out, "\n", 3)
	for i := 0; i < len(lines) && i < 2; i++ {
		if strings.HasPrefix(lines[i], healthBannerLinePrefix) {
			return
		}
	}
	t.Fatalf("%s: banner is not within the first two lines:\n%s", label, out)
}

// driveEverySurface is the shared completeness loop both TestEverySurfaceCarriesHealth
// and TestSixDayFreezeSurfacesOnEverySurface run over an already-seeded, already-
// unhealthy vault: every MCP tool minus mcpHealthExemptTools, every HTTP route
// minus httpHealthExemptRoutes, every cliHealthSurfaces verb, plus doctor's own
// typed-payload check. No helper-level shortcut — each surface goes through its
// REAL dispatcher (callMCPTool, the real httpServer handler chain, Run([...])).
func driveEverySurface(t *testing.T, ctx context.Context, cfg Config) {
	t.Helper()

	for _, name := range mcpToolNames() {
		if mcpHealthExemptTools[name] {
			continue
		}
		res, err := callMCPTool(ctx, name, mcpSurfaceArgs(name))
		if err != nil {
			t.Fatalf("mcp %s: %v", name, err)
		}
		h, ok := mcpToolCompactHealth(res)
		if !ok {
			t.Fatalf("mcp %s: no compact health envelope in the response: %#v", name, res)
		}
		if h.State == "" {
			t.Fatalf("mcp %s: health.state is empty", name)
		}
	}

	srv := &httpServer{token: "tok", port: 7777}
	handler := srv.handler()
	for _, rt := range srv.httpRoutes() {
		key := rt.Method + " " + rt.Pattern
		if httpHealthExemptRoutes[key] {
			continue
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httpSurfaceRequest(rt))
		if rec.Code >= 400 {
			t.Fatalf("http %s: status %d body=%s", key, rec.Code, rec.Body.String())
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte(`"health"`)) {
			t.Fatalf("http %s: response has no \"health\" key: %s", key, rec.Body.String())
		}
	}

	for _, argv := range cliHealthSurfaces {
		var out bytes.Buffer
		if err := Run(ctx, argv, &out, &out, strings.NewReader("")); err != nil {
			t.Fatalf("mora %s: %v\n%s", strings.Join(argv, " "), err, out.String())
		}
		bannerWithinFirstLines(t, "mora "+strings.Join(argv, " "), out.String())
	}

	// doctor: its OWN pre-existing typed-payload convention (see
	// cliHealthSurfaces' doc comment) — not the rendered banner.
	var docOut bytes.Buffer
	if err := Run(ctx, []string{"doctor", "--json"}, &docOut, &docOut, strings.NewReader("")); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, docOut.String())
	}
	var rep doctorReport
	if err := json.Unmarshal(docOut.Bytes(), &rep); err != nil {
		t.Fatalf("doctor --json decode: %v\n%s", err, docOut.String())
	}
	if len(rep.Sources) == 0 {
		t.Fatalf("doctor --json must carry typed per-source health: %s", docOut.String())
	}
}

// TestEverySurfaceCarriesHealth is mutation-matrix row 14: every registered
// MCP tool, HTTP route, and required CLI verb carries the typed compact health
// envelope (or, for the CLI, renders the banner) on an unhealthy vault.
func TestEverySurfaceCarriesHealth(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	for _, id := range []string{"read-target", "delete-target"} {
		if err := writeMemory(cfg, Memory{
			ID: id, Scope: "global", Type: "note", Title: "Surface " + id,
			CreatedAt: "2026-05-01T00:00:00Z", Text: "surface body for " + id,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", time.Now().Add(-30*time.Hour)) // stale (>24h threshold)

	driveEverySurface(t, ctx, cfg)
}

// TestSixDayFreezeSurfacesOnEverySurface discharges HEALTH-05 out of
// verify-only (the requirement text is "the six-day disk-full incident is
// replayed and EVERY AFFECTED SURFACE becomes visibly unhealthy within 24
// hours"): the SAME frozen-source fixture as
// TestSixDayFreezeSurfacesWithin24h (incident_replay_test.go), at T0+25h,
// driven through every entry in the registries above — not just the surfaces
// Gate 1 had wired.
func TestSixDayFreezeSurfacesOnEverySurface(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	for _, id := range []string{"read-target", "delete-target"} {
		if err := writeMemory(cfg, Memory{
			ID: id, Scope: "global", Type: "note", Title: "Surface " + id,
			CreatedAt: "2026-05-01T00:00:00Z", Text: "surface body for " + id,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}

	t0 := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	enableSources(t, cfg, "gmail", "imessage")
	seedSyncStatus(t, cfg, "gmail", t0)
	seedSyncStatus(t, cfg, "imessage", t0)

	const incidentError = "database or disk is full (13)"
	gmailPath := syncStatusPathFor(cfg, Source{Name: "gmail", Type: "gmail"})
	imessagePath := syncStatusPathFor(cfg, Source{Name: "imessage", Type: "imessage"})
	for h := 1; h <= 25; h++ {
		attemptAt := t0.Add(time.Duration(h) * time.Hour).UTC().Format(time.RFC3339)
		for _, path := range []string{gmailPath, imessagePath} {
			st, err := memory.LoadStatus(path)
			if err != nil {
				t.Fatalf("LoadStatus(%s): %v", path, err)
			}
			st.LastAttemptAt = attemptAt
			st.LastError = incidentError
			st.ErrorCount++
			if err := memory.SaveStatus(path, st); err != nil {
				t.Fatalf("SaveStatus(%s): %v", path, err)
			}
		}
	}

	now := t0.Add(25 * time.Hour)
	origBriefClock := briefClock
	briefClock = func() time.Time { return now }
	t.Cleanup(func() { briefClock = origBriefClock })
	origDoctorClock := doctorClock
	doctorClock = func() time.Time { return now }
	t.Cleanup(func() { doctorClock = origDoctorClock })
	origHookNow := hookNow
	hookNow = func() time.Time { return now }
	t.Cleanup(func() { hookNow = origHookNow })

	driveEverySurface(t, ctx, cfg)
}
