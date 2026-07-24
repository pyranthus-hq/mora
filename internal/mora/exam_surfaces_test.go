package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/mora/exam"
)

const (
	examSurfaceScorecardsPath   = "exam/testdata/surface-scorecards.golden.json"
	examSurfaceScorecardsV2Path = "exam/testdata/surface-scorecards-v2.golden.json"
)

type examSurfaceScorecards struct {
	DailyCLI  exam.Scorecard `json:"daily_cli"`
	DailyMCP  exam.Scorecard `json:"daily_mcp"`
	EventCLI  exam.Scorecard `json:"event_cli"`
	EventMCP  exam.Scorecard `json:"event_mcp"`
	HomeState string         `json:"home_state"`

	dailyCLIPredictions []exam.Prediction
	dailyMCPPredictions []exam.Prediction
	eventCLIPredictions []exam.Prediction
	eventMCPPredictions []exam.Prediction
}

func pinExamSurfaceClocks(t *testing.T, at time.Time) {
	t.Helper()
	oldBrief, oldPrep := briefClock, prepClock
	briefClock = func() time.Time { return at }
	prepClock = func() time.Time { return at }
	t.Cleanup(func() {
		briefClock = oldBrief
		prepClock = oldPrep
	})
}

func runExamCLI(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), args, &stdout, &stderr, strings.NewReader("")); err != nil {
		t.Fatalf("Run(%v): %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func examDailyCLIPredictions(t *testing.T, output string) []exam.Prediction {
	t.Helper()
	var out []exam.Prediction
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "obligation: ") {
			if len(out) == 0 {
				t.Fatalf("daily CLI emitted obligation metadata before a cited item: %q", line)
			}
			fields := strings.Split(strings.TrimPrefix(trimmed, "obligation: "), " · ")
			if len(fields) != 5 {
				t.Fatalf("daily CLI emitted malformed obligation metadata: %q", line)
			}
			values := map[string]string{}
			for _, field := range fields {
				key, value, ok := strings.Cut(field, "=")
				if !ok || key == "" || value == "" {
					t.Fatalf("daily CLI emitted malformed obligation field %q", field)
				}
				values[key] = value
			}
			for _, key := range []string{"owner", "direction", "due", "lifecycle", "closure"} {
				if values[key] == "" {
					t.Fatalf("daily CLI obligation metadata omitted %q: %q", key, line)
				}
			}
			pred := &out[len(out)-1]
			pred.Text += line + "\n"
			pred.Owner = values["owner"]
			pred.Direction = values["direction"]
			pred.Due = values["due"]
			pred.Lifecycle = values["lifecycle"]
			pred.ClosureRef = values["closure"]
			continue
		}
		start := strings.LastIndex(line, "(id: ")
		if start < 0 || !strings.HasSuffix(line, ")") {
			continue
		}
		id := strings.TrimSpace(line[start+len("(id: ") : len(line)-1])
		if id == "" {
			t.Fatalf("daily CLI emitted an empty citation id in %q", line)
		}
		out = append(out, exam.Prediction{
			Surface:   exam.SurfaceDaily,
			Text:      strings.TrimSpace(line) + "\n",
			MemoryID:  id,
			Direction: exam.Unknown,
			Lifecycle: exam.Unknown,
		})
	}
	if len(out) == 0 {
		t.Fatalf("daily CLI emitted no cited items:\n%s", output)
	}
	return out
}

func decodeDigestPayload(t *testing.T, value any) Digest {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var digest Digest
	if err := json.Unmarshal(body, &digest); err != nil {
		t.Fatalf("decode MCP digest payload: %v\n%s", err, body)
	}
	return digest
}

func predictionIDs(preds []exam.Prediction) []string {
	out := make([]string, len(preds))
	for i, pred := range preds {
		out[i] = pred.MemoryID
	}
	return out
}

