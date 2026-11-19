package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSpearheadFrictionAcceptance(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_AGENT_SESSION", "")
	t.Setenv("MORA_EMBEDDER", "")
	ctx := testCtx(t)
	run(t, "init")
	caseDir, control, unbound := t.TempDir(), t.TempDir(), t.TempDir()
	const scope = "project:nightshift-xp"
	run(t, "project", "bind", "--scope", scope, caseDir)
	run(t, "project", "bind", "--scope", "project:control", control)
	t.Chdir(caseDir)
	const symptom = "Wrong criterion on a spec waiting for approval"
	const question = "How do I fix a wrong criterion on a spec waiting for approval?"
	const reportJSON = `{"author":"codex","source":"spearhead-fixture","symptom":"Wrong criterion on a spec waiting for approval","expected":"The pending approval card matches the reviewed criteria","evidence":"card:fixture-inverted","at":"2026-11-13T00:06:31Z"}`
	call := func(ctx context.Context, input string, args ...string) ([]byte, error) {
		t.Helper()
		var out, stderr bytes.Buffer
		err := Run(ctx, args, &out, &stderr, strings.NewReader(input))
		if err != nil && stderr.Len() > 0 {
			err = fmt.Errorf("%w: %s", err, stderr.String())
		}
		return out.Bytes(), err
	}
	ok := func(ctx context.Context, input string, args ...string) []byte {
		t.Helper()
		got, err := call(ctx, input, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return got
	}
	read := func(raw []byte, value any) {
		t.Helper()
		if err := json.Unmarshal(raw, value); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
	checkRecall := func(ctx context.Context, want int) []frictionRecallRow {
		t.Helper()
		var result struct{ Results []frictionRecallRow }
		read(ok(ctx, "", "friction", "recall", question), &result)
		if len(result.Results) != want {
			t.Fatalf("recall count %d, want %d", len(result.Results), want)
		}
		return result.Results
	}
	run(t, "config", "mcp-write-policy", "readonly")
	if _, err := call(ctx, reportJSON, "friction", "log"); err == nil {
		t.Fatal("read-only capture succeeded")
	}
	run(t, "config", "mcp-write-policy", "propose")
	var pending struct{ Proposal struct{ ID, Status string } }
	read(ok(ctx, reportJSON, "friction", "log"), &pending)
	if pending.Proposal.ID == "" || pending.Proposal.Status != "pending" {
		t.Fatalf("bad pending receipt: %+v", pending)
	}
	checkRecall(ctx, 0)
	ok(ctx, "", "mcp", "proposals", "show", pending.Proposal.ID)
	ok(ctx, "", "mcp", "proposals", "approve", pending.Proposal.ID)
	report := checkRecall(ctx, 1)[0]
	if report.Report.Symptom != symptom || report.Report.Author != "codex" || report.Report.Expected == "" || !report.DefectOpen {
		t.Fatalf("incorrect saved report: %+v", report)
	}
	run(t, "config", "mcp-write-policy", "tentative")
	recovery := fmt.Sprintf(`{"report_id":%q,"author":"codex","source":"spearhead-fixture","at":"2026-11-13T00:07:00Z","evidence":"card:fixture-compared","summary":"Relaunch restored a reviewable draft. The new card matched the reviewed text.","outcome":"worked","applies_when":"The pending card cannot be edited.","run_unblocked":true,"defect_open":true}`, report.ID)
	var receipt struct {
		Memory     Memory
		Searchable bool
	}
	read(ok(ctx, recovery, "friction", "recover"), &receipt)
	if receipt.Memory.Scope != scope || !receipt.Searchable {
		t.Fatalf("expected bound searchable recovery: %+v", receipt)
	}
	fresh := testCtx(t)
	row := checkRecall(fresh, 1)[0]
	if row.ID != report.ID || len(row.Recoveries) != 1 || row.Recoveries[0].ID != receipt.Memory.ID ||
		row.Recoveries[0].Evidence != "card:fixture-compared" || !*row.Recoveries[0].RunUnblocked || !row.DefectOpen {
		t.Fatalf("recovery or defect status missing: %+v", row)
	}
	t.Chdir(control)
	checkRecall(fresh, 0)
	if _, err := call(fresh, "", "friction", "recall", "--scope", scope, question); err == nil {
		t.Fatal("bound control could override its scope")
	}
	if _, err := call(fresh, recovery, "friction", "recover"); err == nil {
		t.Fatal("control attached a recovery to a different project's report")
	}
	t.Chdir(unbound)
	if _, err := call(fresh, reportJSON, "friction", "log"); err == nil {
		t.Fatal("unbound capture fell back to global")
	}
	t.Chdir(control)
	originalList := listRebuildFiles
	func() {
		defer func() { listRebuildFiles = originalList }()
		listRebuildFiles = func(cfg Config) ([]string, error) {
			raw := ok(fresh, "", "friction", "recall", question)
			if !bytes.Contains(raw, []byte(`"results":[]`)) || !bytes.Contains(raw, []byte(`"index":"refreshing"`)) {
				t.Fatalf("wrong-project recall during rebuild: %s", raw)
			}
			return originalList(cfg)
		}
		if _, err := rebuildIndex(fresh, mustConfig(t)); err != nil {
			t.Fatal(err)
		}
	}()
	t.Chdir(caseDir)
	checkRecall(fresh, 1)
	if _, err := os.Stat(receipt.Memory.Path); err != nil {
		t.Fatal(err)
	}
}

func TestSpearheadReviewedDronaImport(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_AGENT_SESSION", "")
	ctx := testCtx(t)
	run(t, "init")
	dir := t.TempDir()
	const scope = "project:nightshift-xp"
	run(t, "project", "bind", "--scope", scope, dir)
	t.Chdir(dir)
	const symptom = "NightShift Inbox cannot edit a pending spec"
	const sourceID = "feedback-0000000000000001"
	report := frictionRecord{SourceID: sourceID, Author: "agent", Source: "drona",
		Evidence: "drona:" + sourceID, At: "2026-11-13T00:06:31Z", Symptom: symptom,
		Expected: "Correct the acceptance criterion before approving the exact revised spec."}
	packet := frictionPacket{Scope: scope, Report: report}
	type entry struct {
		Status     string
		ProposalID string `json:"proposal_id"`
		MemoryID   string `json:"memory_id"`
	}
	stage := func() []entry {
		t.Helper()
		raw, err := json.Marshal(packet)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := Run(ctx, []string{"friction", "import"}, &out, &out, bytes.NewReader(raw)); err != nil {
			t.Fatalf("import: %v (%s)", err, out.String())
		}
		var result struct{ Records []entry }
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Records
	}
	recall := func() []frictionRecallRow {
		t.Helper()
		var result struct{ Results []frictionRecallRow }
		if err := json.Unmarshal([]byte(run(t, "friction", "recall", symptom)), &result); err != nil {
			t.Fatal(err)
		}
		return result.Results
	}
	first := stage()
	if len(first) != 1 || first[0].Status != "pending" || len(recall()) != 0 {
		t.Fatalf("import was not held for review: %+v", first)
	}
	if second := stage(); !reflect.DeepEqual(second, first) {
		t.Fatalf("staging twice duplicated or changed the proposal: %+v %+v", first, second)
	}
	run(t, "mcp", "proposals", "show", first[0].ProposalID)
	run(t, "mcp", "proposals", "approve", first[0].ProposalID)
	if got := recall(); len(got) != 1 || len(got[0].Recoveries) != 0 || !got[0].DefectOpen {
		t.Fatalf("unrecovered source should stay open: %+v", got)
	}
	// This source has no locally available linked recovery. A synthetic later
	// recovery exercises the append and replay behavior without inventing source history.
	yes := true
	packet.Recoveries = []frictionRecord{{
		SourceID: "recovery-000000000000000000000001", Author: "agent", Source: "drona",
		At: "2026-11-13T00:08:00Z", Evidence: "card:synthetic", Outcome: "worked",
		Summary: "Relaunch restored the approval card.", AppliesWhen: "The pending card cannot be edited.",
		RunUnblocked: &yes, DefectOpen: &yes,
	}}
	later := stage()
	if len(later) != 2 || later[0].Status != "already_saved" || later[1].Status != "pending" {
		t.Fatalf("later recovery did not preserve the existing report: %+v", later)
	}
	run(t, "mcp", "proposals", "show", later[1].ProposalID)
	run(t, "mcp", "proposals", "approve", later[1].ProposalID)
	for range 2 {
		again := stage()
		if len(again) != 2 || again[0].Status != "already_saved" || again[1].Status != "already_saved" ||
			again[0].MemoryID != first[0].MemoryID || again[1].MemoryID != later[1].MemoryID {
			t.Fatalf("replay changed report or recovery identity: %+v", again)
		}
	}
	if got := recall(); len(got) != 1 || len(got[0].Recoveries) != 1 || !got[0].DefectOpen ||
		got[0].Report.SourceID != sourceID || got[0].Report.At != report.At || got[0].Recoveries[0].SourceID != packet.Recoveries[0].SourceID {
		t.Fatalf("lost provenance, duplicated records or changed state: %+v", got)
	}
}
