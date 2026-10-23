package loopbackhttp

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTokenUpgradeOnce(t *testing.T) {
	for _, tc := range []struct {
		name, seed string
		rotate     bool
	}{
		{"legacy", `{"token":"` + strings.Repeat("a", 64) + `","other":{"keep":true}}`, true},
		{"fresh", "", false},
		{"empty-file", "", false},
		{"empty-token", `{"token":""}`, false},
		{"corrupt", `{bad`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			p := filepath.Join(d, "http.json")
			if tc.name != "fresh" {
				if err := os.WriteFile(p, []byte(tc.seed), 0644); err != nil {
					t.Fatal(err)
				}
			}
			token, rotated, err := LoadOrCreateTokenWithRotation(d)
			if err != nil {
				t.Fatal(err)
			}
			if rotated != tc.rotate {
				t.Fatalf("rotated=%v, want %v", rotated, tc.rotate)
			}
			if raw, err := hex.DecodeString(token); err != nil || len(raw) != 32 {
				t.Fatal("expected a 32-byte hex token")
			}
			if token == strings.Repeat("a", 64) {
				t.Fatal("legacy token reused")
			}
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(b, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["token_version"]) != "2" {
				t.Fatal("missing version 2 marker")
			}
			if tc.rotate {
				var other map[string]bool
				if err := json.Unmarshal(fields["other"], &other); err != nil || !other["keep"] {
					t.Fatal("lost existing fields")
				}
			}
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatalf("mode=%v", info.Mode().Perm())
			}
			again, rotated, err := LoadOrCreateTokenWithRotation(d)
			if err != nil || rotated || again != token {
				t.Fatal("second load changed the token or rotated again", err)
			}
			wrapped, err := LoadOrCreateToken(d)
			if err != nil || wrapped != token {
				t.Fatal("wrapper changed token", err)
			}
			if tc.rotate {
				s := New(Options{Token: token, Port: 7777})
				for _, c := range []struct {
					token  string
					status int
				}{{strings.Repeat("a", 64), 401}, {token, 200}} {
					w := request(t, s, "GET", "/", "", "Bearer "+c.token, "127.0.0.1:7777")
					if w.Code != c.status {
						t.Fatalf("status=%d want %d", w.Code, c.status)
					}
				}
			}
		})
	}
}
