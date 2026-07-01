package mora

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func dgConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		VaultDir:  filepath.Join(root, "vault"),
		ConfigDir: filepath.Join(root, "config"),
		DataDir:   filepath.Join(root, "data"),
		StateDir:  filepath.Join(root, "state"),
	}
	for _, dir := range []string{cfg.VaultDir, cfg.ConfigDir, cfg.DataDir, cfg.StateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	return cfg
}

func dgMemory(id, provider, title, created string) Memory {
	return Memory{
		ID:          id,
		Scope:       "global",
		Type:        "note",
		Title:       title,
		Text:        title + " body",
		Provider:    provider,
		ProviderID:  provider + "/" + id,
		Source:      provider + "/" + id,
		ContentHash: "hash-" + id,
		CreatedAt:   created,
	}
}

func dgWriteBadMemoryFile(t *testing.T, cfg Config) {
	t.Helper()
	dir := filepath.Join(memoriesRoot(cfg), "global")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.md"), []byte("---\nnot yaml\n"), 0o600); err != nil {
		t.Fatalf("WriteFile bad memory: %v", err)
	}
}

func dgSectionBySource(d Digest, source string) (DigestSection, bool) {
	for _, s := range d.Sections {
		if s.Source == source {
			return s, true
		}
	}
	return DigestSection{}, false
}

func TestDg_BuildDigestErrorsOnWalkFailure(t *testing.T) {
	cfg := dgConfig(t)
	cfg.VaultDir = string([]byte{'b', 'a', 'd', 0, 'v', 'a', 'u', 'l', 't'})

	_, err := buildDigest(cfg, fixedNow, briefOpts{sinceHours: 24})
	if err == nil || !strings.Contains(err.Error(), "walking") {
		t.Fatalf("buildDigest error = %v, want surfaced walk error", err)
	}
}

func TestDg_WindowDigestSkipsMalformedInputsAndCollapsesServiceSenders(t *testing.T) {
	cfg := dgConfig(t)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	dgWriteBadMemoryFile(t, cfg)
	if err := writeMemory(cfg, dgMemory("bad-created", "gmail", "bad timestamp", "not-rfc3339")); err != nil {
		t.Fatalf("write bad timestamp memory: %v", err)
	}
	human := dgMemory("human", "gmail", "human sender", now.Add(-time.Hour).Format(time.RFC3339))
	human.Meta = map[string]any{"from": "friend@example.com"}
	if err := writeMemory(cfg, human); err != nil {
		t.Fatalf("write human memory: %v", err)
	}
	service := dgMemory("service", "gmail", "service sender", now.Add(-2*time.Hour).Format(time.RFC3339))
	service.Meta = map[string]any{"from": "no-reply@example.com"}
	if err := writeMemory(cfg, service); err != nil {
		t.Fatalf("write service memory: %v", err)
	}

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 24, perSourceCap: 10})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	sec, ok := dgSectionBySource(d, "gmail")
	if !ok {
		t.Fatalf("gmail section missing from digest: %+v", d.Sections)
	}
	if len(sec.Items) != 2 {
		t.Fatalf("gmail items=%d, want 2 valid timestamp items", len(sec.Items))
	}
	low := map[string]bool{}
	for _, it := range sec.Items {
		low[it.ID] = it.LowSignal
	}
	if low["service"] != true {
		t.Fatalf("service sender LowSignal=%v, want true", low["service"])
	}
	if low["human"] {
		t.Fatalf("human sender LowSignal=true, want false")
	}
}

