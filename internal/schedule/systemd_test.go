package schedule

import (
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
		"StandardOutput=append:/home/x/state%%d/share-sync.out.log\n",
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
