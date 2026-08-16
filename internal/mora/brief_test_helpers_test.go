package mora

import (
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 6, 8, 15, 4, 5, 0, time.UTC)

func testCfg(t *testing.T) Config { t.Helper(); return Config{StateDir: t.TempDir()} }
