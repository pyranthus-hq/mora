package mora

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
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

// echo is an entity name the caller sent: a hides surface may repeat it back.
// It is removed from hides surfaces only. A labels surface is never scrubbed,
// and its queries use words that are not the canary.
func checkSurface(t *testing.T, name, class, out, echo string) {
	t.Helper()
	low := strings.ToLower(out)
	switch class {
	case surfaceHides:
		if echo != "" {
			low = strings.ReplaceAll(low, strings.ToLower(echo), "")
		}
		if strings.Contains(low, tentativeCanry) {
			t.Errorf("%s leaks the tentative canary:\n%s", name, out)
		}
	case surfaceLabels:
		// Every canary occurrence needs "tentative" within a short window.
		for from := 0; ; {
			i := strings.Index(low[from:], tentativeCanry)
			if i < 0 {
				break
			}
			i += from
			lo, hi := max(0, i-300), min(len(low), i+300)
			if !strings.Contains(low[lo:hi], "tentative") {
				t.Errorf("%s shows the canary without a tentative label beside it:\n%s", name, out)
				break
			}
			from = i + len(tentativeCanry)
		}
	}
}

func argEcho(a map[string]any) string {
	for _, k := range []string{"name"} {
		if v, ok := a[k].(string); ok {
			return v
		}
	}
	return ""
}

func TestTentativeSurfaceContract(t *testing.T) {
	// The decision, task, personal and global rows save at once since the hold
	// rule widened, so each one must pass the same sweep as the project fact.
	for _, c := range []struct{ typ, scope string }{
		{"fact", "project:" + tentativeCanry + "tag"},
		{"decision", "project:" + tentativeCanry + "tag"},
		{"task", "project:" + tentativeCanry + "tag"},
		{"fact", "personal"},
		{"insight", "global"},
	} {
		t.Run(c.typ+" "+c.scope, func(t *testing.T) { runTentativeSurfaceContract(t, c.typ, c.scope) })
	}
}

// surfaceOut is one surface's output for the sweep.
type surfaceOut struct{ name, class, out, echo string }

// reaches says whether a surface output holds the fixture. An entity surface
// shows the fixture as an entity row, not as the canary text, so it has its own
// test; every other surface must show the canary itself.
func (o surfaceOut) reaches(entity string) bool {
	low := strings.ToLower(o.out)
	switch o.name {
	case "mcp get_entity":
		return strings.Contains(low, `"found":true`)
	case "cli entity":
		return !strings.Contains(low, "no entity named")
	case "mcp list_entities", "cli entities":
		return strings.Contains(low, strings.ToLower(entity))
	}
	if o.echo != "" {
		low = strings.ReplaceAll(low, strings.ToLower(o.echo), "")
	}
	return strings.Contains(low, tentativeCanry)
}

// unreachableByNote lists hides surfaces that no agent note of these types can
// ever reach, even when confirmed, so they cannot have a positive control.
var unreachableByNote = map[string]string{
	"mcp calendar_events":    "lists type event rows only, and write_memory cannot save that type",
	"mcp meeting_prep":       "briefs an upcoming calendar event and its attendees, and no agent note is one",
	"companion meeting_prep": "same as mcp meeting_prep",
}

