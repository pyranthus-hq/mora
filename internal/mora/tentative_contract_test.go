package mora

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/companion"
)

// Every read surface either hides a tentative memory or labels it. A tool in
// mcpToolHandlers with no class here fails, so a new tool cannot ship unreviewed.
const (
	surfaceHides   = "hides"  // the canary must not appear
	surfaceLabels  = "labels" // the canary may appear, with "tentative" beside it
	surfaceWrites  = "writes" // not a read surface
	tentativeCanry = "zorblequxcanary"
)

var tentativeToolClass = map[string]string{
	"write_memory":    surfaceWrites,
	"delete_memory":   surfaceWrites,
	"read_memory":     surfaceLabels,
	"search_memory":   surfaceLabels,
	"list_memory":     surfaceLabels,
	"context_memory":  surfaceLabels,
	"think":           surfaceHides,
	"list_entities":   surfaceHides,
	"get_entity":      surfaceHides,
	"digest":          surfaceHides,
	"brief":           surfaceHides,
	"meeting_prep":    surfaceHides,
	"calendar_events": surfaceHides,
}

// echo is the query or name the caller sent: a surface may repeat it back.
func checkSurface(t *testing.T, name, class, out, echo string) {
	t.Helper()
	low := strings.ToLower(out)
	if echo != "" {
		low = strings.ReplaceAll(low, strings.ToLower(echo), "")
	}
	has := strings.Contains(low, tentativeCanry)
	switch class {
	case surfaceHides:
		if has {
			t.Errorf("%s leaks the tentative canary:\n%s", name, out)
		}
	case surfaceLabels:
		if has && !strings.Contains(low, "tentative") {
			t.Errorf("%s shows the canary without a tentative label:\n%s", name, out)
		}
	}
}

func argEcho(a map[string]any) string {
	for _, k := range []string{"query", "name"} {
		if v, ok := a[k].(string); ok {
			return v
		}
	}
	return ""
}

func TestTentativeSurfaceContract(t *testing.T) {
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := tentativeMemoryID(t, tentativeServe(t, map[string]any{
		"title": "Canary " + tentativeCanry, "text": tentativeCanry + " Zorbleton owns the deploy pipeline.",
		"type": "fact", "scope": "project:" + tentativeCanry + "tag",
	}))
	run(t, "index", "rebuild")

	for name := range mcpToolHandlers {
		if tentativeToolClass[name] == "" {
			t.Errorf("MCP tool %q has no tentative surface class: declare hides, labels or writes", name)
		}
	}
	args := map[string]map[string]any{
		"read_memory":     {"id": id},
		"search_memory":   {"query": tentativeCanry},
		"list_memory":     {},
		"context_memory":  {"query": tentativeCanry},
		"think":           {"query": "Zorbleton deploy pipeline"},
		"list_entities":   {},
		"get_entity":      {"name": tentativeCanry + "tag"},
		"digest":          {},
		"brief":           {},
		"meeting_prep":    {"name": tentativeCanry + "tag"},
		"calendar_events": {"start": "2019-01-01", "end": "2030-01-01"},
	}
	for name, class := range tentativeToolClass {
		if class == surfaceWrites {
			continue
		}
		res, err := mcpToolHandlers[name](testCtx(t), cfg, args[name])
		if err != nil {
			t.Logf("%s returned an error (counts as no leak): %v", name, err)
			continue
		}
		b, _ := json.Marshal(res)
		checkSurface(t, "mcp "+name, class, string(b), argEcho(args[name]))
	}

	// Companion context modes.
	reader := newCompanionReader(cfg)
	for _, mode := range []companion.ContextMode{companion.ModeThink, companion.ModeSearch, companion.ModeMeetingPrep} {
		req := companion.NewContextRequest()
		req.Mode = mode
		req.Query = "Zorbleton deploy pipeline " + tentativeCanry + "tag"
		bundle, err := reader.Context(testCtx(t), req)
		if err != nil {
			t.Logf("companion %s errored (counts as no leak): %v", mode, err)
			continue
		}
		b, _ := json.Marshal(bundle)
		checkSurface(t, "companion "+string(mode), surfaceHides, string(b), req.Query)
	}

	// CLI read commands.
	cli := map[string]struct {
		class string
		args  []string
	}{
		"read":     {surfaceLabels, []string{"read", id}},
		"list":     {surfaceLabels, []string{"list"}},
		"search":   {surfaceLabels, []string{"search", "Zorbleton deploy pipeline"}},
		"entities": {surfaceHides, []string{"entities"}},
		"entity":   {surfaceHides, []string{"entities", tentativeCanry + "tag"}},
	}
	for name, c := range cli {
		var out bytes.Buffer
		if err := Run(testCtx(t), c.args, &out, &out, strings.NewReader("")); err != nil {
			t.Logf("cli %s errored (counts as no leak): %v", name, err)
		}
		checkSurface(t, "cli "+name, c.class, out.String(), c.args[len(c.args)-1])
	}
}
