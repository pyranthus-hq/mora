package mora

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

const ownerTrustJSON = `"trust":"owner-confirmed"`

// ownerCall runs one MCP tool through the real server and returns its
// structured result as JSON.
func ownerCall(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(args)
	res := mcpResult(t, budgetCall(tool, string(raw)))
	if res["isError"] == true {
		t.Fatalf("%s errored: %v", tool, res)
	}
	sc, _ := json.Marshal(res["structuredContent"])
	return string(sc)
}

// ownerSaved saves a multi-line tentative note, so a surface that flattened
// the text before deriving trust would lose the confirmation.
func ownerSaved(t *testing.T, title string) string {
	t.Helper()
	return tentativeMemoryID(t, tentativeServe(t, map[string]any{
		"title": title, "text": "First line of " + title + ".\n\n  Second  line.", "type": "fact", "scope": "project:demo",
	}))
}

// ownerHeldDecision submits a decision that the tentative rule still holds
// (the text names a sensitive word) and returns the proposal id.
func ownerHeldDecision(t *testing.T, title string) string {
	t.Helper()
	res := tentativeServe(t, map[string]any{"title": title, "text": "Body of " + title + ". Passport renewal.", "type": "decision", "scope": "project:demo"})
	p, ok := res["proposal"].(map[string]any)
	if !ok {
		t.Fatalf("write was not held: %v", res)
	}
	return p["id"].(string)
}

func ownerKeepAll(t *testing.T, n int) string {
	t.Helper()
	reviewTerminal(t)
	out, err := reviewWalk(t, strings.NewReader(strings.Repeat("k\n", n)))
	if err != nil {
		t.Fatalf("walk: %v\n%s", err, out)
	}
	return out
}

