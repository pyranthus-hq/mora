package appbundle

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

const statusManifest = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc  mora-status_1.0.0_arm64.zip\n"

func TestGenerateStatusCaskGolden(t *testing.T) {
	got, err := GenerateStatusCask("mora-status-v1.0.0", strings.NewReader(statusManifest))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/mora-status.golden.rb")
	if err != nil {
		t.Fatal(err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Fatalf("generated Cask drift:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestGenerateStatusCaskFailsClosed(t *testing.T) {
	tests := []struct{ name, tag, manifest, want string }{
		{"mora tag", "v1.0.0", statusManifest, "mora-status-v"},
		{"empty", "mora-status-v1.0.0", "", "missing"},
		{"duplicate", "mora-status-v1.0.0", statusManifest + statusManifest, "duplicate"},
		{"mora app asset", "mora-status-v1.0.0", strings.Replace(statusManifest, "mora-status_1.0.0_arm64.zip", "mora_1.0.0_darwin_arm64_app.zip", 1), "unexpected"},
		{"wrong version", "mora-status-v1.0.1", statusManifest, "unexpected"},
		{"uppercase hash", "mora-status-v1.0.0", strings.Replace(statusManifest, "cccc", "CCCC", 1), "non-canonical"},
		{"malformed", "mora-status-v1.0.0", "nope\n", "malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GenerateStatusCask(tt.tag, strings.NewReader(tt.manifest))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
