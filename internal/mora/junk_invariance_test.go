package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var sabotageCleanFixtureFiles = map[string]bool{
	"briefing-event.md":     true,
	"genuine-obligation.md": true,
}

func renderMeetingBriefBytes(t *testing.T, brief MeetingBrief) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := renderMeetingBrief(&out, brief); err != nil {
		t.Fatalf("renderMeetingBrief: %v", err)
	}
	return out.Bytes()
}

func buildSabotageMeetingBrief(t *testing.T, cfg Config, event sabotageEventFixture, at time.Time) MeetingBrief {
	t.Helper()
	brief, err := buildEventMeetingBrief(context.Background(), cfg, event.EventID, at, 0, 8)
	if err != nil {
		t.Fatalf("buildEventMeetingBrief: %v", err)
	}
	return brief
}

func meetingBriefKindBytes(t *testing.T, brief MeetingBrief, kinds ...string) []byte {
	t.Helper()
	want := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		want[kind] = true
	}
	var selected []MeetingBriefSection
	for _, section := range brief.Sections {
		if want[section.Kind] {
			selected = append(selected, section)
		}
	}
	b, err := json.Marshal(selected)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// redateSabotageJunk makes every non-control fixture newer than the newest
// genuine evidence while leaving the committed INPUT fixture untouched.
func redateSabotageJunk(t *testing.T, cfg Config, occurred time.Time) {
	t.Helper()
	for _, pattern := range sabotageJunkPatterns {
		if sabotageCleanFixtureFiles[pattern.sourceFixture] {
			continue
		}
		path := filepath.Join(cfg.VaultDir, "sources", providerDirForSabotageFixture(pattern.sourceFixture), pattern.sourceFixture)
		m, err := parseMemory(path)
		if err != nil {
			t.Fatalf("parse %s for redating: %v", pattern.sourceFixture, err)
		}
		m.CreatedAt = occurred.UTC().Format(time.RFC3339)
		if m.Meta == nil {
			m.Meta = map[string]any{}
		}
		m.Meta["occurred_at"] = m.CreatedAt
		body, err := renderMemory(m)
		if err != nil {
			t.Fatalf("render redated %s: %v", pattern.sourceFixture, err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write redated %s: %v", pattern.sourceFixture, err)
		}
	}
}

func providerDirForSabotageFixture(base string) string {
	if strings.HasPrefix(base, "imessage-") {
		return "imessage"
	}
	return "gmail"
}

type digestJunkInvariant struct {
	Urgent     []DigestItem      `json:"urgent"`
	UrgentMore int               `json:"urgent_more"`
	Freshness  map[string]string `json:"freshness"`
	StaleTasks []string          `json:"stale_tasks"`
}

func digestInvariantOf(d Digest) digestJunkInvariant {
	return digestJunkInvariant{
		Urgent:     d.Urgent,
		UrgentMore: d.UrgentMore,
		Freshness:  d.Freshness,
		StaleTasks: d.StaleTasks,
	}
}

func TestMeetingBriefJunkInvariance(t *testing.T) {
	cfg, event, at := seedSabotageHome(t, sabotageCleanFixtureFiles)
	cleanBrief := buildSabotageMeetingBrief(t, cfg, event, at)
	bytesA := renderMeetingBriefBytes(t, cleanBrief)
	cleanDigest, err := buildDigest(cfg, at, briefOpts{})
	if err != nil {
		t.Fatalf("build clean digest: %v", err)
	}

	// Every committed junk fixture is older than the newest genuine attendee
	// evidence. This timestamp constraint is load-bearing: PersonLastSeen is
	// computed from all dossier evidence before junk filtering, so newer junk may
	// legitimately alter dormancy scores/ranking even when no junk line renders.
	copySabotageVault(t, cfg, nil)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuild with older junk: %v", err)
	}
	withJunk := buildSabotageMeetingBrief(t, cfg, event, at)
	bytesB := renderMeetingBriefBytes(t, withJunk)
	if !bytes.Equal(bytesA, bytesB) {
		t.Fatalf("older junk changed assembled meeting-brief bytes\n--- clean ---\n%s\n--- with junk ---\n%s", bytesA, bytesB)
	}
	assertSabotageBriefPasses(t, "older-junk invariance", withJunk)

	withJunkDigest, err := buildDigest(cfg, at, briefOpts{})
	if err != nil {
		t.Fatalf("build digest with junk: %v", err)
	}
	cleanDigestBytes, _ := json.Marshal(cleanDigest)
	withJunkDigestBytes, _ := json.Marshal(withJunkDigest)
	if !bytes.Equal(cleanDigestBytes, withJunkDigestBytes) {
		t.Fatalf("non-commitment junk changed the commitment-gated daily digest\nclean: %s\nwith junk: %s", cleanDigestBytes, withJunkDigestBytes)
	}

	// The typed inventory now gates the whole digest, so non-commitment junk is
	// byte-invariant across every lane, including the previously exposed delta.
	if !reflect.DeepEqual(digestInvariantOf(cleanDigest), digestInvariantOf(withJunkDigest)) {
		t.Fatalf("junk changed protected digest lanes\nclean: %+v\nwith junk: %+v", digestInvariantOf(cleanDigest), digestInvariantOf(withJunkDigest))
	}

	// Name the meeting-brief lanes explicitly too: these are the staleness-guard
	// and open-loop lines whose full artifact is protected by the stronger check.
	if !bytes.Equal(
		meetingBriefKindBytes(t, cleanBrief, meetingBriefOpenLoops, meetingBriefStaleness),
		meetingBriefKindBytes(t, withJunk, meetingBriefOpenLoops, meetingBriefStaleness),
	) {
		t.Fatal("junk changed meeting-brief open-loop or staleness-guard lines")
	}
}

func TestMeetingBriefJunkInvarianceNewerJunk(t *testing.T) {
	cfg, event, at := seedSabotageHome(t, sabotageCleanFixtureFiles)
	copySabotageVault(t, cfg, nil)

	// Newer-than-genuine junk is intentionally outside byte-invariance: it can
	// shift PersonLastSeen, dormancy, ranking, and caps before filtering. The
	// honest invariant is line-level exclusion plus a non-empty genuine control.
	redateSabotageJunk(t, cfg, at.Add(-4*time.Hour))
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuild with newer junk: %v", err)
	}
	brief := buildSabotageMeetingBrief(t, cfg, event, at)
	assertSabotageBriefPasses(t, "newer-junk line invariance", brief)
}