func ownerMemory(t *testing.T, cfg Config, id string) Memory {
	t.Helper()
	m, err := findMemory(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// ownerAddTag appends tag to the frontmatter tags line of the file at path.
func ownerAddTag(t *testing.T, path, tag string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if inner, ok := strings.CutPrefix(line, "tags: ["); ok {
			inner = strings.TrimSuffix(inner, "]")
			if inner != "" {
				inner += ", "
			}
			lines[i] = "tags: [" + inner + tag + "]"
			break
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ownerTag(m Memory) string {
	return memory.OwnerConfirmedTagPrefix + memory.OwnerConfirmedDigest(m)
}

func TestOwnerConfirmWalkKeepsASavedNote(t *testing.T) {
	cfg := reviewSetup(t)
	id := ownerSaved(t, "Heron owner")
	out := ownerKeepAll(t, 1)
	if !strings.Contains(out, "owner-confirmed "+id) {
		t.Fatalf("walk did not report the keep:\n%s", out)
	}
	m := ownerMemory(t, cfg, id)
	if memory.IsTentative(m) || !memory.IsOwnerConfirmed(m) {
		t.Fatalf("tags after keep = %v", m.Tags)
	}
	raw, _ := os.ReadFile(m.Path)
	if !strings.Contains(string(raw), "tags: ["+ownerTag(m)+"]\n") {
		t.Fatalf("tag not swapped in place:\n%s", raw)
	}
	for tool, args := range map[string]map[string]any{
		"read_memory":   {"id": id},
		"search_memory": {"query": "heron owner"},
		"list_memory":   {},
	} {
		if got := ownerCall(t, tool, args); !strings.Contains(got, ownerTrustJSON) {
			t.Fatalf("%s has no owner-confirmed row:\n%s", tool, got)
		}
	}
	if got := ownerCall(t, "context_memory", map[string]any{"query": "heron owner"}); !strings.Contains(got, "the owner then confirmed this exact text in their own terminal") {
		t.Fatalf("context_memory has no confirmed provenance line:\n%s", got)
	}
	var read struct{ Trust string }
	if err := json.Unmarshal([]byte(run(t, "read", id, "--json")), &read); err != nil || read.Trust != memory.TrustOwnerConfirmed {
		t.Fatalf("read --json trust = %q, %v", read.Trust, err)
	}
	log := readAgentWriteLog(t, cfg)
	if !strings.Contains(log, " owner-confirm "+id+" ") || strings.Contains(log, " confirm "+id+" ") {
		t.Fatalf("log does not record an owner-confirm:\n%s", log)
	}
	run(t, "index", "rebuild")
	var ents entitiesPayload
	if err := json.Unmarshal([]byte(run(t, "entities", "--json")), &ents); err != nil {
		t.Fatal(err)
	}
	for _, e := range ents.Entities {
		if strings.HasPrefix(e.Name, memory.OwnerConfirmedTagPrefix) {
			t.Fatalf("the confirmed tag became an entity: %+v", e)
		}
	}
}

func TestOwnerConfirmWalkKeepsAHeldNote(t *testing.T) {
	cfg := reviewSetup(t)
	pid := ownerHeldDecision(t, "Held tabs")
	// No as_of was given, so the walk names the default rather than a time.
	out := ownerKeepAll(t, 1)
	for _, want := range []string{"decision as of: the time you keep it\n", "created at: the time you keep it\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q not named:\n%s", want, out)
		}
	}
	d, err := readMCPProposalDecision(cfg, pid)
	if err != nil || d == nil || d.Status != "approved" {
		t.Fatalf("decision = %+v, %v", d, err)
	}
	if !memory.IsOwnerConfirmed(ownerMemory(t, cfg, d.MemoryID)) {
		t.Fatal("approved held note is not owner-confirmed")
	}
	if got := ownerCall(t, "read_memory", map[string]any{"id": d.MemoryID}); !strings.Contains(got, ownerTrustJSON) {
		t.Fatalf("read_memory:\n%s", got)
	}
	if got := ownerCall(t, "context_memory", map[string]any{"query": "held tabs"}); !strings.Contains(got, "Adopted by user: the owner confirmed this exact text") {
		t.Fatalf("decision adoption line missing:\n%s", got)
	}
	log := readAgentWriteLog(t, cfg)
	if !strings.Contains(log, " write "+d.MemoryID+" ") || !strings.Contains(log, " owner-confirm "+d.MemoryID+" ") {
		t.Fatalf("log:\n%s", log)
	}
}

// Every other way to accept or write a note leaves it without the trust value,
// even when the agent passes the exact tag the walk would have written.
func TestOnlyTheWalkKeepMakesANoteOwnerConfirmed(t *testing.T) {
	forged := func(title, typ string) map[string]any {
		text := "Body of " + title + "."
		if title == "Held forged note" {
			text += " Passport renewal." // held under the tentative rule
		}
		args := map[string]any{"title": title, "text": text, "type": typ, "scope": "project:demo"}
		m, err := mcpMemoryFromArgs(args, mcpWriteClock())
		if err != nil {
			t.Fatal(err)
		}
		args["tags"] = []any{"keep", ownerTag(m)}
		return args
	}
	cases := []struct {
		name   string
		title  string
		policy string
		typ    string
		accept func(t *testing.T, cfg Config, res map[string]any) string
	}{
		{"tentative then mora confirm", "", "", "fact", func(t *testing.T, cfg Config, res map[string]any) string {
			id := tentativeMemoryID(t, res)
			run(t, "confirm", id)
			return id
		}},
		{"tentative held then proposals approve", "Held forged note", "", "decision", func(t *testing.T, cfg Config, res map[string]any) string {
			p, ok := res["proposal"].(map[string]any)
			if !ok {
				t.Fatalf("write was not held: %v", res)
			}
			pid := p["id"].(string)
			run(t, "mcp", "proposals", "approve", pid)
			d, _ := readMCPProposalDecision(cfg, pid)
			return d.MemoryID
		}},
		{"propose then proposals approve", "", "propose", "decision", func(t *testing.T, cfg Config, res map[string]any) string {
			pid := res["proposal"].(map[string]any)["id"].(string)
			run(t, "mcp", "proposals", "approve", pid)
			d, _ := readMCPProposalDecision(cfg, pid)
			return d.MemoryID
		}},
		{"propose then popup approve", "", "propose", "decision", func(t *testing.T, cfg Config, res map[string]any) string {
			pid := res["proposal"].(map[string]any)["id"].(string)
			p, _, err := readMCPWriteProposal(cfg, pid)
			if err != nil {
				t.Fatal(err)
			}
			digest, _ := mcpProposalArgumentsSHA256(p.Arguments)
			id, err := decideMCPWriteProposalVia(testCtx(t), cfg, pid, "approve", "popup", digest)
			if err != nil {
				t.Fatal(err)
			}
			return id
		}},
		{"open write", "", "open", "decision", func(t *testing.T, cfg Config, res map[string]any) string {
			return res["memory"].(map[string]any)["id"].(string)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := reviewSetup(t)
			if tc.policy != "" {
				run(t, "config", "mcp-write-policy", tc.policy)
			}
			title := tc.title
			if title == "" {
				title = "Forged note"
			}
			id := tc.accept(t, cfg, tentativeServe(t, forged(title, tc.typ)))
			m := ownerMemory(t, cfg, id)
			for _, tag := range m.Tags {
				if strings.HasPrefix(tag, memory.OwnerConfirmedTagPrefix) {
					t.Fatalf("an owner-confirmed tag reached the vault: %v", m.Tags)
				}
			}
			if got := ownerCall(t, "read_memory", map[string]any{"id": id}); strings.Contains(got, ownerTrustJSON) {
				t.Fatalf("not kept in the walk, yet owner-confirmed:\n%s", got)
			}
			if strings.Contains(readAgentWriteLog(t, cfg), " owner-confirm ") {
				t.Fatal("an owner-confirm line was logged")
			}
		})
	}
}

// A tag that does not match the record as it is now, or sits on a record that
// is not the owner's own untentative agent memory, gives no trust value.
func TestOwnerConfirmedTagIsInertAfterForgeryOrDrift(t *testing.T) {
	cfg := reviewSetup(t)
	var kept []string
	for _, title := range []string{"Drift edit", "Drift correct", "Drift supersede"} {
		kept = append(kept, ownerSaved(t, title))
	}
	ownerKeepAll(t, len(kept))
	edited, corrected, superseded := kept[0], kept[1], kept[2]
	stolen := ownerTag(ownerMemory(t, cfg, edited))

	// The same tag pasted on a memory with different text. The write is an
	// ordinary one: in the owner terminal `mora write` would confirm it itself.
	var other bytes.Buffer
	outsideOwnerTerminal(t, func() {
		if err := Run(testCtx(t), []string{"write", "--scope", "project:demo", "--type", "fact", "--title", "Drift edit", "--text", "Different text.", "--json"}, &other, &other, nil); err != nil {
			t.Fatal(err)
		}
	})
	var w struct{ ID, Path string }
	if err := json.Unmarshal(other.Bytes(), &w); err != nil {
		t.Fatal(err)
	}
	ownerAddTag(t, w.Path, stolen)

	// A confirmed memory whose file text is edited afterwards.
	em := ownerMemory(t, cfg, edited)
	raw, _ := os.ReadFile(em.Path)
	if err := os.WriteFile(em.Path, bytes.Replace(raw, []byte("Second  line."), []byte("Second line, edited."), 1), 0o644); err != nil {
		t.Fatal(err)
	}

	// Teach replacements that keep the confirmed title and text word for word.
	revise := func(decision, id string) string {
		m := ownerMemory(t, cfg, id)
		var out bytes.Buffer
		if err := Run(testCtx(t), []string{"teach", "memory", decision, "--id", id, "--title", m.Title, "--text", m.Text, "--yes", "--json"}, &out, &out, nil); err != nil {
			t.Fatalf("teach %s: %v\n%s", decision, err, out.String())
		}
		var r struct {
			Replacement struct{ ID string } `json:"replacement"`
		}
		if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.Replacement.ID == "" {
			t.Fatalf("teach %s receipt: %v\n%s", decision, err, out.String())
		}
		return r.Replacement.ID
	}
	correctedBy := revise("correct", corrected)
	supersededBy := revise("supersede", superseded)

	// A tentative record that also carries a tag matching its own text.
	tent := ownerSaved(t, "Drift tentative")
	tm := ownerMemory(t, cfg, tent)
	ownerAddTag(t, tm.Path, ownerTag(tm))

	// A connector record and a file copied from disk, each with a tag that
	// matches its own content.
	plant := func(m Memory, rel string) {
		m.Tags = append(m.Tags, ownerTag(m))
		body, err := renderMemory(m)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(cfg.VaultDir, "sources", rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plant(Memory{ID: "gmail_thread/drift1", Scope: "personal", Type: "email", Title: "Drift mail", Text: "Drift mail body.", Provider: "gmail", CreatedAt: "2026-11-01T00:00:00Z"}, "gmail/gmail_thread_drift1.md")
	plant(Memory{ID: "src_drift2", Scope: "project:demo", Type: "source", Title: "drift.md", Text: "Drift file body.", Tags: []string{"filesystem", "notes"}, CreatedAt: "2026-11-01T00:00:00Z"}, "filesystem/notes/src_drift2.md")
	run(t, "index", "rebuild")

	cases := []struct{ name, id, wantTrust string }{
		{"tag pasted on other text", w.ID, ""},
		{"text edited after the keep", edited, ""},
		{"teach correct replacement", correctedBy, ""},
		{"teach supersede replacement", supersededBy, ""},
		{"tentative record with a tag", tent, `"trust":"tentative"`},
		{"connector evidence with a tag", "gmail_thread/drift1", ""},
		{"file on disk with a tag", "src_drift2", ""},
	}
	// One home for every case, so no subtests: the hermetic root belongs to t.
	for _, tc := range cases {
		got := ownerCall(t, "read_memory", map[string]any{"id": tc.id})
		if strings.Contains(got, ownerTrustJSON) || (tc.wantTrust != "" && !strings.Contains(got, tc.wantTrust)) {
			t.Errorf("%s: trust wrong:\n%s", tc.name, got)
		}
	}
	for _, id := range []string{correctedBy, supersededBy} {
		for _, tag := range ownerMemory(t, cfg, id).Tags {
			if strings.HasPrefix(tag, memory.OwnerConfirmedTagPrefix) {
				t.Fatalf("teach carried the confirmed tag to %s", id)
			}
		}
	}
	if got := ownerCall(t, "search_memory", map[string]any{"query": "drift"}); strings.Contains(got, ownerTrustJSON) {
		t.Fatalf("search_memory shows a forged or stale row as confirmed:\n%s", got)
	}
}

// A row from a subscribed share can carry a tag that matches its text; it is
// still somebody else's vault, so it gets no trust value.
func TestSharedRowWithAValidTagIsNotOwnerConfirmed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	idn := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "alice")
	m := fixtureMemory("mem_20260601_000001_aaaaaaaa", "Zanzibar", "zanzibar harbor notes")
	m.Tags = []string{ownerTag(m)}
	publishGen(t, cfg, "alice", idn, []Memory{m})
	for tool, args := range map[string]map[string]any{
		"search_memory": {"query": "zanzibar"},
		"read_memory":   {"id": m.ID},
	} {
		got := ownerCall(t, tool, args)
		if !strings.Contains(got, ownerTag(m)) || !strings.Contains(got, `"owner":"alice"`) {
			t.Fatalf("%s did not return the tagged shared row:\n%s", tool, got)
		}
		if strings.Contains(got, ownerTrustJSON) {
			t.Fatalf("%s marks a shared row owner-confirmed:\n%s", tool, got)
		}
	}
	var read struct{ Owner, Trust string }
	if err := json.Unmarshal([]byte(run(t, "read", m.ID, "--json")), &read); err != nil || read.Owner != "alice" || read.Trust != "" {
		t.Fatalf("read --json shared row owner=%q trust=%q, %v", read.Owner, read.Trust, err)
	}
}

func TestReservedTagPrefixIsRefusedWhereverACommandTakesTags(t *testing.T) {
	cfg := reviewSetup(t)
	target := ownerSaved(t, "Teach target")
	tag := ownerTag(Memory{Scope: "global", Type: "insight", Title: "Forged", Text: "Forged text."})
	before, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"write", "--title", "Forged", "--text", "Forged text.", "--tags", "keep," + tag},
		{"teach", "memory", "correct", "--id", target, "--title", "Forged", "--text", "Forged text.", "--tags", tag, "--yes"},
		{"teach", "memory", "supersede", "--id", target, "--title", "Forged", "--text", "Forged text.", "--tags", "keep," + tag, "--yes"},
	} {
		var out bytes.Buffer
		err := Run(testCtx(t), args, &out, &out, nil)
		if err == nil || !strings.Contains(err.Error(), "reserved prefix "+memory.OwnerConfirmedTagPrefix) {
			t.Fatalf("%v: err = %v", args[:3], err)
		}
	}
	after, err := allMemoryFiles(cfg)
	if err != nil || len(after) != len(before) {
		t.Fatalf("a refused command wrote a memory: %d -> %d (%v)", len(before), len(after), err)
	}
	if h := run(t, "teach", "history"); !strings.Contains(h, "no Teach history") {
		t.Fatalf("a refused teach recorded a decision:\n%s", h)
	}
}

