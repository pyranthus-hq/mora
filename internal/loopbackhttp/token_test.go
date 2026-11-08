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

func TestTokenVersionMigration(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		rotate       bool
	}{
		{"version-one", `"token_version":1,"token":`, true},
		{"version-string", `"token_version":"2","token":`, true},
		{"version-float", `"token_version":2.0,"token":`, false},
		{"version-missing", `"token":`, true},
		{"capitalized-token", `"Token":`, true},
		{"version-bool", `"token_version":true,"token":`, true},
		{"version-object", `"token_version":{},"token":`, true},
		{"capitalized-fields", `"TOKEN_VERSION":1,"Token":`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			path := filepath.Join(d, "http.json")
			old := strings.Repeat("a", 64)
			seed := `{` + tc.fields + `"` + old + `","other":{"keep":true}}`
			if err := os.WriteFile(path, []byte(seed), 0600); err != nil {
				t.Fatal(err)
			}
			token, rotated, err := LoadOrCreateTokenWithRotation(d)
			if err != nil {
				t.Fatal(err)
			}
			if rotated != tc.rotate || (token != old) != tc.rotate {
				t.Fatalf("rotated=%v, token changed=%v, want %v", rotated, token != old, tc.rotate)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(b, &fields); err != nil {
				t.Fatal(err)
			}
			var other map[string]bool
			if err := json.Unmarshal(fields["other"], &other); err != nil || !other["keep"] {
				t.Fatal("lost unknown field", err)
			}
			if tc.rotate {
				if len(fields) != 3 || string(fields["token_version"]) != "2" || string(fields["token"]) != `"`+token+`"` {
					t.Fatalf("migration did not canonicalize fields: %s", b)
				}
			} else if string(b) != seed {
				t.Fatal("versioned config was rewritten")
			}
			again, rotated, err := LoadOrCreateTokenWithRotation(d)
			if err != nil || rotated || again != token {
				t.Fatal("second load changed token or rotated again", err)
			}
		})
	}
}
