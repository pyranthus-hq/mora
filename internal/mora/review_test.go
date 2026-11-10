package mora

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// reviewSeed makes the shipped policy save a plain fact as tentative and hold a decision.
func reviewSaved(t *testing.T, title string) string {
	t.Helper()
	return tentativeMemoryID(t, tentativeServe(t, map[string]any{
		"title": title, "text": "Body of " + title + ".", "type": "fact", "scope": "project:demo",
	}))
}

func reviewHeld(t *testing.T, title string) string {
	t.Helper()
	res := tentativeServe(t, map[string]any{"title": title, "text": "Body of " + title + ".", "type": "decision", "scope": "project:demo"})
	p, ok := res["proposal"].(map[string]any)
	if !ok {
		t.Fatalf("write was not held: %v", res)
	}
	return p["id"].(string)
}

func reviewTerminal(t *testing.T) {
	t.Helper()
	clearAgentEnv(t)
	prev := rulesInteractive
	rulesInteractive = func(io.Reader) bool { return true }
	t.Cleanup(func() { rulesInteractive = prev })
}

func reviewWalk(t *testing.T, stdin io.Reader) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(testCtx(t), []string{"review"}, &out, &out, stdin)
	return out.String(), err
}

func reviewTentative(t *testing.T, cfg Config, id string) bool {
	t.Helper()
	m, err := findMemory(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	return memory.IsTentative(m)
}

func reviewSetup(t *testing.T) Config {
	t.Helper()
	useShippedWritePolicyDefault(t)
	withTempHome(t)
	run(t, "init")
	return mustConfig(t)
}

func TestReviewListShowsSavedThenHeldOnly(t *testing.T) {
	cfg := reviewSetup(t)
	empty := run(t, "review", "--list")
	if strings.TrimSpace(empty) != "No agent notes wait for review." {
		t.Fatalf("empty list = %q", empty)
	}
	confirmed := reviewSaved(t, "Confirmed already")
	run(t, "confirm", confirmed)
	saved := reviewSaved(t, "Saved note")
	held := reviewHeld(t, "Held note")
	decided := reviewHeld(t, "Decided note")
	run(t, "mcp", "proposals", "reject", decided)

	out := run(t, "review", "--list")
	want := saved + "\tsaved\tproject:demo\tfact\tSaved note\n" + held + "\theld\tproject:demo\tdecision\tHeld note\n"
	if out != want {
		t.Fatalf("list =\n%q\nwant\n%q", out, want)
	}
	var env struct {
		Schema string      `json:"schema"`
		Notes  []reviewRow `json:"notes"`
	}
	if err := json.Unmarshal([]byte(run(t, "review", "--list", "--json")), &env); err != nil {
		t.Fatal(err)
	}
	if env.Schema != "mora.review" || len(env.Notes) != 2 || env.Notes[0].Kind != "saved" || env.Notes[1].Kind != "held" {
		t.Fatalf("json = %+v", env)
	}
	_ = cfg
}

func TestReviewWalkKeepsRejectsAndSkips(t *testing.T) {
	cfg := reviewSetup(t)
	keepSaved := reviewSaved(t, "Keep saved")
	rejectSaved := reviewSaved(t, "Reject saved")
	skipSaved := reviewSaved(t, "Skip saved")
	keepHeld := reviewHeld(t, "Keep held")
	rejectHeld := reviewHeld(t, "Reject held")
	reviewTerminal(t)

	// Order is saved first, then held; ids sort inside each kind, so answer by reading the list.
	order := strings.Split(strings.TrimSpace(run(t, "review", "--list")), "\n")
	title := func(row string) string { return strings.Split(row, "\t")[4] }
	answers := map[string]string{"Keep saved": "K", "Reject saved": "reject", "Skip saved": "s", "Keep held": "keep", "Reject held": "r"}
	var script strings.Builder
	for _, row := range order {
		script.WriteString("bogus\n" + answers[title(row)] + "\n")
	}
	out, err := reviewWalk(t, strings.NewReader(script.String()))
	if err != nil {
		t.Fatalf("walk: %v\n%s", err, out)
	}
	if !strings.HasSuffix(out, "kept 2, rejected 2, skipped 1, left 0\n") {
		t.Fatalf("last line wrong:\n%s", out)
	}
	if !strings.Contains(out, "held because: type decision") {
		t.Fatalf("hold reasons not shown:\n%s", out)
	}
	if reviewTentative(t, cfg, keepSaved) {
		t.Fatal("kept saved note is still tentative")
	}
	if _, err := findMemory(cfg, rejectSaved); err == nil {
		t.Fatal("rejected saved note still exists")
	}
	if !reviewTentative(t, cfg, skipSaved) {
		t.Fatal("skipped note changed")
	}
	for id, want := range map[string]string{keepHeld: "approved", rejectHeld: "rejected"} {
		d, err := readMCPProposalDecision(cfg, id)
		if err != nil || d == nil || d.Status != want {
			t.Fatalf("proposal %s decision = %+v, %v; want %s", id, d, err, want)
		}
		if (want == "approved") != (d.MemoryID != "") {
			t.Fatalf("proposal %s memory id %q", id, d.MemoryID)
		}
		if want == "approved" {
			if _, err := findMemory(cfg, d.MemoryID); err != nil {
				t.Fatalf("approved memory missing: %v", err)
			}
		}
	}
	// Quit and end of input leave the skipped note where it is.
	for _, in := range []string{"q\n", ""} {
		if out, err := reviewWalk(t, strings.NewReader(in)); err != nil || !strings.HasSuffix(out, "kept 0, rejected 0, skipped 0, left 1\n") {
			t.Fatalf("input %q: %v\n%s", in, err, out)
		}
	}
}

func TestReviewWalkRefusesOutsideOwnTerminal(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "Stay put")
	clearAgentEnv(t)

	_, err := reviewWalk(t, strings.NewReader("k\n"))
	if err == nil || !strings.Contains(err.Error(), "mora review") || !strings.Contains(err.Error(), "non-interactive") {
		t.Fatalf("non-terminal: %v", err)
	}
	reviewTerminal(t)
	t.Setenv("CLAUDECODE", "1")
	_, err = reviewWalk(t, strings.NewReader("k\n"))
	if err == nil || !strings.Contains(err.Error(), "CLAUDECODE") || !strings.Contains(err.Error(), "mora review") {
		t.Fatalf("agent session: %v", err)
	}
	if !reviewTentative(t, cfg, id) {
		t.Fatal("a refused walk changed a note")
	}
	// --list still works there.
	if out := run(t, "review", "--list"); !strings.Contains(out, id) {
		t.Fatalf("list in agent session = %q", out)
	}
}

