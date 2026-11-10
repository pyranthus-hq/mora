package search

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func rrFile(i int) memory.Memory {
	return memory.Memory{ID: fmt.Sprintf("file/%02d", i), Type: "note", Provider: "filesystem", Score: float64(-50 + i)}
}

func rrNote(id, typ string, tags ...string) memory.Memory {
	return memory.Memory{ID: id, Type: typ, Source: "mcp", Tags: tags, Title: id, Text: id + " text", Score: -1}
}

func rrConfirmed(m memory.Memory) memory.Memory {
	m.Tags = append(m.Tags, memory.OwnerConfirmedTagPrefix+memory.OwnerConfirmedDigest(m))
	return m
}

func rrIDs(rows []memory.Memory) []string {
	ids := make([]string, len(rows))
	for i, m := range rows {
		ids[i] = m.ID
	}
	return ids
}

func rrIndex(rows []memory.Memory, id string) int {
	for i, m := range rows {
		if m.ID == id {
			return i
		}
	}
	return -1
}

func TestRerankDecisionBehindThreeLongFilesGoesFirst(t *testing.T) {
	rows := []memory.Memory{rrFile(0), rrFile(1), rrFile(2), rrNote("mem_dec", "decision")}
	got := RerankByMetadata(rows)
	if got[0].ID != "mem_dec" {
		t.Fatalf("order = %v, want the decision first", rrIDs(got))
	}
}

func TestRerankWeakDecisionAtFifteenDoesNotPassStrongMatches(t *testing.T) {
	var rows []memory.Memory
	for i := 0; i < 15; i++ {
		rows = append(rows, rrFile(i))
	}
	rows = append(rows, rrConfirmed(rrNote("mem_dec", "decision")), rrFile(15))
	got := RerankByMetadata(rows)
	if at := rrIndex(got, "mem_dec"); at < 3 {
		t.Fatalf("decision moved from 15 to %d; it must stay behind the strong matches at 0 to 2", at)
	}
	for i := 0; i < 3; i++ {
		if got[i].ID != rows[i].ID {
			t.Fatalf("order = %v, want the strong matches first", rrIDs(got))
		}
	}
}

func TestRerankOwnerConfirmedBeatsUnconfirmedAndTentativeFallsBehind(t *testing.T) {
	plain := rrNote("mem_a", "decision")
	conf := rrConfirmed(rrNote("mem_b", "decision"))
	got := RerankByMetadata([]memory.Memory{plain, conf})
	if got[0].ID != "mem_b" {
		t.Fatalf("order = %v, want the confirmed decision first", rrIDs(got))
	}
	tent := rrNote("mem_t", "fact", memory.TentativeTag)
	eq := rrNote("mem_c", "fact")
	got = RerankByMetadata([]memory.Memory{tent, eq})
	if got[0].ID != "mem_c" {
		t.Fatalf("order = %v, want the tentative note behind the equal note", rrIDs(got))
	}
	got = RerankByMetadata([]memory.Memory{tent, rrConfirmed(rrNote("mem_d", "fact"))})
	if got[0].ID != "mem_d" {
		t.Fatalf("order = %v, want the tentative note behind the confirmed note", rrIDs(got))
	}
}

func TestRerankTiePromotedWinsAgainstZeroBonus(t *testing.T) {
	// File at 0 and a fact (bonus 1) at 1 share key 0; the promoted row goes first.
	got := RerankByMetadata([]memory.Memory{rrFile(0), rrNote("mem_f", "fact")})
	if got[0].ID != "mem_f" {
		t.Fatalf("order = %v", rrIDs(got))
	}
	// Equal rows keep the original order.
	got = RerankByMetadata([]memory.Memory{rrFile(0), rrFile(1), rrFile(2)})
	if !reflect.DeepEqual(rrIDs(got), []string{"file/00", "file/01", "file/02"}) {
		t.Fatalf("order = %v", rrIDs(got))
	}
}

