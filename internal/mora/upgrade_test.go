package mora

import "testing"

func TestIsHomebrewManaged(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"cask symlink target", "/opt/homebrew/Caskroom/mora/0.2.0/mora", true},
		{"intel cask", "/usr/local/Caskroom/mora/0.2.0/mora", true},
		{"formula cellar", "/opt/homebrew/Cellar/mora/0.2.0/bin/mora", true},
		{"intel cellar", "/usr/local/Cellar/mora/0.2.0/bin/mora", true},
		// install.sh drops a REAL file in these dirs — not Homebrew-managed.
		{"self-install opt-homebrew bin", "/opt/homebrew/bin/mora", false},
		{"self-install usr-local bin", "/usr/local/bin/mora", false},
		{"self-install local bin", "/Users/neil/.local/bin/mora", false},
		{"go build in repo", "/Users/dev/src/mora/mora", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHomebrewManaged(tc.path); got != tc.want {
				t.Fatalf("isHomebrewManaged(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "tok"); got != "tok" {
		t.Fatalf("got %q, want tok", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