// The walk shows every field an agent chose or a reader acts on, the keep
// binds them, and a later change to one of them drops the trust value and the
// adoption line.
func TestReviewShowsAndBindsGoverningFields(t *testing.T) {
	cfg := reviewSetup(t)
	var w bytes.Buffer
	if err := Run(testCtx(t), []string{"write", "--scope", "project:demo", "--type", "fact", "--title", "Old plan", "--text", "The old plan.", "--json"}, &w, &w, nil); err != nil {
		t.Fatal(err)
	}
	var target struct{ ID string }
	if err := json.Unmarshal(w.Bytes(), &target); err != nil {
		t.Fatal(err)
	}
	held := func(res map[string]any) string {
		p, ok := res["proposal"].(map[string]any)
		if !ok {
			t.Fatalf("write was not held: %v", res)
		}
		return p["id"].(string)
	}
	decision := held(tentativeServe(t, map[string]any{
		// The sensitive word keeps the decision held under the tentative rule.
		"title": "Rerank lane", "text": "We ship the rerank lane. Passport renewal.", "type": "decision", "scope": "project:demo", "source": "agent-label",
		"as_of": "2026-01-02T00:00:00Z", "durability": "standing", "flip_conditions": "latency regresses;cost doubles", "review_by": "2027-01-01T00:00:00Z",
	}))
	correction := held(tentativeServe(t, map[string]any{
		"title": "Retire old plan", "text": "The old plan no longer holds.", "type": "correction", "scope": "project:demo",
		"target": target.ID, "disposition": "outdated",
	}))

	// The same decision saved at once as tentative (no sensitive word).
	savedRes := tentativeServe(t, map[string]any{
		"title": "Saved plan", "text": "We ship the saved plan.", "type": "decision", "scope": "project:demo", "source": "agent-label",
		"as_of": "2026-01-02T00:00:00Z", "durability": "standing", "flip_conditions": "latency regresses;cost doubles", "review_by": "2027-01-01T00:00:00Z",
	})
	savedID := tentativeMemoryID(t, savedRes)

	out := ownerKeepAll(t, 3)
	if !strings.Contains(out, "created at: 20") || !strings.Contains(out, "created at: the time you keep it\n") {
		t.Fatalf("created at not shown for saved and held notes:\n%s", out)
	}
	if sm := ownerMemory(t, cfg, savedID); !memory.IsOwnerConfirmed(sm) {
		t.Fatalf("saved tentative decision not confirmed: %+v", sm.Tags)
	}
	for _, want := range []string{
		"source: agent-label\n",
		"decision as of: 2026-01-02T00:00:00Z\n",
		"decision durability: standing\n",
		"decision flip condition: cost doubles\n",
		"decision flip condition: latency regresses\n",
		"decision review by: 2027-01-01T00:00:00Z\n",
		"decision complete: true\n",
		`meta: {"disposition":"outdated","target":"` + target.ID + `"}` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("walk did not show %q:\n%s", want, out)
		}
	}
	memID := func(pid string) string {
		d, err := readMCPProposalDecision(cfg, pid)
		if err != nil || d == nil || d.Status != "approved" {
			t.Fatalf("proposal %s: %+v %v", pid, d, err)
		}
		return d.MemoryID
	}
	dm, cm := ownerMemory(t, cfg, memID(decision)), ownerMemory(t, cfg, memID(correction))
	for tool, args := range map[string]map[string]any{
		"read_memory":   {"id": dm.ID},
		"search_memory": {"query": "rerank lane"},
		"list_memory":   {},
	} {
		if got := ownerCall(t, tool, args); !strings.Contains(got, ownerTrustJSON) {
			t.Fatalf("%s lost the confirmed decision:\n%s", tool, got)
		}
	}
	if got := ownerCall(t, "context_memory", map[string]any{"query": "rerank lane"}); !strings.Contains(got, "Adopted by user: the owner confirmed this exact text") {
		t.Fatalf("no adoption line:\n%s", got)
	}
	if got := ownerCall(t, "read_memory", map[string]any{"id": cm.ID}); !strings.Contains(got, ownerTrustJSON) {
		t.Fatalf("confirmed correction:\n%s", got)
	}

	// Edit a flip condition and the disposition after the keep.
	for path, swap := range map[string][2]string{dm.Path: {"latency regresses", "latency improves"}, cm.Path: {`"disposition":"outdated"`, `"disposition":"done"`}} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		edited := bytes.Replace(raw, []byte(swap[0]), []byte(swap[1]), 1)
		if bytes.Equal(edited, raw) {
			t.Fatalf("%q not in %s", swap[0], path)
		}
		if err := os.WriteFile(path, edited, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(t, "index", "rebuild")
	for _, id := range []string{dm.ID, cm.ID} {
		if got := ownerCall(t, "read_memory", map[string]any{"id": id}); strings.Contains(got, ownerTrustJSON) {
			t.Fatalf("an edited governing field kept the trust value:\n%s", got)
		}
	}
	got := ownerCall(t, "context_memory", map[string]any{"query": "rerank lane"})
	if strings.Contains(got, "the owner confirmed") || strings.Contains(got, "owner then confirmed") || !strings.Contains(got, "Adopted by user: no record either way") {
		t.Fatalf("edited decision still claims adoption:\n%s", got)
	}
}

