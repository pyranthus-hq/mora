package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	ingestpkg "github.com/pyranthus-hq/mora/internal/ingest"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestIMessageAttachmentRetry(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	pdf := filepath.Join(t.TempDir(), "lease.pdf")
	writeMinimalPDF(t, pdf, "retry attachment text")
	parent := memory.MappedMemory{
		StableID: "imessage_chat_retry", Provider: "imessage", ProviderID: "retry",
		Scope: "personal", Type: "source", Title: "Conversation", Body: "See lease",
		ContentHash: memory.ContentHash("unchanged conversation"),
		Attachments: []memory.Attachment{{Path: pdf, Filename: "lease.pdf", MimeType: "application/pdf"}},
	}
	derived := memory.MappedMemory{Provider: parent.Provider, StableID: "att_" + memory.ContentHash(parent.StableID+":"+pdf)}
	target := ingestpkg.MappedTargetPath(cfg, derived)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "blocker"), []byte("block"), 0o644); err != nil {
		t.Fatal(err)
	}

	// AC-001.1: the parent survives a derived write failure.
	if _, err := writeIMessageConversation(cfg, parent); err == nil {
		t.Fatal("expected attachment write error")
	}
	if info, err := os.Stat(ingestpkg.MappedTargetPath(cfg, parent)); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("parent missing: %v", err)
	}

	// AC-002.1: retry errors must propagate even with an unchanged parent.
	if _, err := writeIMessageConversation(cfg, parent); err == nil {
		t.Fatal("expected unchanged-parent retry error")
	}

	// AC-001.2: removing the obstruction lets the unchanged parent repair the PDF.
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	if wrote, err := writeIMessageConversation(cfg, parent); err != nil || wrote {
		t.Fatalf("retry = %v, %v", wrote, err)
	}
	body, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(body), "retry attachment text") {
		t.Fatalf("missing PDF text: %q, %v", body, err)
	}

	// AC-001.3: a regular derived file bypasses the attachment writer entirely.
	calls := 0
	testHookAttachmentAfterParentCheck = func() { calls++ }
	t.Cleanup(func() { testHookAttachmentAfterParentCheck = nil })
	// Changing the source makes any accidental PDF re-read observable in the output.
	writeMinimalPDF(t, pdf, "changed source text")
	if wrote, err := writeIMessageConversation(cfg, parent); err != nil || wrote {
		t.Fatalf("unchanged = %v, %v", wrote, err)
	}
	if calls != 0 {
		t.Fatalf("attachment writer called %d times", calls)
	}
	after, err := os.ReadFile(target)
	if err != nil || string(after) != string(body) {
		t.Fatalf("existing memory changed: %v", err)
	}

	// AC-001.4: even a readable PDF payload is ignored when attachment metadata is non-PDF.
	nonPDF := filepath.Join(t.TempDir(), "photo.bin")
	writeMinimalPDF(t, nonPDF, "must not be extracted")
	parent.Attachments = append(parent.Attachments, memory.Attachment{Path: nonPDF, Filename: "photo.bin", MimeType: "image/png"})
	if wrote, err := writeIMessageConversation(cfg, parent); err != nil || wrote {
		t.Fatalf("non-PDF = %v, %v", wrote, err)
	}
	derived.StableID = "att_" + memory.ContentHash(parent.StableID+":"+nonPDF)
	if _, err := os.Stat(ingestpkg.MappedTargetPath(cfg, derived)); !os.IsNotExist(err) {
		t.Fatalf("non-PDF memory exists: %v", err)
	}
	if calls != 0 {
		t.Fatalf("attachment writer called for non-PDF: %d", calls)
	}
}

// AC-002.2: run the existing direct-caller contracts without changing their assertions.
func TestIMessageAttachmentRetryExistingContracts(t *testing.T) {
	withTempHome(t)
	t.Run("PDF", TestWriteAttachmentMemories)
	t.Run("Suppression", TestGovernance_AttachmentInheritsParentSuppression)
	t.Run("Cascade", TestGovernance_ForgetCascadesToDerivedAttachmentAndBlocksReingest)
	t.Run("Race", TestGovernance_ParentForgetAfterAttachmentPrecheckBlocksDerivedWrite)
	t.Run("TitleFallback", TestMc_WriteAttachmentMemoriesTitleFallback)
	t.Run("WriteError", TestMc_WriteAttachmentMemoriesWriteError)
}
