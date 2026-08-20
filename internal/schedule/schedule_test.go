package schedule

import (
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
)

func TestCoreB_UtilSchedulePlistFor(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	t.Setenv("MORA_CONFIG_DIR", "")

	// Unknown job → ("", false).
	if plist, ok := PlistFor(cfg, "does-not-exist"); ok || plist != "" {
		t.Fatalf("unknown job = (%q,%v), want (\"\",false)", plist, ok)
	}

	// Known job: label + program args + RunAtLoad + schedule fragment.
	plist, ok := PlistFor(cfg, "index-hourly")
	if !ok {
		t.Fatal("index-hourly must be a known job")
	}
	for _, want := range []string{
		"<string>com.mora.index-hourly</string>",
		"<string>index</string><string>rebuild</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>StartInterval</key><integer>3600</integer>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("index-hourly plist missing %q:\n%s", want, plist)
		}
	}
	// No env vars set → no EnvironmentVariables dict.
	if strings.Contains(plist, "EnvironmentVariables") {
		t.Fatalf("plist must omit EnvironmentVariables when no env set:\n%s", plist)
	}

	// pulse-daily drops RunAtLoad and uses a calendar interval.
	pulse, ok := PlistFor(cfg, "pulse-daily")
	if !ok {
		t.Fatal("pulse-daily must be a known job")
	}
	if strings.Contains(pulse, "RunAtLoad") {
		t.Fatalf("pulse-daily must NOT set RunAtLoad:\n%s", pulse)
	}
	if !strings.Contains(pulse, "StartCalendarInterval") {
		t.Fatalf("pulse-daily must use StartCalendarInterval:\n%s", pulse)
	}

	// Env snapshot: both PATHs embedded into an EnvironmentVariables dict.
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "/creds/g.json")
	t.Setenv("MORA_CONFIG_DIR", "/scratch/mora")
	withEnv, _ := PlistFor(cfg, "index-hourly")
	if !strings.Contains(withEnv, "<key>EnvironmentVariables</key>") {
		t.Fatalf("plist missing EnvironmentVariables dict:\n%s", withEnv)
	}
	if !strings.Contains(withEnv, "<key>MORA_GOOGLE_CREDENTIALS</key><string>/creds/g.json</string>") {
		t.Fatalf("plist missing creds env:\n%s", withEnv)
	}
	if !strings.Contains(withEnv, "<key>MORA_CONFIG_DIR</key><string>/scratch/mora</string>") {
		t.Fatalf("plist missing config-dir env:\n%s", withEnv)
	}
}

// ---------------------------------------------------------------------------
// installSchedule + listSchedules (launchd assertions are darwin-only).
// ---------------------------------------------------------------------------

func TestCoreB_UtilLaunchdSchedule(t *testing.T) {
	cases := map[string]string{
		"index-hourly":  "<key>StartInterval</key><integer>3600</integer>",
		"ingest-hourly": "<key>StartInterval</key><integer>3600</integer>",
		"pulse-daily":   "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>8</integer><key>Minute</key><integer>0</integer></dict>",
		"backup-daily":  "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>2</integer><key>Minute</key><integer>0</integer></dict>",
		"git-daily":     "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>0</integer></dict>",
		"lint-weekly":   "<key>StartCalendarInterval</key><dict><key>Weekday</key><integer>0</integer><key>Hour</key><integer>9</integer><key>Minute</key><integer>0</integer></dict>",
		"unknown-job":   "<key>StartInterval</key><integer>3600</integer>", // default
	}
	for job, want := range cases {
		if got := LaunchdSchedule(job); got != want {
			t.Errorf("LaunchdSchedule(%q) = %q, want %q", job, got, want)
		}
	}
	// The calendar jobs must differ from the interval jobs.
	if LaunchdSchedule("pulse-daily") == LaunchdSchedule("index-hourly") {
		t.Fatal("pulse-daily and index-hourly schedules must differ")
	}
	if LaunchdSchedule("backup-daily") == LaunchdSchedule("git-daily") {
		t.Fatal("backup-daily and git-daily schedules must differ (different Hour)")
	}
}

// ---------------------------------------------------------------------------
// extractDocxText — happy path + tab/br/cr + error branches.
// ---------------------------------------------------------------------------
