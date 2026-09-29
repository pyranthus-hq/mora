package mora

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

type allowlistTestFetcher struct {
	items []memory.Item
}

func (f *allowlistTestFetcher) FetchPage(memory.ItemKind, memory.FetchWindow, string) (memory.Page, error) {
	return memory.Page{Items: f.items}, nil
}
func (f *allowlistTestFetcher) Close() error { return nil }

func TestWhatsAppChatFlagPersistsFailClosedAllowlist(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	withRuntimeGOOS(t, "darwin")
	out, err := runErr(t, "connectors", "enable", "whatsapp", "--chat", " MCAC Test Group ", "--chat", "mcac test group")
	if err != nil {
		t.Fatalf("enable whatsapp allowlist: %v\n%s", err, out)
	}
	sources, err := loadSources(mustConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if source.Type != "whatsapp" {
			continue
		}
		if !source.IsEnabled() || !source.WhatsAppAllowlistConfigured {
			t.Fatalf("allowlisted source not enabled/configured: %+v", source)
		}
		if len(source.AllowConversations) != 1 || source.AllowConversations[0] != "MCAC Test Group" {
			t.Fatalf("allowlist = %#v", source.AllowConversations)
		}
		return
	}
	t.Fatal("whatsapp source was not created")
}

func TestWhatsAppChatFlagRejectsOtherConnectors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	if out, err := runErr(t, "connectors", "enable", "imessage", "--chat", "MCAC Test Group"); err == nil || !strings.Contains(out+err.Error(), "only valid") {
		t.Fatalf("non-whatsapp --chat error=%v out=%q", err, out)
	}
}

func TestWhatsAppAllowlistChangeResetsCheckpoint(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := setWhatsAppAllowlist(cfg, []string{"Old"}); err != nil {
		t.Fatal(err)
	}
	statusPath := whatsAppStatusPath(cfg, "whatsapp")
	if err := os.MkdirAll(filepath.Dir(statusPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath, []byte(`{"checkpoint":"99"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resetWhatsAppSyncStatus(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statusPath); !os.IsNotExist(err) {
		t.Fatalf("status checkpoint was not reset: %v", err)
	}
}

func TestWhatsAppWriterRejectsFetcherOutsideAllowlist(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	withRuntimeGOOS(t, "darwin")
	original := newWhatsAppFetcher
	t.Cleanup(func() { newWhatsAppFetcher = original })
	newWhatsAppFetcher = func(string, []string, bool) (whatsAppFetcher, error) {
		return &allowlistTestFetcher{items: []memory.Item{{
			Kind: whatsappConversationKindForTest(), ProviderID: "excluded@g.us", Title: "Excluded Group", Body: "EXCLUDED-CANARY",
		}}}, nil
	}
	source := Source{Name: "whatsapp", Type: "whatsapp", Scope: "personal", AllowConversations: []string{"MCAC Test Group"}, WhatsAppAllowlistConfigured: true}
	var out bytes.Buffer
	if _, err := ingestWhatsAppDetailed(context.Background(), cfg, source, &out); err == nil || !strings.Contains(err.Error(), "failed to write") {
		t.Fatalf("excluded fake item error=%v out=%q", err, out.String())
	}
	matches, err := filepath.Glob(filepath.Join(cfg.VaultDir, "sources", "whatsapp", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("excluded item reached vault: %v", matches)
	}
}

func whatsappConversationKindForTest() memory.ItemKind {
	return memory.ItemKind("whatsapp_conversation")
}