func TestDg_DeltaDigestErrorAndFilterPaths(t *testing.T) {
	t.Run("filtered advance rejected before io", func(t *testing.T) {
		cfg := dgConfig(t)
		_, err := buildDeltaDigest(cfg, fixedNow, briefOpts{advance: true, source: "gmail"}, 10, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "preview-only") {
			t.Fatalf("buildDeltaDigest error = %v, want preview-only advance guard", err)
		}
	})

	t.Run("invalid sources json surfaces enumeration error", func(t *testing.T) {
		cfg := dgConfig(t)
		if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "sources.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatalf("WriteFile sources: %v", err)
		}
		_, err := buildDeltaDigest(cfg, fixedNow, briefOpts{}, 10, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "invalid character") {
			t.Fatalf("buildDeltaDigest error = %v, want json parse error", err)
		}
	})

	t.Run("source filter narrows enumerated sections", func(t *testing.T) {
		cfg := dgConfig(t)
		enableSources(t, cfg, "gmail", "imessage")
		seedSyncStatus(t, cfg, "gmail", fixedNow.Add(-time.Hour))
		seedSyncStatus(t, cfg, "imessage", fixedNow.Add(-time.Hour))

		d, err := buildDeltaDigest(cfg, fixedNow, briefOpts{source: "gmail"}, 10, nil, nil)
		if err != nil {
			t.Fatalf("buildDeltaDigest: %v", err)
		}
		if len(d.Sections) != 1 || d.Sections[0].Source != "gmail" {
			t.Fatalf("sections = %+v, want only gmail", d.Sections)
		}
	})

	t.Run("advance reports lock acquisition failure", func(t *testing.T) {
		cfg := dgConfig(t)
		enableSources(t, cfg, "gmail")
		if err := os.WriteFile(filepath.Join(cfg.StateDir, "brief"), []byte("not a dir"), 0o600); err != nil {
			t.Fatalf("WriteFile brief blocker: %v", err)
		}
		_, err := buildDeltaDigest(cfg, fixedNow, briefOpts{advance: true}, 10, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "brief commit in progress") {
			t.Fatalf("buildDeltaDigest error = %v, want lock acquisition error", err)
		}
	})

	t.Run("advance reports snapshot write failure", func(t *testing.T) {
		cfg := dgConfig(t)
		sources := []Source{{
			Name:    "gmail",
			Type:    "gmail",
			Account: string([]byte{'b', 'a', 'd', 0, 'a', 'c', 'c', 't'}),
			Enabled: ptr(true),
		}}
		if err := saveSources(cfg, sources); err != nil {
			t.Fatalf("saveSources: %v", err)
		}
		_, err := buildDeltaDigest(cfg, fixedNow, briefOpts{advance: true}, 10, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "commit watermark") {
			t.Fatalf("buildDeltaDigest error = %v, want snapshot commit error", err)
		}
	})
}

func TestDg_DeltaSectionItemsColdAndSteadyEdges(t *testing.T) {
	cfg := dgConfig(t)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	coldMems := []Memory{
		dgMemory("bad-cold", "gmail", "bad cold", "not-rfc3339"),
		dgMemory("good-cold", "gmail", "good cold", now.Add(-time.Hour).Format(time.RFC3339)),
	}
	cold, shownIDs, more := deltaSectionItems(cfg, briefDelta{ColdStart: true}, coldMems, now, "gmail", 10, nil)
	if len(cold) != 1 || cold[0].ID != "good-cold" || len(shownIDs) != 0 || more != 0 {
		t.Fatalf("cold items=%+v shown=%v more=%d, want only good cold item and no shown ids", cold, shownIDs, more)
	}

	seriesA := dgMemory("series-a", "calendar", "standup a", now.Add(time.Hour).Format(time.RFC3339))
	seriesA.Meta = map[string]any{"recurring_event_id": "series-1"}
	seriesB := dgMemory("series-b", "calendar", "standup b", now.Add(30*time.Minute).Format(time.RFC3339))
	seriesB.Meta = map[string]any{"recurring_event_id": "series-1"}
	bad := dgMemory("bad-steady", "calendar", "bad steady", "not-rfc3339")
	mems := []Memory{seriesA, seriesB, bad}
	delta := briefDelta{Items: []briefDeltaItem{
		{ID: "missing", Change: "new"},
		{ID: "series-a", Change: "new"},
		{ID: "series-b", Change: "new"},
		{ID: "bad-steady", Change: "updated"},
	}}

	items, shown, more := deltaSectionItems(cfg, delta, mems, now, "calendar", 10, map[string]int64{"series-b": 9})
	if more != 0 {
		t.Fatalf("more=%d, want 0", more)
	}
	if shown["missing"] {
		t.Fatalf("missing delta id was marked shown")
	}
	if !shown["series-a"] || !shown["series-b"] || !shown["bad-steady"] {
		t.Fatalf("shown ids=%v, want recurring members and bad timestamp item acknowledged", shown)
	}
	if len(items) != 2 {
		t.Fatalf("items=%+v, want collapsed series plus bad timestamp item", items)
	}
	foundSeries := false
	for _, it := range items {
		if it.ID == "series-a" || it.ID == "series-b" {
			foundSeries = true
			if !strings.Contains(it.Title, "through") {
				t.Fatalf("collapsed series title %q does not include span", it.Title)
			}
		}
	}
	if !foundSeries {
		t.Fatalf("collapsed recurring series missing from items: %+v", items)
	}
}

