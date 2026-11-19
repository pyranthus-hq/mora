package schedule

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
)

// TestSystemdUnitsEscapePathsAndSpecifiers: a path with spaces, quotes or `%`
// must reach systemd literally. Unescaped, `%h` would expand to the home dir and
// a space would split ExecStart into two words.
func TestSystemdUnitsEscapePathsAndSpecifiers(t *testing.T) {
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	t.Setenv("MORA_VAULT", "")
	t.Setenv("MORA_CONFIG_DIR", `/home/x/100%h "cfg"`)
	old := executable
	executable = func() (string, error) { return "/opt/my apps/mora", nil }
	t.Cleanup(func() { executable = old })

	service, timer, ok := SystemdUnitsFor(config.Config{StateDir: "/home/x/state%d"}, "share-sync")
	if !ok {
		t.Fatal("share-sync must render systemd units")
	}
	for _, want := range []string{
		`ExecStart="/opt/my apps/mora" share sync` + "\n",
		`Environment="MORA_CONFIG_DIR=/home/x/100%%h \"cfg\""` + "\n",
		"StandardOutput=append:" + strings.ReplaceAll(filepath.Join("/home/x/state%d", "share-sync.out.log"), "%", "%%") + "\n",
	} {
		if !strings.Contains(service, want) {
			t.Fatalf("service missing %q:\n%s", want, service)
		}
	}
	if !strings.Contains(timer, "Unit=mora-share-sync.service\n") {
		t.Fatalf("timer:\n%s", timer)
	}
	if _, _, ok := SystemdUnitsFor(config.Config{}, "ingest-hourly"); ok {
		t.Fatal("only share-sync installs a systemd timer")
	}
}

func TestSystemdQuoteEscapesControlCharacters(t *testing.T) {
	got := systemdQuote("a\nb\rc\td")
	if got != `"a\nb\rc\td"` {
		t.Fatalf("quote = %q", got)
	}
}

func TestWindowsShareSyncCarriesVaultAndNoninteractiveGit(t *testing.T) {
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	t.Setenv("MORA_CONFIG_DIR", `C:\scratch\config`)
	t.Setenv("MORA_VAULT", `C:\scratch\vault`)
	got := WindowsTaskCommand(`C:\mora.exe`, "share sync")
	for _, want := range []string{`set "MORA_CONFIG_DIR=C:\scratch\config"`, `set "MORA_VAULT=C:\scratch\vault"`, `set "GIT_TERMINAL_PROMPT=0"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(WindowsTaskCommand(`C:\mora.exe`, "index rebuild"), "GIT_TERMINAL_PROMPT") {
		t.Fatal("git env leaked to other job")
	}
}

func TestSystemdUnitsRejectLogPathDirectiveInjection(t *testing.T) {
	for _, path := range []string{"/tmp/state\nExecStart=/bin/false", "/tmp/state\rBad=value", "/tmp/state\x00bad"} {
		if _, _, ok := SystemdUnitsFor(config.Config{StateDir: path}, "share-sync"); ok {
			t.Fatalf("accepted unsafe log path %q", path)
		}
	}
}
