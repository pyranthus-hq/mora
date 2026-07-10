package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// destFor returns the on-disk path a connector memory would write to.
func destFor(cfg Config, mm memory.MappedMemory) string {
	return filepath.Join(sourcesRoot(cfg), mm.Provider, memory.SafeFilename(mm.StableID)+".md")
}

func TestForget_ChatRemovesAndSuppressesResync(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	mm := imsgMM("solo", "+14155550123")
	dest := destFor(cfg, mm)
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatal(err)
	}

	run(t, "forget", "--chat", "imessage_chat/solo", "--yes")

	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("forget --chat must remove the existing memory now")
	}
	g, _ := loadGovernance(cfg)
	if len(g.activeSuppress()) != 1 {
		t.Fatalf("forget must record 1 active suppression, got %d", len(g.activeSuppress()))
	}
	// The resurrection test: the next hourly sync re-fetches the live item.
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("RESURRECTION: forgotten chat came back on the next sync")
	}
}

func TestForget_HandleRemovesSoleKeepsGroup(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	solo := imsgMM("solo", "+14155550123")
	group := imsgMM("grp", "+14155550123", "+14155550999")
	if err := writeMappedMemory(cfg, solo); err != nil {
		t.Fatal(err)
	}
	if err := writeMappedMemory(cfg, group); err != nil {
		t.Fatal(err)
	}

	run(t, "forget", "--handle", "+14155550123", "--yes")

	if _, err := os.Stat(destFor(cfg, solo)); !os.IsNotExist(err) {
		t.Fatal("1:1 chat with the forgotten handle must be removed")
	}
	if _, err := os.Stat(destFor(cfg, group)); err != nil {
		t.Fatal("GROUP thread must be KEPT (person forget must not delete group memories)")
	}
}

func TestForget_EmailSoleAddressKeepsMulti(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	solo := gmailMM("solo", []string{"sam@example.com"}, nil)
	multi := gmailMM("multi", []string{"sam@example.com"}, []string{"me@x.com"})
	if err := writeMappedMemory(cfg, solo); err != nil {
		t.Fatal(err)
	}
	if err := writeMappedMemory(cfg, multi); err != nil {
		t.Fatal(err)
	}

	run(t, "forget", "--email", "Sam@Example.com", "--yes")

	if _, err := os.Stat(destFor(cfg, solo)); !os.IsNotExist(err) {
		t.Fatal("sole-address thread must be removed")
	}
	if _, err := os.Stat(destFor(cfg, multi)); err != nil {
		t.Fatal("multi-address thread must be kept")
	}
}

func TestForget_DryRunListsNoChange(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	mm := imsgMM("solo", "+14155550123")
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatal(err)
	}
	out := run(t, "forget", "--chat", "imessage_chat/solo", "--dry-run")
	if !strings.Contains(out, "imessage_chat/solo") {
		t.Fatalf("dry-run must list the affected memory, got: %q", out)
	}
	if _, err := os.Stat(destFor(cfg, mm)); err != nil {
		t.Fatal("dry-run must NOT remove anything")
	}
	g, _ := loadGovernance(cfg)
	if len(g.Entries) != 0 {
		t.Fatal("dry-run must NOT write a ledger entry")
	}
}

func TestForget_RefusesWithoutYes(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	mm := imsgMM("solo", "+14155550123")
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatal(err)
	}
	if _, err := runErr(t, "forget", "--chat", "imessage_chat/solo"); err == nil {
		t.Fatal("forget without --yes or --dry-run must refuse")
	}
	if _, err := os.Stat(destFor(cfg, mm)); err != nil {
		t.Fatal("a refused forget must not remove anything")
	}
}

func TestUnforget_RevokesSuppression(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	mm := imsgMM("solo", "+14155550123")
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatal(err)
	}
	run(t, "forget", "--chat", "imessage_chat/solo", "--yes")
	g, _ := loadGovernance(cfg)
	if len(g.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(g.Entries))
	}
	id := g.Entries[0].ID

	run(t, "unforget", id, "--yes")

	// After unforget, the next sync re-ingests (the write is no longer suppressed).
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(destFor(cfg, mm)); err != nil {
		t.Fatal("after unforget the memory must be re-ingestable")
	}
}

func TestForget_ListShowsActiveEntries(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	if err := writeMappedMemory(cfg, imsgMM("solo", "+14155550123")); err != nil {
		t.Fatal(err)
	}
	run(t, "forget", "--chat", "imessage_chat/solo", "--yes")
	out := run(t, "forget", "list")
	if !strings.Contains(out, "imessage_chat/solo") {
		t.Fatalf("forget list must show the active entry, got: %q", out)
	}
	_ = cfg
}
