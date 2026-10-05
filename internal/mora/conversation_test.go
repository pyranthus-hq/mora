package mora

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	conv "github.com/pyranthus-hq/mora/internal/conversation"
)

func seedConversation(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(cfg.HomeDir(), "synthetic.jsonl")
	text := `{"type":"session_meta","payload":{"id":"fixture","cwd":` + quoteJSON(cfg.HomeDir()) + `}}` + "\n" + `{"timestamp":"2026-10-05T09:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"rareorchid correction use green not blue"}]}}` + "\n"
	if err := os.WriteFile(source, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if err := conv.Enable(conversationSettingsPath(cfg), conv.Source{Provider: "codex", File: source, Scope: "project:work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := syncConversations(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}
func TestConversationCLIAndMCPRecallUseOriginals(t *testing.T) {
	cfg := seedConversation(t)
	var out bytes.Buffer
	if err := Run(testCtx(t), []string{"conversation", "search", "--query", "rareorchid", "--scope", "project:work"}, &out, testStderr, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rareorchid") || !strings.Contains(out.String(), "original-conversation-message:user") {
		t.Fatalf("CLI %s", out.String())
	}
	result, err := mcpConversationSearch(testCtx(t), cfg, map[string]any{"query": "rareorchid", "scope": "project:work"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(result)
	if !strings.Contains(string(b), "rareorchid") {
		t.Fatalf("search %s", b)
	}
	result, err = mcpConversationZoom(testCtx(t), cfg, map[string]any{"session": "codex/fixture", "start": 0, "count": 1, "max_bytes": 12})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(result)
	if !strings.Contains(string(b), "next_offset") || strings.Contains(string(b), "rareorchid correction use green not blue") {
		t.Fatalf("unbounded MCP read %s", b)
	}
	out.Reset()
	if err := Run(testCtx(t), []string{"conversation", "zoom", "--session", "codex/fixture", "--start", "0", "--count", "1"}, &out, testStderr, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rareorchid correction use green not blue") {
		t.Fatalf("exact read %s", out.String())
	}
}
func TestConversationContinuityIsScopedAndDisabledByDefault(t *testing.T) {
	cfg := seedConversation(t)
	text, err := conversationContinuity(cfg, "project:work", "", 4096)
	if err != nil || !strings.Contains(text, "rareorchid") {
		t.Fatalf("continuity %q %v", text, err)
	}
	text, err = conversationContinuity(cfg, "project:other", "", 4096)
	if err != nil || text != "" {
		t.Fatalf("scope leaked %q %v", text, err)
	}
	before, err := os.ReadFile(conversationStore(cfg).Dir + "/index.db")
	if err != nil {
		t.Fatal(err)
	}
	_, err = mcpConversationSearch(testCtx(t), cfg, map[string]any{"query": "rareorchid"})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(conversationStore(cfg).Dir + "/index.db")
	if !bytes.Equal(before, after) {
		t.Fatal("MCP read mutated index")
	}
	if err := os.Remove(conversationSettingsPath(cfg)); err != nil {
		t.Fatal(err)
	}
	text, err = conversationContinuity(cfg, "", "", 4096)
	if err != nil || text != "" {
		t.Fatalf("default-off %q %v", text, err)
	}
}
func TestConversationBriefAndContextRespectMemoryFilters(t *testing.T) {
	cfg := seedConversation(t)
	got, err := mcpBrief(testCtx(t), cfg, map[string]any{"scope": "project:work"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), "conversation_continuity") || !strings.Contains(string(b), "rareorchid") {
		t.Fatalf("brief not using archive %s", b)
	}
	got, err = mcpContextMemory(testCtx(t), cfg, map[string]any{"query": "rareorchid", "scope": "project:work"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(got)
	if !strings.Contains(string(b), "rareorchid") {
		t.Fatalf("context missing originals %s", b)
	}
	got, err = mcpContextMemory(testCtx(t), cfg, map[string]any{"query": "rareorchid", "source": "gmail"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(got)
	if strings.Contains(string(b), "rareorchid") {
		t.Fatalf("explicit filter leaked archive %s", b)
	}
}

func TestConversationClaudeHookAndCLIActuallyRecallArchive(t *testing.T) {
	cfg := seedConversation(t)
	restore := stubHookBrief(t, "ordinary brief")
	defer restore()
	var out bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"session-start"}, &out, testStderr, strings.NewReader(`{}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rareorchid") {
		t.Fatalf("startup not using conversation %s", out.String())
	}
	out.Reset()
	if err := cmdHook(testCtx(t), []string{"recall"}, &out, testStderr, strings.NewReader(`{"prompt":"What was our rareorchid correction about green?"}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rareorchid") {
		t.Fatalf("recall not using conversation %s", out.String())
	}
	got, err := mcpBrief(testCtx(t), cfg, map[string]any{"scope": "project:work", "envelope": true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), "conversation_continuity") {
		t.Fatalf("envelope forgot continuity %s", b)
	}
	out.Reset()
	if err := Run(testCtx(t), []string{"brief", "--scope", "project:work", "--json"}, &out, testStderr, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rareorchid") {
		t.Fatalf("CLI brief missing archive %s", out.String())
	}
	got, err = mcpSearchMemory(testCtx(t), cfg, map[string]any{"query": "rareorchid", "scope": "project:work"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(got)
	if !strings.Contains(string(b), "conversation_results") {
		t.Fatalf("ordinary search missed conversation %s", b)
	}
	got, err = mcpSearchMemory(testCtx(t), cfg, map[string]any{"query": "rareorchid", "source": "gmail"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(got)
	if strings.Contains(string(b), "rareorchid") {
		t.Fatalf("source filter leaked %s", b)
	}
}

func TestConversationCLIValidationAndCaptureErrors(t *testing.T) {
	cfg := seedConversation(t)
	for _, args := range [][]string{{"conversation", "enable", "--provider", "codex"}, {"conversation", "watch", "--interval", "1s"}, {"conversation", "view", "--session", "codex/fixture", "--max-bytes", "12"}, {"conversation", "zoom", "--session", "codex/fixture", "--start", "1", "--count", "2"}, {"conversation", "search", "--query", "rareorchid", "--limit", "0"}} {
		var out bytes.Buffer
		if err := Run(testCtx(t), args, &out, testStderr, strings.NewReader("")); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	settings, err := conv.LoadSettings(conversationSettingsPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(settings.Sources[0].File); err != nil {
		t.Fatal(err)
	}
	result, err := syncConversations(cfg)
	if err == nil || len(result.Errors) == 0 {
		t.Fatalf("silent capture failure %+v %v", result, err)
	}
	status, err := getConversationStatus(cfg)
	if err != nil || status.Capture == nil || len(status.Capture.Errors) == 0 {
		t.Fatalf("failure not persisted %+v %v", status, err)
	}
}

func TestConversationVaultContainmentRefusesCaptureAndPublication(t *testing.T) {
	cfg := seedConversation(t)
	cfg.DataDir = filepath.Join(cfg.VaultDir, "data")
	if _, err := syncConversations(cfg); err == nil {
		t.Fatal("capture allowed inside vault")
	}
	f := &fakeExec{}
	if err := syncGit(testCtx(t), cfg, nil, testStderr, f.run); err == nil || len(f.calls) != 0 {
		t.Fatalf("publication not blocked: %v %v", err, f.calls)
	}
	cfg.DataDir = t.TempDir()
	if err := os.Symlink(cfg.VaultDir, filepath.Join(cfg.DataDir, "conversations")); err != nil {
		t.Fatal(err)
	}
	if err := conversationGuardPaths(cfg); err == nil {
		t.Fatal("symlink alias allowed")
	}
}
func TestConversationRecallKeepsLongMessageMatchAndPointer(t *testing.T) {
	cfg := seedConversation(t)
	if err := conversationStore(cfg).Import(conv.Session{ID: "codex/long", Provider: "codex", Scope: "project:work", Source: "synthetic"}, []conv.Message{{Kind: "echo", Text: strings.Repeat("longcontextword ", 100) + "rareconstraint" + strings.Repeat(" longcontextword", 100)}}); err != nil {
		t.Fatal(err)
	}
	text, err := conversationContinuity(cfg, "project:work", "rareconstraint", 400)
	if err != nil || len(text) > 400 || !strings.Contains(text, "rareconstraint") || !strings.Contains(text, "codex/long message 0") {
		t.Fatalf("recall lost match/pointer: %q %v", text, err)
	}
}

func TestConversationContinuityFullTreeFitsIncludingFraming(t *testing.T) {
	cfg := seedConversation(t)
	messages := make([]conv.Message, 128)
	for i := range messages {
		messages[i] = conv.Message{Kind: "user", Text: strings.Repeat("nontrivial history ", 80), Date: "2030-01-01T00:00:00Z"}
	}
	if err := conversationStore(cfg).Import(conv.Session{ID: "codex/full", Provider: "codex", Scope: "project:work", Source: "synthetic"}, messages); err != nil {
		t.Fatal(err)
	}
	text, err := conversationContinuity(cfg, "project:work", "", 2000)
	if err != nil || len(text) > 2000 || !strings.Contains(text, "codex/full") {
		t.Fatalf("saturated view dropped: length=%d err=%v", len(text), err)
	}
}
