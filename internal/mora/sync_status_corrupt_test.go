package mora

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/imessage"
)

// Audit V8: a truncated status file made iMessage, WhatsApp, Apple Calendar and
// GitHub dereference a nil status and panic, taking sibling sources down.
func TestCorruptSyncStatusIsAnErrorNotAPanic(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	defer stubIMessageReadiness(t, true)()
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	orig := newIMessageFetcher
	defer func() { newIMessageFetcher = orig }()
	newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
		return imessage.NewSyntheticFetcher(1, 1), nil
	}
	s := Source{Name: "imessage", Type: "imessage"}
	path := syncStatusPathFor(cfg, s)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"source":"imess`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ingestIMessageDetailed(testCtx(t), cfg, s, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "is unreadable") {
		t.Fatalf("want a named unreadable-status error, got %v", err)
	}
}
