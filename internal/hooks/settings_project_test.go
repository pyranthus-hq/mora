package hooks

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallProjectRecallRefusesDuplicateManagedHooks(t *testing.T) {
	for _, event := range []string{"UserPromptSubmit", "Stop"} {
		t.Run(event, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, ".claude", "settings.local.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"theme":"dark","hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"mora hook recall #mora-managed:recall"},{"type":"command","command":"unrelated"}]}],"` + event + `": [{"hooks":[{"type":"command","command":"mora hook recall #mora-managed:recall"}]}]}}`)
			if event == "UserPromptSubmit" {
				body = []byte(`{"theme":"dark","hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"mora hook recall #mora-managed:recall"},{"type":"command","command":"unrelated"},{"type":"command","command":"mora hook recall #mora-managed:recall"}]}]}}`)
			}
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := InstallProjectRecall(path, "/bin/mora", "project:demo", root, 0); err == nil {
				t.Fatal("ambiguous managed hooks were accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("refused install changed settings: %s, %v", got, err)
			}
		})
	}
}

func TestInstallProjectRecallRefusesNullSettings(t *testing.T) {
	for _, body := range []string{"null", `{"hooks":null}`} {
		t.Run(body, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, ".claude", "settings.local.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := InstallProjectRecall(path, "/bin/mora", "project:demo", root, 0); err == nil {
				t.Fatal("null settings accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != body {
				t.Fatalf("null settings changed: %s, %v", got, err)
			}
		})
	}
}
