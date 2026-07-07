package mora

import "testing"

// `mora connect imessage --since-days N` must persist the backlog window onto the
// imessage source (mirroring `connect google --since-days`), so the depth is really
// flag-customizable — not pinned to the 90-day default. The flag is persisted before
// the FDA-readiness gate, so this holds even where chat.db is absent (temp HOME).
func TestConnectIMessageSinceDaysPersists(t *testing.T) {
	asDarwinOnWindows(t)
	withTempHome(t)
	run(t, "init")
	run(t, "connect", "imessage", "--since-days", "365")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	sources, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	found := false
	for _, s := range sources {
		if s.Type == "imessage" {
			found = true
			if s.SinceDays != 365 {
				t.Fatalf("imessage SinceDays = %d, want 365 (flag not persisted)", s.SinceDays)
			}
		}
	}
	if !found {
		t.Fatal("no imessage source after `connect imessage`")
	}
}

// A negative window selects all-time (windowForIMessage treats <0 as no lower bound).
func TestConnectIMessageSinceDaysAllTime(t *testing.T) {
	asDarwinOnWindows(t)
	withTempHome(t)
	run(t, "init")
	run(t, "connect", "imessage", "--since-days", "-1")

	cfg, _ := loadConfig()
	sources, _ := loadSources(cfg)
	for _, s := range sources {
		if s.Type == "imessage" && s.SinceDays != -1 {
			t.Fatalf("imessage SinceDays = %d, want -1 (all-time)", s.SinceDays)
		}
	}
}
