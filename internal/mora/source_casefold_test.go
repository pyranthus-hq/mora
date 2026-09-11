package mora

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/genericutil"
)

func TestSourceCaseFold(t *testing.T) {
	for _, command := range []string{"connect", "add"} {
		for _, same := range []bool{false, true} {
			t.Run(command+map[bool]string{false: "/collision", true: "/refresh"}[same], func(t *testing.T) {
				withTempHome(t)
				run(t, "init")
				cfg, err := loadConfigFor(testCtx(t))
				if err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(t.TempDir(), "Pyranthus")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				dir, err = filepath.EvalSymlinks(dir)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("case collision regression"), 0600); err != nil {
					t.Fatal(err)
				}
				oldPath := dir
				if !same {
					oldPath = t.TempDir()
				}
				created := "2020-01-02T03:04:05Z"
				if err := saveSources(cfg, []Source{{Name: "pyranthus", Type: "filesystem", Path: oldPath, Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: created}}); err != nil {
					t.Fatal(err)
				}
				registryPath := filepath.Join(cfg.ConfigDir, "sources.json")
				before, err := os.ReadFile(registryPath)
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"connect", "filesystem", dir, "--json"}
				if command == "add" {
					args = []string{"sources", "add", "filesystem", "--name", "Pyranthus", "--path", dir, "--json"}
				}
				out, runErr := runErr(t, args...)
				if !same {
					if runErr == nil {
						t.Error("case-colliding folder accepted")
					}
					var doc struct {
						Schema  string `json:"schema"`
						Message string `json:"message"`
					}
					if err := json.Unmarshal([]byte(out), &doc); err != nil {
						t.Errorf("error document: %v: %s", err, out)
					}
					if doc.Schema != "mora.error" || !strings.Contains(doc.Message, "pyranthus") || !strings.Contains(doc.Message, "Pyranthus") {
						t.Errorf("missing collision error document: %s", out)
					}
					after, err := os.ReadFile(registryPath)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(before, after) {
						t.Error("sources.json changed on rejection")
					}
					for _, root := range []string{filepath.Join(cfg.VaultDir, "sources", "filesystem"), filepath.Join(cfg.StateDir, "sync")} {
						_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
							if err == nil && !d.IsDir() {
								t.Errorf("unexpected vault/status write: %s", p)
							}
							return nil
						})
					}
					return
				}
				if runErr != nil {
					t.Fatalf("refresh: %v: %s", runErr, out)
				}
				sources, err := loadSources(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if len(sources) != 1 {
					t.Fatalf("refresh created %d entries", len(sources))
				}
				if sources[0].Name != "pyranthus" || sources[0].CreatedAt != created {
					t.Errorf("refresh lost identity: %+v", sources[0])
				}
			})
		}
	}
}

func TestEnsureGoogleSourcesCaseFold(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := saveSources(cfg, []Source{{Name: "Gmail-work", Type: "filesystem", Path: t.TempDir(), Enabled: genericutil.Ptr(true)}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.ConfigDir, "sources.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureGoogleSources(cfg, "work"); err == nil {
		t.Fatal("case-colliding Google source accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("collision changed registry")
	}
}
