package mora

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/operation"
)

func TestIngestBeginRetainsUncoveredAbandonedReceipt(t *testing.T) {
	for _, brokenProbe := range []bool{false, true} {
		name := "uncovered_path"
		if brokenProbe {
			name = "unreadable_journal"
		}
		t.Run(name, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			now := time.Now().UTC()
			cfg.SetOperationClock(func() time.Time { return now })
			oldAlive := operation.ProcessAlive
			operation.ProcessAlive = func(int) bool { return false }
			t.Cleanup(func() { operation.ProcessAlive = oldAlive })
			rec := operation.Record{SchemaVersion: operation.SchemaVersion, Kind: operation.KindIngest, State: operation.Running, RunID: "op_later_run", OwnerPID: 4242, StartedAt: now.Add(-90 * time.Minute).Format(time.RFC3339Nano), HeartbeatAt: now.Add(-90 * time.Minute).Format(time.RFC3339Nano), Phase: "awaiting_rebuild", Counts: operation.Counts{Items: 9}}
			path := operation.Path(cfg, operation.KindIngest, rec.RunID)
			if err := operation.SaveRecord(path, rec); err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(cfg.StateDir, "ingest", "filesystem", "journal.log")
			if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
				t.Fatal(err)
			}
			if brokenProbe {
				if err := os.Mkdir(journal, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				evidence := filepath.Join(t.TempDir(), "uncovered.md")
				if err := os.WriteFile(evidence, []byte("evidence"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(journal, []byte("run op_earlier_run now\n"+evidence+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Exercise the real ingest entry point. An invalid source can fail after
			// Begin; its dispatch outcome must never erase the earlier receipt.
			_, _ = ingestSourceDetailed(context.Background(), cfg, Source{}, io.Discard)
			got, err := operation.LoadRecord(path)
			if err != nil || got.State != operation.Failed || got.FailureCode != operation.FailureOwnerAbandoned || got.Phase != rec.Phase || got.Counts != rec.Counts {
				t.Fatalf("abandoned publication evidence lost: %+v, %v", got, err)
			}
		})
	}
}
