package mora

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/pyranthus-hq/mora/internal/resource"
)

const maxResourcePreviewFileBytes = 20 << 10

// readResourcePreviewFile is intentionally manifest-addressed: callers cannot
// use an arbitrary path to inspect the host, and oversized files are refused
// instead of silently clipped before a native decision.
func readResourcePreviewFile(cfg Config, projectDir, client, id, revision, path string) (string, error) {
	_, _, _, _, _, err := resourcePreview(cfg, projectDir, client, id, revision)
	if err != nil {
		return "", err
	}
	_, rev, err := resource.ReadRevision(cfg.VaultDir, id, revision)
	if err != nil {
		return "", err
	}
	found := false
	for _, file := range rev.Files {
		if file.Path == path {
			found = true
			break
		}
	}
	if !found || path == "" || filepath.IsAbs(path) {
		return "", errors.New("resource file not in manifest")
	}
	name := filepath.Join(cfg.VaultDir, "resources", id, "revisions", revision, "tree", filepath.FromSlash(path))
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxResourcePreviewFileBytes {
		return "", errors.New("resource file unavailable")
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