func TestRerankRowsInRowsOut(t *testing.T) {
	for _, n := range []int{0, 1, 50} {
		var rows []memory.Memory
		for i := 0; i < n; i++ {
			switch i % 5 {
			case 0:
				rows = append(rows, rrNote(fmt.Sprintf("mem_%02d", i), "decision"))
			case 1:
				rows = append(rows, rrNote(fmt.Sprintf("mem_%02d", i), "fact", memory.TentativeTag))
			case 2:
				rows = append(rows, rrConfirmed(rrNote(fmt.Sprintf("mem_%02d", i), "correction")))
			default:
				rows = append(rows, rrFile(i))
			}
		}
		before := append([]memory.Memory(nil), rows...)
		got := RerankByMetadata(rows)
		if !reflect.DeepEqual(rows, before) {
			t.Fatalf("n=%d: input changed", n)
		}
		if len(got) != n {
			t.Fatalf("n=%d: got %d rows", n, len(got))
		}
		byID := map[string]memory.Memory{}
		for _, m := range got {
			if _, dup := byID[m.ID]; dup {
				t.Fatalf("n=%d: duplicate %s", n, m.ID)
			}
			byID[m.ID] = m
		}
		for _, m := range before {
			if g, ok := byID[m.ID]; !ok || !reflect.DeepEqual(g, m) {
				t.Fatalf("n=%d: row %s missing or changed", n, m.ID)
			}
		}
	}
}

func TestRerankBoundedMovement(t *testing.T) {
	var rows []memory.Memory
	for i := 0; i < 50; i++ {
		switch i % 4 {
		case 0:
			rows = append(rows, rrNote(fmt.Sprintf("mem_%02d", i), "decision"))
		case 1:
			rows = append(rows, rrNote(fmt.Sprintf("mem_%02d", i), "fact", memory.TentativeTag))
		case 2:
			rows = append(rows, rrConfirmed(rrNote(fmt.Sprintf("mem_%02d", i), "correction")))
		default:
			rows = append(rows, rrFile(i))
		}
	}
	got := RerankByMetadata(rows)
	maxUp := rerankCorrection + rerankOwnerConfirmed
	for from, m := range rows {
		to := rrIndex(got, m.ID)
		net := rerankNet(m)
		up := max(net, 0)
		// Up: own bonus, plus at most one place per tentative row it passes.
		if from-to > up+rerankTentative {
			t.Fatalf("%s moved up %d with bonus %d", m.ID, from-to, up)
		}
		if to-from > max(-net, 0)+maxUp {
			t.Fatalf("%s moved down %d", m.ID, to-from)
		}
	}
}

func TestRerankDeterministic(t *testing.T) {
	rows := []memory.Memory{rrFile(0), rrNote("mem_a", "decision"), rrFile(1), rrNote("mem_b", "fact", memory.TentativeTag), rrConfirmed(rrNote("mem_c", "correction")), rrFile(2), rrFile(3), rrFile(4), rrNote("mem_d", "insight")}
	want := rrIDs(RerankByMetadata(rows))
	for i := 0; i < 100; i++ {
		if got := rrIDs(RerankByMetadata(rows)); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: %v != %v", i, got, want)
		}
	}
}

func TestRerankPoolKeepsFilteredSlotAndSubsequence(t *testing.T) {
	// "gone" was filtered out of visible but still holds rawIDs position 1.
	raw := []string{"file/00", "gone", "file/01", "file/02", "mem_dec"}
	visible := []memory.Memory{rrFile(0), rrFile(1), rrFile(2), rrNote("mem_dec", "decision")}
	nextRaw, ranked := RerankPool(raw, visible)
	if nextRaw[1] != "gone" {
		t.Fatalf("filtered row lost its slot: %v", nextRaw)
	}
	var sub []string
	for _, id := range nextRaw {
		if id != "gone" {
			sub = append(sub, id)
		}
	}
	if !reflect.DeepEqual(sub, rrIDs(ranked)) {
		t.Fatalf("visible is not a subsequence of rawIDs: raw %v visible %v", nextRaw, rrIDs(ranked))
	}
	if ranked[0].ID != "mem_dec" {
		t.Fatalf("order = %v", rrIDs(ranked))
	}
}
