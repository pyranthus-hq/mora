package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWritesCaskAtomically(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "checksums-app.txt")
	body := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  mora_1.2.3_darwin_amd64_app.zip\n" +
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  mora_1.2.3_darwin_arm64_app.zip\n"
	if err := os.WriteFile(manifest, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "mora.rb")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--tag", "v1.2.3", "--checksums", manifest, "--out", outPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `app "Mora.app"`) {
		t.Fatalf("wrong output: %s", got)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".mora-cask-*.tmp")); len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
}

func TestRunRefusesAutoUpdates(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "checksums-app.txt")
	body := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  mora_1.2.3_darwin_amd64_app.zip\n" +
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  mora_1.2.3_darwin_arm64_app.zip\n"
	if err := os.WriteFile(manifest, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--tag", "v1.2.3", "--checksums", manifest, "--out", "-", "--auto-updates"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "#291") || stdout.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(stderr.String(), "usage: gencask") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}
