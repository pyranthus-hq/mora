//go:build whatsapplive

package whatsapp

import (
	"os"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// Run explicitly with: go test -tags whatsapplive ./internal/whatsapp
func TestLiveDatabase(t *testing.T) {
	if os.Getenv("MORA_WHATSAPP_LIVE_TEST") != "1" {
		t.Skip("set MORA_WHATSAPP_LIVE_TEST=1 to read the local WhatsApp store")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewLiveFetcher(DefaultDBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.FetchPage(KindConversation, memory.FetchWindow{}, ""); err != nil {
		t.Fatal(err)
	}
}