// Plain `mora confirm` strips a confirmed tag that is already on the file, so
// it can never yield an owner-confirmed note.
func TestPlainConfirmStripsAConfirmedTag(t *testing.T) {
	cfg := reviewSetup(t)
	id := ownerSaved(t, "Planted")
	m := ownerMemory(t, cfg, id)
	ownerAddTag(t, m.Path, ownerTag(m))
	run(t, "confirm", id)
	for _, tag := range ownerMemory(t, cfg, id).Tags {
		if strings.HasPrefix(tag, memory.OwnerConfirmedTagPrefix) || tag == memory.TentativeTag {
			t.Fatalf("tags after plain confirm: %v", ownerMemory(t, cfg, id).Tags)
		}
	}
	if got := ownerCall(t, "read_memory", map[string]any{"id": id}); strings.Contains(got, ownerTrustJSON) {
		t.Fatalf("plain confirm produced an owner-confirmed note:\n%s", got)
	}
}

// The governed create refuses a caller-supplied confirmed tag, with or without
// the keep option, and refuses tentative and owner-confirmed together.
func TestCreateMemoryRefusesACallerConfirmedTag(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	m := Memory{Scope: "global", Type: "insight", Title: "Forged", Text: "Forged text.", Source: "mcp", CreatedAt: "2026-11-01T00:00:00Z"}
	m.Tags = []string{ownerTag(m)}
	for _, opts := range [][]createOption{nil, {withOwnerConfirmed()}} {
		if _, _, err := createMemory(testCtx(t), cfg, m, opts...); err == nil || !strings.Contains(err.Error(), "reserved prefix") {
			t.Fatalf("opts %d: err = %v", len(opts), err)
		}
	}
	m.Tags = nil
	if _, _, err := createMemory(testCtx(t), cfg, m, withTentative(), withOwnerConfirmed()); err == nil {
		t.Fatal("tentative and owner-confirmed together were accepted")
	}
	if files, err := allMemoryFiles(cfg); err != nil || len(files) != 0 {
		t.Fatalf("a refused create wrote %v (%v)", files, err)
	}
}

