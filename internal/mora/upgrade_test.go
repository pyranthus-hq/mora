package mora

import (
	"bytes"
	"strings"
	"testing"
)

func TestClassifyUpgradeInstall(t *testing.T) {
	oldVersion := BuildVersion
	t.Cleanup(func() { BuildVersion = oldVersion })
	cases := []struct {
		name, version, exe string
		want               upgradeInstallRoute
	}{
		{"signed app", "1.2.3", "/Applications/Mora.app/Contents/MacOS/mora", upgradeRouteApp},
		{"Caskroom app", "1.2.3", "/opt/homebrew/Caskroom/mora/1.2.3/Mora.app/Contents/MacOS/mora", upgradeRouteApp},
		{"raw formula", "1.2.3", "/opt/homebrew/Cellar/mora/1.2.3/bin/mora", upgradeRouteHomebrew},
		{"source", "dev", "/Users/dev/src/mora/mora", upgradeRouteSource},
		{"release archive", "1.2.3", "/Users/me/.local/bin/mora", upgradeRouteDirect},
	}
	for _, tc := range cases {
		subRun(t, tc.name, func(t *testing.T) {
			BuildVersion = tc.version
			if got := classifyUpgradeInstall(tc.exe); got != tc.want {
				t.Fatalf("classifyUpgradeInstall(%q) = %q, want %q", tc.exe, got, tc.want)
			}
		})
	}
}

func TestCmdUpgradeRoutesHomebrewWithoutNetwork(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	oldExe, oldVersion := upgradeExecutable, BuildVersion
	t.Cleanup(func() { upgradeExecutable, BuildVersion = oldExe, oldVersion })
	BuildVersion = "1.2.3"
	upgradeExecutable = func() (string, error) { return "/opt/homebrew/Cellar/mora/1.2.3/bin/mora", nil }
	var out bytes.Buffer
	if err := cmdUpgrade(testCtx(t), []string{"--check"}, &out, testStderr); err != nil {
		t.Fatalf("cmdUpgrade: %v", err)
	}
	if !strings.Contains(out.String(), "brew upgrade pyranthus-hq/tap/mora") {
		t.Fatalf("Homebrew route output = %q", out.String())
	}
}

func TestDecideUpgrade(t *testing.T) {
	cases := []struct {
		name        string
		current     string
		latest      string
		wantVerdict upgradeVerdict
		wantLocal   bool
		wantErr     bool
	}{
		// The live bug: a local build 60 commits past the v0.10.0 tag must
		// NOT be offered the older v0.10.0 release as an "update".
		{"local build ahead of equal base tag", "v0.10.0-60-g2d08334", "0.10.0", verdictLocalAhead, true, false},
		{"local build ahead, dirty", "v0.10.0-60-g2d08334-dirty", "0.10.0", verdictLocalAhead, true, false},
		{"dirty exact tag", "v0.10.0-dirty", "0.10.0", verdictLocalAhead, true, false},
		{"local build with base past latest", "v0.11.0-2-gabc1234", "0.10.0", verdictLocalAhead, true, false},
		{"local build behind latest", "v0.9.1-5-gabc1234", "0.10.0", verdictUpgrade, true, false},
		{"clean release behind latest", "0.9.1", "0.10.0", verdictUpgrade, false, false},
		{"clean release current", "0.10.0", "0.10.0", verdictUpToDate, false, false},
		{"clean release ahead of latest", "0.11.0", "0.10.0", verdictUpToDate, false, false},
		// "dev" never reaches decideUpgrade (cmdUpgrade refuses it first) —
		// if it somehow did, fail loudly instead of comparing garbage.
		{"literal dev fails parse", "dev", "0.10.0", 0, false, true},
	}
	for _, tc := range cases {
		subRun(t, tc.name, func(t *testing.T) {
			verdict, isLocal, err := decideUpgrade(tc.current, tc.latest)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("decideUpgrade(%q, %q) expected an error, got (%v, %v)", tc.current, tc.latest, verdict, isLocal)
				}
				return
			}
			if err != nil {
				t.Fatalf("decideUpgrade(%q, %q): %v", tc.current, tc.latest, err)
			}
			if verdict != tc.wantVerdict || isLocal != tc.wantLocal {
				t.Fatalf("decideUpgrade(%q, %q) = (%v, %v), want (%v, %v)", tc.current, tc.latest, verdict, isLocal, tc.wantVerdict, tc.wantLocal)
			}
		})
	}
}
