package health

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProjectCompactOrderingCapsAndDeterminism(t *testing.T) {
	longKey := strings.Repeat("escaped_\"_日本語_", 12)
	h := Health{State: Degraded, Sources: []Source{
		{Key: "src_young", State: Stale, AgeHours: 5},
		{Key: "src_oldest", State: Stale, AgeHours: 100},
		{Key: "src_older", State: Stale, AgeHours: 50},
		{Key: "src_newest", State: Stale, AgeHours: 1},
		{Key: "failed", State: Failed},
		{Key: longKey, State: Failed},
	}, Index: Index{State: IndexFresh}}
	got1, got2 := ProjectCompact(h), ProjectCompact(h)
	if got1.State != Degraded || got1.Sources != Failed || got1.Index != IndexFresh {
		t.Fatal(got1)
	}
	if len(got1.PerSource) != CompactSourceCap {
		t.Fatalf("per source = %#v", got1.PerSource)
	}
	if got1.PerSource["failed"] != Failed || got1.PerSource["src_oldest"] != Stale || got1.PerSource["src_older"] != Stale {
		t.Fatalf("selection = %#v", got1.PerSource)
	}
	if _, ok := got1.PerSource[longKey]; ok {
		t.Fatal("oversized key admitted")
	}
	if got1.SourcesOmitted != 3 {
		t.Fatalf("omitted=%d", got1.SourcesOmitted)
	}
	mapBody, _ := json.Marshal(got1.PerSource)
	if len(mapBody) > CompactSourceBytesCap {
		t.Fatalf("map bytes=%d", len(mapBody))
	}
	body1, _ := json.Marshal(got1)
	body2, _ := json.Marshal(got2)
	if string(body1) != string(body2) {
		t.Fatalf("nondeterministic: %s != %s", body1, body2)
	}
	if h.Sources[0].Key != "src_young" {
		t.Fatal("projection mutated input order")
	}
}

func TestProjectCompactExactKeysBannerAndEmpty(t *testing.T) {
	if got := ProjectCompact(Health{State: Healthy, Index: Index{State: IndexFresh}}); got.Sources != Fresh || got.PerSource != nil || got.Banner != "" {
		t.Fatal(got)
	}
	h := Health{State: Degraded, Sources: []Source{{Key: "shared_prefix_alpha", State: Stale}, {Key: "shared_prefix_beta", State: Stale}}, Index: Index{State: IndexFresh}}
	got := ProjectCompact(h)
	if got.PerSource["shared_prefix_alpha"] != Stale || got.PerSource["shared_prefix_beta"] != Stale || got.SourcesOmitted != 0 {
		t.Fatal(got)
	}
	if got.Banner == "" {
		t.Fatal("missing alarm banner")
	}
}

func TestFromPartsAggregatesAllArms(t *testing.T) {
	got := FromParts([]Source{{State: Fresh}}, Index{State: IndexFresh}, []Producer{{State: ProducerStale, Subject: ProducerSubjectProducer}})
	if got.State != Degraded || len(got.Sources) != 1 || len(got.Producers) != 1 {
		t.Fatal(got)
	}
	failed := FromParts(nil, Index{State: IndexFailed}, nil)
	if failed.State != Unhealthy {
		t.Fatal(failed)
	}
}
