package mora

import (
	"testing"
	"time"
)

// TestDigestSourceFilter locks the per-source rundown contract: digest with a
// source filter returns ONLY the matching connector instance's section, so an
// "iMessage for the past week" ask can't be starved by calendar sections that
// rank earlier and eat the byte budget (the live 2026-06-10 flood: recurring
// events truncated the imessage section entirely). The filter matches the
// instance key exactly AND its provider family ("gmail" also selects
// "gmail:work") so multi-account mailboxes stay reachable by family.
func TestDigestSourceFilter(t *testing.T) {
	if !digestSourceMatches("imessage", "imessage") {
		t.Fatalf("exact key must match")
	}
	if !digestSourceMatches("gmail:work", "gmail") {
		t.Fatalf("provider family must select account instances")
	}
	if digestSourceMatches("calendar", "imessage") || digestSourceMatches("applecalendar", "calendar") {
		t.Fatalf("non-matching keys must be excluded (applecalendar is NOT in the calendar family)")
	}
	if !digestSourceMatches("gmail:work", "gmail:work") {
		t.Fatalf("exact composite key must match")
	}
	if digestSourceMatches("gmail", "gmail:work") {
		t.Fatalf("an account-scoped filter must not select the default instance")
	}
	// Empty filter = no filtering (every key passes).
	if !digestSourceMatches("anything", "") {
		t.Fatalf("empty filter must pass everything")
	}
}

// TestWindowDigestHonorsSourceFilter proves the filter end-to-end on the
// window path: two instances in, one section out.
func TestWindowDigestHonorsSourceFilter(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	now, err := time.Parse(time.RFC3339, "2026-06-10T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	byInstance := map[string][]Memory{
		"imessage": {{ID: "m1", Title: "Kai", Provider: "imessage", Text: "hey", CreatedAt: "2026-06-09T10:00:00Z"}},
		"calendar": {{ID: "c1", Title: "Standup", Provider: "calendar", Text: "daily", CreatedAt: "2026-06-09T11:00:00Z"}},
	}
	d, err := buildWindowDigest(cfg, now, 168, 8, byInstance, nil, "imessage")
	if err != nil {
		t.Fatalf("buildWindowDigest: %v", err)
	}
	if len(d.Sections) != 1 || d.Sections[0].Source != "imessage" {
		t.Fatalf("want only the imessage section, got %+v", d.Sections)
	}
}

// TestDigestSourceFilterAcceptsProviderAlias: "applecal" is the string a user
// actually sees on disk (frontmatter `provider: applecal`, the sources/applecal/
// directory) and it matched in window mode before the keying fix. The filter
// must normalize through the same provider→type alias as the keying seam, so
// `--source applecal` selects the applecalendar section instead of silently
// returning an empty digest.
func TestDigestSourceFilterAcceptsProviderAlias(t *testing.T) {
	if !digestSourceMatches("applecalendar", "applecal") {
		t.Fatal("--source applecal (the on-disk provider string) must match the applecalendar section")
	}
	if !digestSourceMatches("applecalendar", "applecalendar") {
		t.Fatal("the documented type string must keep matching")
	}
	if digestSourceMatches("calendar", "applecal") {
		t.Fatal("the alias must not bleed into other sections")
	}
}