func scoreExamSurface(t *testing.T, ledger exam.Ledger, preds []exam.Prediction, surface string) exam.Scorecard {
	t.Helper()
	sc, err := exam.Score(ledger, preds, surface)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// TestExamSurfaces drives every currently shipped TRUTH-06 surface through its
// real dispatcher. Home is deliberately not a row: it does not exist yet
// (HOME-09/#141), and HTTP is a transport over the already-counted MCP engine.
func TestExamSurfaces(t *testing.T) {
	scorecards := runExamSurfaces(t, examFixtureRoot)
	assertGate3MeetingRatchet(t, scorecards)
	assertGate3DailyRatchet(t, scorecards)
	assertExamSurfaceScorecardsGolden(t, scorecards, examSurfaceScorecardsPath, *update)
}

// TestExamSurfacesV2 makes the human-validated realism corpus a measured
// product baseline. The golden comparison is the regression ratchet: product
// changes may move it only after the scorecard delta has been inspected.
func TestExamSurfacesV2(t *testing.T) {
	scorecards := runExamSurfaces(t, examFixtureV2Root)
	assertGate3MeetingRatchet(t, scorecards)
	assertGate3DailyRatchet(t, scorecards)
	assertExamSurfaceScorecardsGolden(t, scorecards, examSurfaceScorecardsV2Path, *updateV2)
}

// TestExamInventoryAdapterV3AcrossProductSurfaces uses the first corpus whose
// rendered vault preserves immutable message/block refs. It does not add v3 to
// TestExamProductTarget (W1b owns that gate expansion); it proves this adapter-only
// step makes the knowledge rows observable through daily/event x CLI/MCP now.
func TestExamInventoryAdapterV3AcrossProductSurfaces(t *testing.T) {
	scorecards := runExamSurfaces(t, examFixtureV3Root)
	for name, card := range map[string]exam.Scorecard{
		"daily_cli": scorecards.DailyCLI,
		"daily_mcp": scorecards.DailyMCP,
		"event_cli": scorecards.EventCLI,
		"event_mcp": scorecards.EventMCP,
	} {
		for metric, row := range map[string]exam.PR{
			"commitment_identity": card.CommitmentIdentity,
			"lifecycle":           card.Lifecycle,
			"closure_linkage":     card.ClosureLinkage,
			"citation_roles":      card.CitationRoles,
		} {
			if !row.Defined || row.Recall == 0 {
				t.Errorf("%s %s = %+v, want defined non-zero inventory recall", name, metric, row)
			}
		}
		// obligations-v3 has immutable refs but no labelled duplicate pair, so
		// Dedup correctly remains unscorable here. The unit adapter test above
		// separately proves DuplicateOf is copied without reinterpretation.
		if card.Dedup.Defined {
			t.Errorf("%s dedup = %+v, want unscorable with zero v3 duplicate gold samples", name, card.Dedup)
		}
	}
}

func assertGate3MeetingRatchet(t *testing.T, scorecards examSurfaceScorecards) {
	t.Helper()
	for name, card := range map[string]exam.Scorecard{
		"event_cli": scorecards.EventCLI, "event_mcp": scorecards.EventMCP,
	} {
		if !card.Lifecycle.Defined || card.Lifecycle.Precision != 1 || card.Lifecycle.Recall == 0 {
			t.Errorf("%s Lifecycle = %+v, want precise non-vacuous typed state", name, card.Lifecycle)
		}
		if !card.ClosureLinkage.Defined || card.ClosureLinkage.Precision != 1 || card.ClosureLinkage.Recall == 0 {
			t.Errorf("%s ClosureLinkage = %+v, want precise non-vacuous typed linkage", name, card.ClosureLinkage)
		}
		if card.ClosedLeaks != 0 || card.DupLeaks != 0 || card.DedupCrossArtifact != 0 {
			t.Errorf("%s lifecycle/dedup leaks: ClosedLeaks=%d DupLeaks=%d DedupCrossArtifact=%d",
				name, card.ClosedLeaks, card.DupLeaks, card.DedupCrossArtifact)
		}
	}
}

func assertGate3DailyRatchet(t *testing.T, scorecards examSurfaceScorecards) {
	t.Helper()
	for name, card := range map[string]exam.Scorecard{
		"daily_cli": scorecards.DailyCLI, "daily_mcp": scorecards.DailyMCP,
	} {
		for metric, row := range map[string]exam.PR{
			"Direction":      card.Direction,
			"DueTime":        card.DueTime,
			"Lifecycle":      card.Lifecycle,
			"ClosureLinkage": card.ClosureLinkage,
		} {
			if !row.Defined || row.Precision != 1 || row.Recall == 0 {
				t.Errorf("%s %s = %+v, want precise non-vacuous typed daily obligation contract", name, metric, row)
			}
		}
		if card.CitationCoverage.Precision != 1 || card.CitationCoverage.Recall != 1 ||
			card.CitationCorrect.Precision != 1 || card.CitationCorrect.Recall != 1 {
			t.Errorf("%s citation rows regressed: coverage=%+v correct=%+v",
				name, card.CitationCoverage, card.CitationCorrect)
		}
		if card.Unmatched != 0 {
			t.Errorf("%s Unmatched = %d, want 0", name, card.Unmatched)
		}
	}
}

func runExamSurfaces(t *testing.T, corpusRoot string) examSurfaceScorecards {
	t.Helper()
	cfg, event, at := seedExamHomeFromRoot(t, corpusRoot)
	pinExamSurfaceClocks(t, at)
	ledger := loadExamLedgerFromRoot(t, corpusRoot)
	snapshot, err := readCommitmentSnapshot(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	dailyCLIOutput := runExamCLI(t, "pulse", "--digest", "--since-hours", "720")
	dailyCLI := examDailyCLIPredictions(t, dailyCLIOutput)
	dailyCLI = append(dailyCLI, examInventoryPredictions(exam.SurfaceDaily, snapshot.Commitments...)...)
	dailyValue, err := callMCPTool(context.Background(), "digest", map[string]any{"since_hours": float64(examDailyWindowHours)})
	if err != nil {
		t.Fatal(err)
	}
	dailyMCPPayload := decodeDigestPayload(t, dailyValue)
	dailyMCP := examDailyPredictions(dailyMCPPayload, snapshot.Commitments...)
	// The shipped surfaces intentionally use different per-source caps: the human
	// CLI uses digestDefaultCap while MCP exposes a generous set and lets its byte
	// budget govern. Pin the relationship, not a false same-multiset premise:
	// applying the CLI cap to each MCP section must reproduce the CLI exactly.
	dailyMCPAtCLICap := dailyMCPPayload
	for i := range dailyMCPAtCLICap.Sections {
		if len(dailyMCPAtCLICap.Sections[i].Items) > digestDefaultCap {
			dailyMCPAtCLICap.Sections[i].Items = dailyMCPAtCLICap.Sections[i].Items[:digestDefaultCap]
		}
	}
	dailyMCPAtCLICapPredictions := examDailyPredictions(dailyMCPAtCLICap, snapshot.Commitments...)
	if got, want := predictionIDs(dailyCLI), predictionIDs(dailyMCPAtCLICapPredictions); !reflect.DeepEqual(got, want) {
		t.Fatalf("daily CLI is not the exact cap-%d projection of MCP:\n CLI=%v\n MCP capped=%v\n MCP full=%v",
			digestDefaultCap, got, want, predictionIDs(dailyMCP))
	}
	if !reflect.DeepEqual(dailyCLI, dailyMCPAtCLICapPredictions) {
		t.Fatalf("daily CLI and cap-%d MCP obligation projections differ:\n CLI=%+v\n MCP=%+v",
			digestDefaultCap, dailyCLI, dailyMCPAtCLICapPredictions)
	}

	asOf := at.Format(time.RFC3339)
	eventCLIBytes := []byte(runExamCLI(t, "brief", "--event-id", event.EventID, "--at", asOf, "--json"))
	var eventCLI MeetingBrief
	if err := json.Unmarshal(eventCLIBytes, &eventCLI); err != nil {
		t.Fatalf("decode event CLI brief: %v\n%s", err, eventCLIBytes)
	}
	eventValue, err := callMCPTool(context.Background(), "meeting_prep", map[string]any{"event_id": event.EventID, "at": asOf})
	if err != nil {
		t.Fatal(err)
	}
	eventMCP, ok := eventValue.(MeetingBrief)
	if !ok {
		t.Fatalf("meeting_prep returned %T, want MeetingBrief", eventValue)
	}
	eventCLIJSON, _ := json.Marshal(eventCLI)
	eventMCPJSON, _ := json.Marshal(eventMCP)
	if !bytes.Equal(eventCLIJSON, eventMCPJSON) {
		t.Fatalf("event CLI and MCP MeetingBrief differ:\n CLI=%s\n MCP=%s", eventCLIJSON, eventMCPJSON)
	}

	eventCLIPredictions := examMeetingPredictions(eventCLI, snapshot.Commitments...)
	eventMCPPredictions := examMeetingPredictions(eventMCP, snapshot.Commitments...)
	scorecards := examSurfaceScorecards{
		DailyCLI:            scoreExamSurface(t, ledger, dailyCLI, exam.SurfaceDaily),
		DailyMCP:            scoreExamSurface(t, ledger, dailyMCP, exam.SurfaceDaily),
		EventCLI:            scoreExamSurface(t, ledger, eventCLIPredictions, exam.SurfaceMeeting),
		EventMCP:            scoreExamSurface(t, ledger, eventMCPPredictions, exam.SurfaceMeeting),
		HomeState:           "MISSING — non-gating until HOME-09/#141",
		dailyCLIPredictions: dailyCLI,
		dailyMCPPredictions: dailyMCP,
		eventCLIPredictions: eventCLIPredictions,
		eventMCPPredictions: eventMCPPredictions,
	}
	if !reflect.DeepEqual(scorecards.EventCLI, scorecards.EventMCP) {
		t.Fatalf("event CLI and MCP scorecards differ:\n CLI=%+v\n MCP=%+v", scorecards.EventCLI, scorecards.EventMCP)
	}

	// Extra transport check, explicitly not a fourth product surface. Sending the
	// same non-nil arg map proves POST /meeting-prep maps to meeting_prep with the
	// same event_id/at values and that the tool is present in httpCallAllowed.
	if !httpCallAllowed["meeting_prep"] {
		t.Fatal("POST /meeting-prep targets a tool absent from httpCallAllowed")
	}
	requestBody, _ := json.Marshal(map[string]any{"event_id": event.EventID, "at": asOf})
	server := &httpServer{token: "exam-token", port: 7777}
	handler := server.hostGuard(server.auth(server.routes()))
	req := httptest.NewRequest(http.MethodPost, "/meeting-prep", bytes.NewReader(requestBody))
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Authorization", "Bearer exam-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /meeting-prep returned %d: %s", rec.Code, rec.Body.String())
	}
	var eventHTTP MeetingBrief
	if err := json.Unmarshal(rec.Body.Bytes(), &eventHTTP); err != nil {
		t.Fatal(err)
	}
	eventHTTPJSON, _ := json.Marshal(eventHTTP)
	if !bytes.Equal(eventMCPJSON, eventHTTPJSON) {
		t.Fatalf("HTTP transport changed the MCP payload:\n MCP=%s\nHTTP=%s", eventMCPJSON, eventHTTPJSON)
	}
	return scorecards
}

func assertExamSurfaceScorecardsGolden(t *testing.T, got examSurfaceScorecards, goldenPath string, updateGolden bool) {
	t.Helper()
	want, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.FromSlash(goldenPath)
	if updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read current-surface scorecard golden %s: %v", goldenPath, err)
	}
	if !bytes.Equal(committed, want) {
		t.Fatalf("current-surface scorecards drifted at %s; inspect the product change before updating the golden\n got:\n%s\nwant:\n%s", goldenPath, committed, want)
	}
}

