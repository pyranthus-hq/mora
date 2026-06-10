package mora

import (
	"strings"
	"testing"
)

// TestSchedulePlistCarriesGoogleCredsEnv locks the launchd-env contract: launchd
// jobs do NOT inherit the user's shell environment, so a BYO-creds setup
// (MORA_GOOGLE_CREDENTIALS in the profile) silently breaks every scheduled
// Google sync — the job hits the embedded DEV_PLACEHOLDER client and the vault
// goes stale while `mora sync` from a terminal still works. The fix: the plist
// builder snapshots MORA_GOOGLE_CREDENTIALS (a PATH, not a secret) into an
// EnvironmentVariables dict at install time, and omits the dict entirely when
// the var is unset (embedded real creds need no override).
func TestSchedulePlistCarriesGoogleCredsEnv(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	t.Setenv("MORA_GOOGLE_CREDENTIALS", "/Users/x/creds/google-client.json")
	plist := schedulePlist(t, cfg, "ingest-hourly")
	if !strings.Contains(plist, "<key>EnvironmentVariables</key>") {
		t.Fatalf("plist missing EnvironmentVariables dict when MORA_GOOGLE_CREDENTIALS is set:\n%s", plist)
	}
	if !strings.Contains(plist, "<key>MORA_GOOGLE_CREDENTIALS</key><string>/Users/x/creds/google-client.json</string>") {
		t.Fatalf("plist missing the snapshotted creds path:\n%s", plist)
	}

	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	plist = schedulePlist(t, cfg, "ingest-hourly")
	if strings.Contains(plist, "EnvironmentVariables") {
		t.Fatalf("plist must omit EnvironmentVariables when the var is unset:\n%s", plist)
	}
}
