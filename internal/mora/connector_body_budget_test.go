package mora

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// budgetThread builds a Gmail-shaped thread of n messages whose joined body is
// exactly size bytes, with a unique marker at the very end of the last message.
func budgetThread(pid string, n, size int) (memory.Item, string) {
	id := memory.StableID(google.KindGmailThread, pid)
	tail := "TAILMARKER" + strings.ToUpper(pid)
	msgs := make([]commitmentMessageEvidence, n)
	parts := make([]string, n)
	for i := range parts {
		from := fmt.Sprintf("sender%d@example.com", i)
		msgs[i] = commitmentMessageEvidence{MessageRef: fmt.Sprintf("%s#m%02d", id, i), Sender: from, At: "2026-06-10T10:00:00Z", BlockRefs: []string{"body"}}
		parts[i] = fmt.Sprintf("From: %s\n\n", from)
	}
	fixed := len(strings.Join(parts, "\n\n---\n\n")) + len(tail)
	fill := size - fixed
	for i := range parts {
		w := fill / n
		if i == n-1 {
			w = fill - (n-1)*(fill/n)
		}
		parts[i] += strings.Repeat("lorem ipsum ", w/12+1)[:w]
	}
	parts[n-1] += tail
	body := strings.Join(parts, "\n\n---\n\n")
	return memory.Item{Kind: google.KindGmailThread, ProviderID: pid, Title: pid, Body: body, Meta: map[string]any{"messages": msgs}}, tail
}

// TestConnectorBodyBudgetKeeps40KBWholeAndCuts70KBAt64KB pins the Gmail ingest
// budget end to end: a 40 KB thread is stored whole and gets one segment per
// message; a 70 KB thread is cut to its first 64 KiB, marked Truncated, and
// fails closed to a "truncated" segment diagnostic.
func TestConnectorBodyBudgetKeeps40KBWholeAndCuts70KBAt64KB(t *testing.T) {
	if connectorBodyBudget != 64*1024 {
		t.Fatalf("connectorBodyBudget = %d, want %d", connectorBodyBudget, 64*1024)
	}
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	small, smallTail := budgetThread("budget40", 10, 40*1024)
	big, bigTail := budgetThread("budget70", 17, 70*1024)
	if len(small.Body) != 40*1024 || len(big.Body) != 70*1024 {
		t.Fatalf("fixture sizes: %d, %d", len(small.Body), len(big.Body))
	}

	ms := memory.MapItem(small, "personal", connectorBodyBudget)
	if ms.Truncated || ms.Body != small.Body || ms.IngestedSize != 40*1024 {
		t.Fatalf("40 KB thread must be kept whole: truncated=%v ingested=%d", ms.Truncated, ms.IngestedSize)
	}
	mb := memory.MapItem(big, "personal", connectorBodyBudget)
	if !mb.Truncated || len(mb.Body) != 64*1024 || mb.Body != big.Body[:64*1024] || mb.OriginalSize != 70*1024 {
		t.Fatalf("70 KB thread must be cut to its first 64 KiB: truncated=%v len=%d orig=%d", mb.Truncated, len(mb.Body), mb.OriginalSize)
	}
	if strings.Contains(mb.Body, bigTail) {
		t.Fatal("cut body must not contain the tail marker")
	}

	for _, mm := range []memory.MappedMemory{ms, mb} {
		if _, err := writeMappedMemoryDetailed(cfg, mm); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}

	rows := gmailSegRowsFor(t, cfg, ms.StableID)
	if len(rows) != 10 {
		t.Fatalf("40 KB thread: got %d segments, want 10", len(rows))
	}
	if !strings.HasSuffix(rows[9].Text, smallTail) {
		t.Fatalf("last segment must keep the thread tail, got ...%q", rows[9].Text[max(0, len(rows[9].Text)-40):])
	}
	if _, has := gmailSegDiagnosticFor(t, cfg, ms.StableID); has {
		t.Fatal("40 KB thread must not get a segment diagnostic")
	}

	if rows := gmailSegRowsFor(t, cfg, mb.StableID); len(rows) != 0 {
		t.Fatalf("cut thread must fail closed to zero segments, got %d", len(rows))
	}
	if d, has := gmailSegDiagnosticFor(t, cfg, mb.StableID); !has || d.Reason != gmailSegDiagTruncated {
		t.Fatalf("cut thread diagnostic = %+v (present %v), want %q", d, has, gmailSegDiagTruncated)
	}
}