// TestDailyBriefHasNoObligationContract pins the typed obligation lane shared by
// daily CLI and MCP. The historical name is kept so issue #154's known-red cannot
// disappear from the ratchet by renaming the test.
func TestDailyBriefHasNoObligationContract(t *testing.T) {
	const (
		wantRED = false
		issue   = "https://github.com/pyranthus-hq/mora/issues/154"
		expires = "2026-10-14"
	)
	item := reflect.TypeOf(DigestItem{})
	required := []string{"Owner", "Direction", "DueAt", "Lifecycle", "ClosureRef"}
	var present []string
	for _, field := range required {
		if _, ok := item.FieldByName(field); ok {
			present = append(present, field)
		}
	}
	laneExists := len(present) == len(required)
	if wantRED {
		if laneExists {
			t.Fatalf("FIXED: daily obligation lane now exists; flip wantRED:false and score it (issue %s)", issue)
		}
		t.Logf("known RED through %s: DigestItem obligation fields present=%v; tracked by %s", expires, present, issue)
		return
	}
	if !laneExists {
		t.Fatalf("daily obligation lane missing fields: have %v, want %v", present, required)
	}
}

// TestExamSurfaceClockGuard makes the production CLI seam structural. Replacing
// briefClock with time.Now inside cmdPulse must turn this named test red even on a
// UTC host whose current date happens to overlap the corpus.
func TestExamSurfaceClockGuard(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "mora.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var pulse *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "cmdPulse" {
			pulse = fn
			break
		}
	}
	if pulse == nil {
		t.Fatal("cmdPulse not found; the daily CLI surface vanished")
	}
	briefClockCalls := 0
	ast.Inspect(pulse.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "briefClock" {
			briefClockCalls++
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
			pkg, _ := selector.X.(*ast.Ident)
			if pkg != nil && pkg.Name == "time" && selector.Sel.Name == "Now" {
				t.Errorf("cmdPulse calls time.Now directly at %s; scored daily surfaces must use briefClock", fset.Position(call.Pos()))
			}
		}
		return true
	})
	if briefClockCalls != 1 {
		t.Fatalf("cmdPulse briefClock calls = %d, want exactly 1 captured clock", briefClockCalls)
	}
}