func TestDg_RecurringSeriesRepresentativeBranches(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	if !betterSeriesRep(now.Add(time.Hour), now.Add(-time.Hour), now) {
		t.Fatalf("future occurrence should beat past occurrence")
	}
	if !betterSeriesRep(now.Add(time.Hour), now.Add(2*time.Hour), now) {
		t.Fatalf("earlier future occurrence should beat later future occurrence")
	}
	if !betterSeriesRep(now.Add(-time.Hour), now.Add(-2*time.Hour), now) {
		t.Fatalf("later past occurrence should beat older past occurrence")
	}

	one := []tsItem{{item: DigestItem{ID: "only"}, ts: now, series: "solo"}}
	got := collapseRecurringSeries(one, now)
	if len(got) != 1 || got[0].item.ID != "only" || len(got[0].members) != 0 {
		t.Fatalf("single recurring item = %+v, want pass-through without members", got)
	}
}

func TestDg_SyncStatusLookupPathsAndFailures(t *testing.T) {
	cfg := dgConfig(t)
	if got := loadConnectorSyncStatus(Config{ConfigDir: string([]byte{'b', 'a', 'd', 0})}, "gmail"); got != nil {
		t.Fatalf("loadConnectorSyncStatus with invalid config dir = %+v, want nil", got)
	}

	if err := saveSources(cfg, []Source{
		{Name: "off", Type: "gmail", Enabled: ptr(false)},
		{Name: "custom", Type: "custom", Enabled: ptr(true)},
	}); err != nil {
		t.Fatalf("saveSources: %v", err)
	}
	if got := loadConnectorSyncStatus(cfg, "custom"); got != nil {
		t.Fatalf("custom source status = %+v, want nil for unknown status path", got)
	}
	if got := loadConnectorSyncStatus(cfg, "gmail"); got != nil {
		t.Fatalf("disabled gmail status = %+v, want nil", got)
	}

	if err := saveSources(cfg, []Source{{Name: "gmail", Type: "gmail", Enabled: ptr(true)}}); err != nil {
		t.Fatalf("saveSources gmail: %v", err)
	}
	badStatusPath := syncStatusPathFor(cfg, Source{Name: "gmail", Type: "gmail"})
	if err := os.MkdirAll(filepath.Dir(badStatusPath), 0o700); err != nil {
		t.Fatalf("MkdirAll status dir: %v", err)
	}
	if err := os.WriteFile(badStatusPath, []byte("{bad json"), 0o600); err != nil {
		t.Fatalf("WriteFile status: %v", err)
	}
	if got := loadConnectorSyncStatus(cfg, "gmail"); got != nil {
		t.Fatalf("bad status load = %+v, want nil", got)
	}

	cases := []struct {
		src  Source
		want string
	}{
		{src: Source{Name: "cal", Type: "applecalendar"}, want: filepath.Join(cfg.StateDir, "sync", "applecal-cal.json")},
		{src: Source{Name: "fs", Type: "filesystem"}, want: filepath.Join(cfg.StateDir, "sync", "filesystem-fs.json")},
		{src: Source{Name: "x", Type: "unknown"}, want: ""},
	}
	for _, tc := range cases {
		if got := syncStatusPathFor(cfg, tc.src); got != tc.want {
			t.Fatalf("syncStatusPathFor(%+v)=%q, want %q", tc.src, got, tc.want)
		}
	}
}