// created_at governs correction eligibility, so editing it in the file drops
// the trust value.
func TestEditedCreatedAtDropsOwnerConfirmed(t *testing.T) {
	cfg := reviewSetup(t)
	id := ownerSaved(t, "Dated note")
	ownerKeepAll(t, 1)
	m := ownerMemory(t, cfg, id)
	if got := ownerCall(t, "read_memory", map[string]any{"id": id}); !strings.Contains(got, ownerTrustJSON) {
		t.Fatalf("not confirmed before the edit:\n%s", got)
	}
	raw, _ := os.ReadFile(m.Path)
	edited := bytes.Replace(raw, []byte("created_at: "+m.CreatedAt), []byte("created_at: 2001-01-01T00:00:00Z"), 1)
	if bytes.Equal(edited, raw) {
		t.Fatalf("created_at line not found in:\n%s", raw)
	}
	if err := os.WriteFile(m.Path, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ownerCall(t, "read_memory", map[string]any{"id": id}); strings.Contains(got, ownerTrustJSON) {
		t.Fatalf("edited created_at kept the trust value:\n%s", got)
	}
}

// A confirmed note whose file no longer parses is served from the old index
// row, and that row must not read as owner-confirmed.
func TestUnreadableFileIsNotOwnerConfirmedInSearch(t *testing.T) {
	cfg := reviewSetup(t)
	id := ownerSaved(t, "Zephyr fragile")
	ownerKeepAll(t, 1)
	m := ownerMemory(t, cfg, id)
	if got := ownerCall(t, "search_memory", map[string]any{"query": "zephyr fragile"}); !strings.Contains(got, ownerTrustJSON) {
		t.Fatalf("not confirmed before the file broke:\n%s", got)
	}
	if err := os.WriteFile(m.Path, []byte("---\nid: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseMemory(m.Path); err == nil {
		t.Fatal("test file still parses")
	}
	got := ownerCall(t, "search_memory", map[string]any{"query": "zephyr fragile"})
	if !strings.Contains(got, id) {
		t.Fatalf("indexed row gone, test proves nothing:\n%s", got)
	}
	if strings.Contains(got, "owner-confirmed") {
		t.Fatalf("unreadable file still owner-confirmed:\n%s", got)
	}
}

// ownerBrokenConfirmed confirms a note, breaks its file, and returns the id.
func ownerBrokenConfirmed(t *testing.T, cfg Config, title string) string {
	t.Helper()
	id := ownerSaved(t, title)
	ownerKeepAll(t, 1)
	m := ownerMemory(t, cfg, id)
	if err := os.WriteFile(m.Path, []byte("---\nid: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseMemory(m.Path); err == nil {
		t.Fatal("test file still parses")
	}
	return id
}

// context_memory reads the indexed row when the live file is unreadable; it
// must not claim the owner confirmed it.
func TestUnreadableFileIsNotOwnerConfirmedInContext(t *testing.T) {
	cfg := reviewSetup(t)
	id := ownerBrokenConfirmed(t, cfg, "Quillon fragile")
	got := ownerCall(t, "context_memory", map[string]any{"query": "quillon fragile"})
	if !strings.Contains(got, "Quillon fragile") {
		t.Fatalf("indexed row gone, test proves nothing (%s):\n%s", id, got)
	}
	for _, bad := range []string{"owner-confirmed", "the owner confirmed", "owner then confirmed"} {
		if strings.Contains(got, bad) {
			t.Fatalf("unreadable file shows %q:\n%s", bad, got)
		}
	}
}

// The hybrid path loads rows by id from the index and falls back to them when
// hydration fails.
func TestUnreadableFileIsNotOwnerConfirmedInHybridSearch(t *testing.T) {
	cfg := reviewSetup(t)
	id := ownerBrokenConfirmed(t, cfg, "Marlow fragile")
	mems, _, err := hybridSearchTrace(testCtx(t), cfg, "marlow fragile", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range memory.WithProvenanceAll(mems) {
		if m.ID != id {
			continue
		}
		found = true
		if m.Trust == memory.TrustOwnerConfirmed || memory.IsOwnerConfirmed(m) {
			t.Fatalf("unreadable file is owner-confirmed via hybrid: %+v", m.Tags)
		}
	}
	if !found {
		t.Fatalf("indexed row gone, test proves nothing: %d rows", len(mems))
	}
}
