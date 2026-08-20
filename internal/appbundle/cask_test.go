package appbundle

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

const testManifest = `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  mora_1.2.3_darwin_amd64_app.zip
bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  mora_1.2.3_darwin_arm64_app.zip
`

func TestGenerateCaskGolden(t *testing.T) {
	got, err := GenerateCask("v1.2.3", strings.NewReader(testManifest), false)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/mora.golden.rb")
	if err != nil {
		t.Fatal(err)
	}
	// Git may materialize the text fixture with CRLF on Windows runners; the
	// generated Cask is intentionally byte-stable with LF line endings.
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Fatalf("generated Cask drift:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	again, err := GenerateCask("v1.2.3", strings.NewReader(testManifest), false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, again) {
		t.Fatal("identical inputs produced different bytes")
	}
}

func TestGenerateCaskRefusesAutoUpdatesUntilUpdaterLands(t *testing.T) {
	if CaskAutoUpdatesReady {
		t.Fatal("test must be revisited when #291 enables Cask auto-updates")
	}
	_, err := GenerateCask("v1.2.3", strings.NewReader(testManifest), true)
	if err == nil || !strings.Contains(err.Error(), "#291") {
		t.Fatalf("error = %v, want #291 refusal", err)
	}
}

func TestGenerateCaskFailsClosedOnManifestDrift(t *testing.T) {
	tests := []struct{ name, manifest, want string }{
		{"missing arch", strings.Split(testManifest, "\n")[0] + "\n", "missing"},
		{"duplicate", testManifest + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  mora_1.2.3_darwin_amd64_app.zip\n", "duplicate"},
		{"raw archive", strings.Replace(testManifest, "mora_1.2.3_darwin_arm64_app.zip", "mora_1.2.3_darwin_arm64.tar.gz", 1), "unexpected"},
		{"wrong version", strings.Replace(testManifest, "1.2.3_darwin_arm64", "1.2.4_darwin_arm64", 1), "unexpected"},
		{"uppercase hash", strings.Replace(testManifest, "aaaaaaaa", "AAAAAAAA", 1), "non-canonical"},
		{"malformed", "not a manifest line\n", "malformed"},
		{"oversized trailing data", testManifest + strings.Repeat("\n", int(maxCaskChecksumBytes)), "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GenerateCask("v1.2.3", strings.NewReader(tt.manifest), false)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestGenerateCaskRejectsUnsafeTags(t *testing.T) {
	for _, tag := range []string{"1.2.3", "v1.2", "v01.2.3", "v1.2.3-rc.1", "v1.2.3/../../tap"} {
		t.Run(tag, func(t *testing.T) {
			if _, err := GenerateCask(tag, strings.NewReader(testManifest), false); err == nil {
				t.Fatalf("tag %q accepted", tag)
			}
		})
	}
}

func TestGeneratedCaskUsesOnlySignedAppArtifact(t *testing.T) {
	body, err := GenerateCask("v1.2.3", strings.NewReader(testManifest), false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{`_app.zip`, `app "Mora.app"`, `Contents/MacOS/mora`} {
		if !strings.Contains(s, want) {
			t.Errorf("Cask missing %q", want)
		}
	}
	for _, forbidden := range []string{"xattr", "quarantine", "postflight", "zap ", ".tar.gz", "auto_updates true", `license "`} {
		if strings.Contains(s, forbidden) {
			t.Errorf("Cask contains forbidden %q", forbidden)
		}
	}
}