func runTentativeSurfaceContract(t *testing.T, typ, scope string) {
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	entity := scope // the graph names a scope entity by the full scope string
	id := tentativeMemoryID(t, tentativeServe(t, map[string]any{
		"title": "Canary " + tentativeCanry, "text": tentativeCanry + " Zorbleton owns the deploy pipeline. I will send Jordan the report by Friday.",
		"type": typ, "scope": scope,
	}))
	run(t, "index", "rebuild")

	for name := range mcpToolHandlers {
		if tentativeToolClass[name] == "" {
			t.Errorf("MCP tool %q has no tentative surface class: declare hides, labels or writes", name)
		}
	}
	args := map[string]map[string]any{
		"read_memory":     {"id": id},
		"search_memory":   {"query": "Zorbleton deploy pipeline"},
		"list_memory":     {},
		"context_memory":  {"query": "Zorbleton deploy pipeline"},
		"think":           {"query": "Zorbleton deploy pipeline"},
		"list_entities":   {},
		"get_entity":      {"name": entity},
		"digest":          {"since_hours": float64(24)},
		"brief":           {},
		"meeting_prep":    {},
		"calendar_events": {"start": "2019-01-01", "end": "2030-01-01"},
	}
	sweep := func() []surfaceOut {
		var outs []surfaceOut
		for name, class := range tentativeToolClass {
			if class == surfaceWrites {
				continue
			}
			res, err := mcpToolHandlers[name](testCtx(t), cfg, args[name])
			if err != nil {
				t.Fatalf("mcp %s errored: %v", name, err)
			}
			b, _ := json.Marshal(res)
			outs = append(outs, surfaceOut{"mcp " + name, class, string(b), argEcho(args[name])})
		}
		reader := newCompanionReader(cfg)
		for _, mode := range []companion.ContextMode{companion.ModeThink, companion.ModeSearch, companion.ModeMeetingPrep} {
			req := companion.NewContextRequest()
			req.Mode = mode
			req.Query = "Zorbleton deploy pipeline"
			bundle, err := reader.Context(testCtx(t), req)
			if err != nil {
				t.Fatalf("companion %s errored: %v", mode, err)
			}
			b, _ := json.Marshal(bundle)
			outs = append(outs, surfaceOut{"companion " + string(mode), surfaceHides, string(b), ""})
		}
		cli := []struct {
			name, class string
			args        []string
		}{
			{"read", surfaceLabels, []string{"read", id}},
			{"list", surfaceLabels, []string{"list"}},
			{"search", surfaceLabels, []string{"search", "Zorbleton deploy pipeline"}},
			{"entities", surfaceHides, []string{"entities"}},
			{"entity", surfaceHides, []string{"entities", entity}},
		}
		for _, c := range cli {
			var out bytes.Buffer
			if err := Run(testCtx(t), c.args, &out, &out, strings.NewReader("")); err != nil {
				t.Fatalf("cli %s errored: %v\n%s", c.name, err, out.String())
			}
			echo := ""
			if c.name == "entity" {
				echo = entity
			}
			outs = append(outs, surfaceOut{"cli " + c.name, c.class, out.String(), echo})
		}
		return outs
	}

	for _, o := range sweep() {
		checkSurface(t, o.name, o.class, o.out, o.echo)
		if o.class == surfaceHides && o.reaches(entity) {
			t.Errorf("%s shows the tentative fixture:\n%s", o.name, o.out)
		}
	}
	// Positive control: the same fixture, once confirmed, must reach every
	// surface that can reach it. Without this a hidden row proves nothing.
	run(t, "confirm", id)
	run(t, "index", "rebuild")
	for _, o := range sweep() {
		if _, skip := unreachableByNote[o.name]; skip {
			continue
		}
		if !o.reaches(entity) {
			t.Errorf("control: %s does not show the confirmed fixture, so its tentative check proves nothing:\n%s", o.name, o.out)
		}
	}
}

// CLI `context --json` receipts name the memory without its body, so each one
// carries the trust itself. A small budget can drop the labelled blob and keep
// the receipts.
func TestTentativeContextReceiptsCarryTrust(t *testing.T) {
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	run(t, "init")
	id := tentativeMemoryID(t, tentativeServe(t, map[string]any{
		"title": "Canary " + tentativeCanry, "text": "Zorbleton owns the deploy pipeline.", "type": "fact", "scope": "project:demo",
	}))
	run(t, "index", "rebuild")
	for _, args := range [][]string{
		{"context", "--json", "--budget", "200", "Zorbleton deploy pipeline"},
		{"context", "--json", "--budget", "200"},
	} {
		out := run(t, args...)
		var res struct {
			Items []struct{ ID, Trust string } `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		found := false
		for _, it := range res.Items {
			if it.ID == id {
				found = true
				if it.Trust != "tentative" {
					t.Errorf("%v: receipt for %s has trust %q", args, id, it.Trust)
				}
			}
		}
		if !found {
			t.Errorf("%v: no receipt for the tentative note:\n%s", args, out)
		}
	}
}

// Filtering must come before the limit: newer tentative notes may not push a
// confirmed memory out of the default list.
func TestTentativeDoesNotCrowdOutListMemory(t *testing.T) {
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "project:demo", "--type", "fact", "--title", "Confirmed baseline", "--text", "Confirmed note.")
	cfg := mustConfig(t)
	rows, err := listMemories(cfg, "project:demo", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("baseline rows = %v, %v", rows, err)
	}
	// Age the confirmed note so the drafts below are certainly newer.
	path := mustFindPath(t, cfg, rows[0].ID)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	aged := regexp.MustCompile(`(?m)^created_at: .*$`).ReplaceAllString(string(raw), "created_at: 2001-01-01T00:00:00Z")
	if aged == string(raw) {
		t.Fatalf("no created_at line to age:\n%s", raw)
	}
	if err := os.WriteFile(path, []byte(aged), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		tentativeMemoryID(t, tentativeServe(t, map[string]any{
			"title": "Draft " + strings.Repeat("x", i+1), "text": "agent draft", "type": "fact", "scope": "project:demo",
		}))
	}
	res, err := mcpToolHandlers["list_memory"](testCtx(t), cfg, map[string]any{"limit": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), "Confirmed baseline") {
		t.Fatalf("list_memory limit 2 lost the confirmed memory behind tentative ones:\n%s", b)
	}
}
