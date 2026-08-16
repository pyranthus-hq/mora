package update

import (
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("MORA_TEST_REAL_FSYNC") != "1" {
		atomicio.MarkerSyncFn = func(*os.File) error { return nil }
		atomicio.SyncDirFn = func(string) error { return nil }
	}
	os.Exit(m.Run())
}