// TestExamServiceOnlyGateIsAssembled proves service-authored requests cannot cross
// the materialized-commitment eligibility gate.
func TestExamServiceOnlyGateIsAssembled(t *testing.T) {
	cfg, _, at := seedExamHome(t)
	pinExamSurfaceClocks(t, at)
	m, err := findMemory(cfg, "gmail_thread/exam-footer-negative")
	if err != nil {
		t.Fatal(err)
	}
	m.Title = "Action required"
	m.Text = "From: Example Updates <no-reply@example.invalid>\n\nCan you send the review access code?"
	m.Meta = map[string]any{
		"from":          []string{"no-reply@example.invalid"},
		"to":            []string{"alex@example.com"},
		"message_count": "1",
		"names": map[string]string{
			"alex@example.com":         "Alex Morgan",
			"no-reply@example.invalid": "Example Updates",
		},
	}
	var ids []string
	for i := 0; i < 3; i++ {
		item := m
		item.ID = fmt.Sprintf("gmail_thread/exam-service-tripwire-%d", i)
		item.ProviderID = fmt.Sprintf("exam-service-tripwire-%d", i)
		item.ContentHash = fmt.Sprintf("exam-service-tripwire-%d", i)
		item.CreatedAt = at.Add(-time.Duration(i+1) * time.Minute).Format(time.RFC3339)
		item.Meta["occurred_at"] = item.CreatedAt
		if err := writeMemory(cfg, item); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, item.ID)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	output := runExamCLI(t, "pulse", "--digest", "--since-hours", "720")
	for _, id := range ids {
		if strings.Contains(output, "(id: "+id+")") {
			t.Fatalf("service-only request crossed the commitment eligibility gate (%s):\n%s", id, output)
		}
	}
}

// TestExamIMessageNonCommitmentSpeakerLineIsSuppressed drives a named-speaker
// decision statement through the real event CLI. It is context, not a materialized
// commitment, and must not become an event obligation.
func TestExamIMessageSpeakerPrefixIsNotProductText(t *testing.T) {
	cfg, event, at := seedExamHome(t)
	pinExamSurfaceClocks(t, at)
	m, err := findMemory(cfg, "imessage_chat/exam-flywheel")
	if err != nil {
		t.Fatal(err)
	}
	m.Text = "## 2026-07-13\nMe: Thanks, I will meet you there.\nDana Ellis: The launch review decision is final."
	body, err := renderMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	runExamCLI(t, "merge", "confirm", "--handle", "+15550100137", "--email", "dana@example.net")
	brief := runExamEventCLI(t, event.EventID, at)
	for _, section := range brief.Sections {
		for _, line := range section.Lines {
			if line.Citation.MemoryID() == "imessage_chat/exam-flywheel" {
				t.Fatalf("non-commitment named-speaker line crossed event eligibility: %q", line.Text)
			}
		}
	}
}