func TestReviewWalkEscapesNoteText(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "Plain title")
	m, err := findMemory(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(m.Path)
	evil := "\x1b[2Jfake\rmore \u202egnp.exe"
	if err := os.WriteFile(m.Path, bytes.Replace(raw, []byte("Body of Plain title."), []byte(evil), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewTerminal(t)
	out, err := reviewWalk(t, strings.NewReader("q\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"\x1b", "\r", "\u202e"} {
		if strings.Contains(out, bad) {
			t.Fatalf("raw %q reached the screen:\n%q", bad, out)
		}
	}
	if !strings.Contains(out, `\x1b[2J`) || !strings.Contains(out, `\r`) || !strings.Contains(out, `\u202e`) {
		t.Fatalf("escapes missing:\n%q", out)
	}
	// The list escapes the title the same way.
	if strings.ContainsAny(run(t, "review", "--list"), "\x1b\r\u202e") {
		t.Fatal("raw control rune in --list")
	}
}

// changeOnRead runs mutate when the walk first reads its answer, which is after the note was printed.
type changeOnRead struct {
	mutate func()
	data   *strings.Reader
}

func (r *changeOnRead) Read(p []byte) (int, error) {
	if r.mutate != nil {
		r.mutate()
		r.mutate = nil
	}
	return r.data.Read(p)
}

func TestReviewWalkRefusesAChangedNote(t *testing.T) {
	cfg := reviewSetup(t)
	saved := reviewSaved(t, "Shifty saved")
	held := reviewHeld(t, "Shifty held")
	edit := func(path, from, to string) func() {
		return func() {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(path, bytes.Replace(raw, []byte(from), []byte(to), 1), 0o644); err != nil {
				t.Error(err)
			}
		}
	}
	sm, err := findMemory(cfg, saved)
	if err != nil {
		t.Fatal(err)
	}
	reviewTerminal(t)

	out, err := reviewWalk(t, &changeOnRead{mutate: edit(sm.Path, "Body of Shifty saved.", "Changed body."), data: strings.NewReader("k\nq\n")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "changed after it was shown") || !strings.Contains(out, "Changed body.") || !reviewTentative(t, cfg, saved) {
		t.Fatalf("saved note was kept after it changed:\n%s", out)
	}

	pp := filepath.Join(mcpProposalDir(cfg), held+".json")
	out, err = reviewWalk(t, &changeOnRead{mutate: edit(pp, "Body of Shifty held.", "Changed held."), data: strings.NewReader("s\nr\nq\n")})
	if err != nil {
		t.Fatal(err)
	}
	// The saved note is first: skip it, then reject the held note, which changed after it was shown.
	if !strings.Contains(out, "changed after it was shown") && !strings.Contains(out, "Changed held.") {
		t.Fatalf("held note change not caught:\n%s", out)
	}
	if d, _ := readMCPProposalDecision(cfg, held); d != nil {
		t.Fatalf("held note decided after it changed: %+v", d)
	}
}

const reviewHostile = "\x1b[2Jfake\rline \u202egnp"

func reviewSetTitle(t *testing.T, cfg Config, id, title string) {
	t.Helper()
	m, err := findMemory(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(m.Path)
	var out []string
	done := false
	for _, line := range strings.Split(string(raw), "\n") {
		if !done && strings.HasPrefix(line, "title:") {
			line, done = "title: "+strconv.Quote(title), true
		}
		out = append(out, line)
	}
	if !done {
		t.Fatalf("no title line in\n%s", raw)
	}
	if err := os.WriteFile(m.Path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func reviewHostileHeld(t *testing.T, cfg Config, id string) string {
	t.Helper()
	p, _, err := readMCPWriteProposal(cfg, reviewHeld(t, "Hostile held"))
	if err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(mcpProposalDir(cfg), p.ID+".json")
	p.ID = id
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestReviewEscapesEveryPrintedLine(t *testing.T) {
	cfg := reviewSetup(t)
	reviewTerminal(t)
	raw := func(s string) bool { return strings.ContainsAny(s, "\x1b\r\u202e") }

	keepSaved := reviewSaved(t, "Keep me")
	rejectSaved := reviewSaved(t, "Reject me")
	reviewSetTitle(t, cfg, keepSaved, "K"+reviewHostile)
	reviewSetTitle(t, cfg, rejectSaved, "R"+reviewHostile)
	hostileID := reviewHostileHeld(t, cfg, "p_"+reviewHostile)

	if list := run(t, "review", "--list"); raw(list) || strings.Count(list, `\x1b`) < 3 {
		t.Fatalf("--list not escaped:\n%q", list)
	}
	order := strings.Split(strings.TrimSpace(run(t, "review", "--list")), "\n")
	var script strings.Builder
	for _, row := range order {
		switch {
		case strings.Contains(row, "\tsaved\t") && strings.Contains(row, `"K\x1b`):
			script.WriteString("k\n")
		case strings.Contains(row, "\tsaved\t"):
			script.WriteString("r\n")
		default:
			script.WriteString("k\n")
		}
	}
	_ = hostileID
	out, err := reviewWalk(t, strings.NewReader(script.String()))
	if err != nil {
		t.Fatal(err)
	}
	if raw(out) {
		t.Fatalf("raw control rune reached the screen:\n%q", out)
	}
	for _, want := range []string{"confirmed ", "undid "} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q line:\n%s", want, out)
		}
	}
	// Whatever the hostile held note's keep did (approve or error), it is escaped and shows its id.
	if !strings.Contains(out, `"p_\x1b[2Jfake\rline \u202egnp"`) {
		t.Fatalf("hostile proposal id not shown escaped:\n%s", out)
	}
}

func TestReviewRejectLineAndErrorLineAreEscaped(t *testing.T) {
	cfg := reviewSetup(t)
	reviewTerminal(t)
	id := reviewHostileHeld(t, cfg, "p_"+reviewHostile)
	out, err := reviewWalk(t, strings.NewReader("r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\x1b\r\u202e") || !strings.Contains(out, `Rejected "p_\x1b[2J`) {
		t.Fatalf("reject line:\n%q", out)
	}
	_ = id

	// An error line: the proposal vanishes after it was shown.
	reviewHostileHeld(t, cfg, "p_2"+reviewHostile)
	gone := filepath.Join(mcpProposalDir(cfg), "p_2"+reviewHostile+".json")
	rm := &changeOnRead{mutate: func() { _ = os.Remove(gone) }, data: strings.NewReader("k\n")}
	out, err = reviewWalk(t, rm)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\x1b\r\u202e") || !strings.Contains(out, "error: ") || !strings.HasSuffix(out, "left 1\n") {
		t.Fatalf("error line:\n%q", out)
	}
}

func TestReviewSavedActionsBindToTheShownBytes(t *testing.T) {
	cfg := reviewSetup(t)
	keepID := reviewSaved(t, "Bind keep")
	rejectID := reviewSaved(t, "Bind reject")
	shown := map[string]reviewNote{}
	for _, id := range []string{keepID, rejectID} {
		n, err := loadReviewNote(cfg, reviewKindSaved, id)
		if err != nil {
			t.Fatal(err)
		}
		shown[id] = n
		reviewSetTitle(t, cfg, id, "swapped after it was shown")
	}
	if _, _, err := confirmMemory(testCtx(t), cfg, keepID, shown[keepID].SHA, io.Discard); err == nil || !strings.Contains(err.Error(), "changed after") {
		t.Fatalf("confirm acted on a swapped note: %v", err)
	}
	if !reviewTentative(t, cfg, keepID) {
		t.Fatal("swapped note lost its tag")
	}
	m, err := findMemory(cfg, rejectID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := undoMemory(testCtx(t), cfg, m, false, shown[rejectID].SHA, io.Discard); err == nil || !strings.Contains(err.Error(), "changed after") {
		t.Fatalf("undo acted on a swapped note: %v", err)
	}
	if _, err := os.Stat(m.Path); err != nil {
		t.Fatalf("swapped note was deleted: %v", err)
	}
}

func TestReviewRejectDoesNotDeleteANoteConfirmedElsewhere(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "Confirmed elsewhere")
	reviewTerminal(t)
	in := &changeOnRead{mutate: func() { run(t, "confirm", id) }, data: strings.NewReader("r\n")}
	out, err := reviewWalk(t, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := findMemory(cfg, id); err != nil {
		t.Fatalf("a note confirmed after the print was deleted: %v\n%s", err, out)
	}
	if reviewTentative(t, cfg, id) || strings.Contains(out, "undid ") {
		t.Fatalf("unexpected state:\n%s", out)
	}
}

func TestReviewTitlesAreComparedAndShownInFull(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "Long")
	long := strings.Repeat("a", 130)
	reviewSetTitle(t, cfg, id, long+"X")
	reviewTerminal(t)
	in := &changeOnRead{mutate: func() { reviewSetTitle(t, cfg, id, long+"Y") }, data: strings.NewReader("k\nq\n")}
	out, err := reviewWalk(t, in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "changed after it was shown") || !reviewTentative(t, cfg, id) {
		t.Fatalf("a change after rune 120 was not caught:\n%s", out)
	}
	if !strings.Contains(out, "title: "+long+"X") || !strings.Contains(out, "title: "+long+"Y") {
		t.Fatalf("full title not printed:\n%s", out)
	}
}
