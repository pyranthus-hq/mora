package mora

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// runHookRecall drives the real UserPromptSubmit path and returns the injected
// additionalContext ("" when the hook stayed silent).
func runHookRecall(t *testing.T, prompt string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	argv := append([]string{"recall"}, args...)
	if err := cmdHook(testCtx(t), argv, &out, testStderr, strings.NewReader(`{"prompt":`+quoteJSON(prompt)+`}`)); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		return ""
	}
	return decodeHookOutput(t, out.String()).HookSpecificOutput.AdditionalContext
}

// A prompt that matches an email message body must still inject recall. The
// segment arm fuses those matches into positive RRF scores, and the hook's
// --threshold is a BM25 cost, so before this fix every fused row was dropped
// and the hook injected nothing.
func TestHookRecallKeepsFusedSegmentMatch(t *testing.T) {
	cfg := seedGmailSegmentsSearchFixture(t)
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	got := runHookRecall(t, "what did the kickoff thread say about "+gsSearchBeta+"?")
	if got == "" {
		t.Fatal("hook recall injected nothing for a prompt that matches an email body")
	}
	if !strings.Contains(got, "id: "+gsSearchWellFormedID) {
		t.Fatalf("recall should cite the matching thread %s, got:\n%s", gsSearchWellFormedID, got)
	}
	if !strings.Contains(got, gsSearchBeta) {
		t.Fatalf("recall preview should show the matching message text, got:\n%s", got)
	}
	if len(got) > hookRecallByteLimit {
		t.Fatalf("additionalContext length = %d, want <= %d", len(got), hookRecallByteLimit)
	}
}

// --threshold keeps its meaning on fused rows: it is compared to the row's
// parent BM25 cost, so an impossibly strict cost still silences the hook.
func TestHookRecallThresholdStillAppliesToFusedRows(t *testing.T) {
	cfg := seedGmailSegmentsSearchFixture(t)
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	if got := runHookRecall(t, "what did the kickoff thread say about "+gsSearchBeta+"?", "--threshold", "-1000"); got != "" {
		t.Fatalf("a -1000 BM25 cost threshold should drop every row, got:\n%s", got)
	}
}

// A match past the first 180 runes of a body must be what the preview shows.
func TestHookRecallPreviewCentersOnLateMatch(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	filler := strings.Repeat("Routine planning notes about the weekly sync and agenda items. ", 12)
	if err := writeMemory(cfg, Memory{
		ID: newID(), Scope: "personal", Type: "note", Title: "Weekly sync notes", Source: "test",
		CreatedAt: "2026-06-01T00:00:00Z",
		Text:      filler + "The vendor contract renewal uses codeword quillbarrow and closes Friday.",
	}); err != nil {
		t.Fatal(err)
	}
	// A word is only rare against a real corpus, so give the index one.
	for i := 0; i < 24; i++ {
		if err := writeMemory(cfg, Memory{
			ID: newID(), Scope: "personal", Type: "note", Title: fmt.Sprintf("Filler %d", i), Source: "test",
			CreatedAt: "2026-06-01T00:00:00Z", Text: fmt.Sprintf("Ordinary planning note number %d.", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	got := runHookRecall(t, "remind me what quillbarrow was about")
	if !strings.Contains(got, "quillbarrow") {
		t.Fatalf("recall preview should be centered on the late match, got:\n%s", got)
	}
}