func TestDg_RenderLabelsAndBudgetEdges(t *testing.T) {
	sections := []DigestSection{
		{Source: "zeta", Items: []DigestItem{{ID: "z", Title: "Z"}}},
		{Source: "alpha", Items: []DigestItem{{ID: "a", Title: "A"}}},
	}
	sortSections(sections)
	if got := []string{sections[0].Source, sections[1].Source}; !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Fatalf("unknown section sort = %v, want lexical tie-break", got)
	}

	headings := []string{
		sectionHeading(DigestSection{Source: "gmail", State: stateNoChanges}),
		sectionHeading(DigestSection{Source: "gmail", State: stateStale}),
		sectionHeading(DigestSection{Source: "gmail", State: stateUnavailable}),
		sectionHeading(DigestSection{Source: "gmail", State: stateColdStart, Items: []DigestItem{{ID: "1"}}}),
	}
	for _, want := range []string{"no changes", "stale", "unavailable", "baseline"} {
		found := false
		for _, h := range headings {
			found = found || strings.Contains(h, want)
		}
		if !found {
			t.Fatalf("headings %q do not contain %q", headings, want)
		}
	}
	if got := changePrefix("updated"); got != "[updated] " {
		t.Fatalf("changePrefix(updated)=%q", got)
	}
	if got := mcpStateLabel(stateNoChanges); got != "no_change" {
		t.Fatalf("mcpStateLabel(no changes)=%q", got)
	}

	out := renderDigest(Digest{
		Generated:  fixedNow.Format(time.RFC3339),
		SinceHours: 3,
		StaleTasks: []string{"old task"},
		Sections: []DigestSection{{
			Source:    "gmail",
			Items:     []DigestItem{{ID: "id1", Title: "Title", Snippet: "Snippet", Change: "updated"}},
			MoreCount: 2,
		}},
	}, 0)
	for _, want := range []string{"last 3h", "[updated] Title", "+2 more since last brief", "Open tasks", "old task"} {
		if !strings.Contains(out, want) {
			t.Fatalf("renderDigest output missing %q:\n%s", want, out)
		}
	}
}

func TestDg_BudgetSectionsShellsAndJSONFallback(t *testing.T) {
	sections := []DigestSection{
		{Source: "gmail", State: stateDelta, Items: []DigestItem{{ID: "a"}, {ID: "b"}}, MoreCount: 1},
		{Source: "imessage", State: stateNoChanges, Items: []DigestItem{{ID: "c"}}},
	}
	got := budgetSections(sections, -1)
	if len(got) != 2 {
		t.Fatalf("budgetSections len=%d, want 2 shells", len(got))
	}
	if !got[0].Truncated || got[0].MoreCount != 3 || len(got[0].Items) != 0 {
		t.Fatalf("first shell = %+v, want all gmail items counted as more", got[0])
	}
	if !got[1].Truncated || got[1].MoreCount != 1 || len(got[1].Items) != 0 {
		t.Fatalf("second shell = %+v, want exhausted shell", got[1])
	}
	if got := jsonLen(func() {}); got != 0 {
		t.Fatalf("jsonLen(unmarshalable func)=%d, want 0", got)
	}
}

func TestDg_EnvelopeZeroValueConversions(t *testing.T) {
	if got := asString(42); got != "" {
		t.Fatalf("asString(non-string)=%q, want empty", got)
	}
	if got := asInt("42"); got != 0 {
		t.Fatalf("asInt(non-int)=%d, want 0", got)
	}
	if got := asStringMap(map[string]int{"x": 1}); got != nil {
		t.Fatalf("asStringMap(wrong map)=%v, want nil", got)
	}
	if got := asStringSlice([]int{1}); got != nil {
		t.Fatalf("asStringSlice(wrong slice)=%v, want nil", got)
	}

}

func TestDg_BuildSourceStatesReadsErroredStatus(t *testing.T) {
	cfg := dgConfig(t)
	enableSources(t, cfg, "gmail")
	seedSyncStatusFull(t, cfg, "gmail", &memory.SyncStatus{
		Source:        "gmail",
		LastSynced:    fixedNow.Format(time.RFC3339),
		LastSuccessAt: fixedNow.Format(time.RFC3339),
		LastError:     "boom",
		ErrorCount:    2,
	})
	states := buildSourceStates(cfg, Digest{Sections: []DigestSection{{Source: "gmail", State: stateUnavailable, MoreCount: 2}}})
	if len(states) != 1 {
		t.Fatalf("states len=%d, want 1", len(states))
	}
	if !states[0].Errored || states[0].LastSynced == "" || states[0].Count != 2 || states[0].State != "unavailable" {
		t.Fatalf("state = %+v, want errored unavailable with count and last_synced", states[0])
	}
}
