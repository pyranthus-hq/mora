package conversation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestCodexCaptureAllowlistPartialTailAndReasoning(t *testing.T) {
	root := t.TempDir()
	work := t.TempDir()
	other := t.TempDir()
	p := filepath.Join(root, "work.jsonl")
	header := `{"type":"session_meta","payload":{"id":"one","cwd":` + quote(work) + `}}` + "\n"
	body := `{"timestamp":"2026-10-05T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"original correction choose green"}]}}` + "\n" +
		`{"type":"response_item","payload":{"type":"reasoning","encrypted_content":"never-archive-this"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"assistant","channel":"analysis","content":[{"type":"output_text","text":"hidden-analysis"}]}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"user_message","message":"original correction choose green"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call","name":"inspect","arguments":"{\"path\":\"file\"}","call_id":"c1"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"rareorchid complete tool result"}}` + "\n"
	writeTranscript(t, p, header+body+`{"type":"response_item"`)
	writeTranscript(t, filepath.Join(root, "other.jsonl"), `{"type":"session_meta","payload":{"id":"other","cwd":`+quote(other)+`}}`+"\n"+body)
	s := Store{Dir: t.TempDir()}
	cfg := Settings{Sources: []Source{{Provider: "codex", Root: root, CWDs: []string{work}, Scope: "project:work"}}}
	r, err := Capture(s, cfg)
	if err != nil || r.Messages != 3 || r.Sessions != 1 || r.PendingTails != 1 {
		t.Fatalf("capture %+v %v", r, err)
	}
	hits, err := s.Search("rareorchid", "project:work", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("search %+v %v", hits, err)
	}
	b, err := os.ReadFile(s.path("codex/one"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "never-archive-this") || strings.Contains(string(b), "hidden-analysis") {
		t.Fatal("reasoning captured")
	}
	r, err = Capture(s, cfg)
	if err != nil || r.Added != 0 {
		t.Fatalf("repeat %+v %v", r, err)
	}
	writeTranscript(t, p, header+body+`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"new final"}]}}`+"\n")
	r, err = Capture(s, cfg)
	if err != nil || r.Added != 1 {
		t.Fatalf("append %+v %v", r, err)
	}
}
func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestClaudeCaptureKeepsToolsExcludesThinkingAndSidechain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claude.jsonl")
	cwd := t.TempDir()
	writeTranscript(t, p, `{"type":"user","sessionId":"claudeone","cwd":`+quote(cwd)+`,"timestamp":"2026-10-05T10:00:00Z","message":{"role":"user","content":"hello"}}`+"\n"+
		`{"type":"assistant","sessionId":"claudeone","message":{"role":"assistant","content":[{"type":"thinking","thinking":"secret-thought"},{"type":"text","text":"answer"},{"type":"tool_use","name":"read","input":{"path":"x"}}]}}`+"\n"+
		`{"type":"user","sessionId":"claudeone","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"exact evidence"}]}}`+"\n"+
		`{"type":"user","isSidechain":true,"sessionId":"claudeone","message":{"content":"delegated prompt"}}`+"\n")
	s := Store{Dir: t.TempDir()}
	r, err := Capture(s, Settings{Sources: []Source{{Provider: "claude", File: p, Scope: "project:work"}}})
	if err != nil || r.Messages != 3 {
		t.Fatalf("capture %+v %v", r, err)
	}
	b, _ := os.ReadFile(s.path("claude/claudeone"))
	if strings.Contains(string(b), "secret-thought") || strings.Contains(string(b), "delegated prompt") {
		t.Fatal("hidden/sidechain captured")
	}
	z, err := s.Zoom("claude/claudeone", 2, 1)
	if err != nil || z.Message.Kind != "echo" || !strings.Contains(z.Message.Text, "exact evidence") {
		t.Fatalf("tool result provenance %+v %v", z, err)
	}
}
func TestCaptureReportsMalformedAllowedSourceAndRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "bad.jsonl")
	work := t.TempDir()
	writeTranscript(t, p, `{"type":"session_meta","payload":{"id":"bad","cwd":`+quote(work)+`}}`+"\nnot-json\n")
	cfg := Settings{Sources: []Source{{Provider: "codex", Root: root, CWDs: []string{work}, Scope: "project:work"}}}
	r, err := Capture(Store{Dir: t.TempDir()}, cfg)
	if err == nil || len(r.Errors) != 1 {
		t.Fatalf("silent corruption %+v %v", r, err)
	}
	external := filepath.Join(t.TempDir(), "outside.jsonl")
	writeTranscript(t, external, `{"type":"session_meta","payload":{"id":"outside","cwd":`+quote(work)+`}}`+"\n")
	if err := os.Symlink(external, filepath.Join(root, "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeSource(Source{Provider: "codex", File: filepath.Join(root, "link.jsonl"), Scope: "project:work"}); err == nil {
		t.Fatal("symlink transcript enabled")
	}
}

func TestSeparateWorkScopesCanShareSourceRoot(t *testing.T) {
	root := t.TempDir()
	one := t.TempDir()
	two := t.TempDir()
	settingsPath := filepath.Join(t.TempDir(), "conversations.json")
	for _, item := range []struct{ id, cwd string }{{"one", one}, {"two", two}} {
		writeTranscript(t, filepath.Join(root, item.id+".jsonl"), `{"type":"session_meta","payload":{"id":`+quote(item.id)+`,"cwd":`+quote(item.cwd)+`}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"scopekeyword"}]}}`+"\n")
	}
	for _, src := range []Source{{Provider: "codex", Root: root, CWDs: []string{one}, Scope: "project:one"}, {Provider: "codex", Root: root, CWDs: []string{two}, Scope: "project:two"}} {
		if err := Enable(settingsPath, src); err != nil {
			t.Fatal(err)
		}
	}
	settings, err := LoadSettings(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: t.TempDir()}
	if _, err := Capture(s, settings); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"project:one", "project:two"} {
		hits, err := s.Search("scopekeyword", scope, 5)
		if err != nil || len(hits) != 1 || hits[0].Scope != scope {
			t.Fatalf("scope %s %+v %v", scope, hits, err)
		}
	}
	if err := Enable(settingsPath, Source{Provider: "codex", Root: root, CWDs: []string{one}, Scope: "project:other"}); err == nil {
		t.Fatal("overlapping scope accepted")
	}
}

func TestCodexForkUsesChildProviderIdentity(t *testing.T) {
	root := t.TempDir()
	parent := "11111111-1111-1111-1111-111111111111"
	child := "22222222-2222-2222-2222-222222222222"
	header := `{"type":"session_meta","payload":{"id":"` + parent + `","cwd":"` + root + `"}}` + "\n"
	original := filepath.Join(root, "rollout-"+parent+".jsonl")
	fork := filepath.Join(root, "rollout-"+parent+"_"+child+".jsonl")
	for _, p := range []string{original, fork} {
		if err := os.WriteFile(p, []byte(header+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fork fixture"}]}}`+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := Store{Dir: t.TempDir()}
	result, err := Capture(store, Settings{Sources: []Source{{Provider: "codex", Root: root, CWDs: []string{root}, Scope: "project:work"}}})
	if err != nil || result.Sessions != 2 {
		t.Fatalf("fork identity collided: %+v %v", result, err)
	}
	for _, id := range []string{parent, child} {
		if _, err := store.Zoom("codex/"+id, 0, 1); err != nil {
			t.Fatal(err)
		}
	}
}
