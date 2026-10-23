package conversation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

type Source struct {
	Provider string   `json:"provider"`
	Root     string   `json:"root,omitempty"`
	File     string   `json:"file,omitempty"`
	CWDs     []string `json:"cwd_roots,omitempty"`
	Scope    string   `json:"scope"`
}
type Settings struct {
	Sources []Source `json:"sources"`
}

func LoadSettings(path string) (Settings, error) {
	var c Settings
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("invalid conversation capture settings: %w", err)
	}
	return c, nil
}
func NormalizeSource(src Source) (Source, error) {
	if src.Provider != "codex" && src.Provider != "claude" {
		return src, errors.New("provider must be codex or claude")
	}
	if strings.TrimSpace(src.Scope) == "" {
		return src, errors.New("explicit conversation scope required")
	}
	if (src.Root == "") == (src.File == "") {
		return src, errors.New("choose exactly one transcript file or source root")
	}
	if src.File != "" {
		p, err := filepath.Abs(src.File)
		if err != nil {
			return src, err
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return src, err
		}
		link, err := os.Lstat(p)
		if err != nil {
			return src, err
		}
		if link.Mode()&os.ModeSymlink != 0 {
			return src, errors.New("symlink transcript paths are not allowed")
		}
		info, err := os.Stat(p)
		if err != nil {
			return src, err
		}
		if !info.Mode().IsRegular() {
			return src, errors.New("transcript must be a regular file")
		}
		src.File = real
	} else {
		p, err := filepath.Abs(src.Root)
		if err != nil {
			return src, err
		}
		p, err = filepath.EvalSymlinks(p)
		if err != nil {
			return src, err
		}
		info, err := os.Stat(p)
		if err != nil {
			return src, err
		}
		if !info.IsDir() {
			return src, errors.New("source root must be a directory")
		}
		src.Root = p
		if len(src.CWDs) == 0 {
			return src, errors.New("source-root capture requires at least one cwd-root allowlist")
		}
	}
	for i, cwd := range src.CWDs {
		p, err := filepath.Abs(cwd)
		if err != nil {
			return src, err
		}
		p, err = filepath.EvalSymlinks(p)
		if err != nil {
			return src, err
		}
		info, err := os.Stat(p)
		if err != nil || !info.IsDir() {
			return src, errors.New("cwd-root must be an existing directory")
		}
		src.CWDs[i] = p
	}
	return src, nil
}
func Enable(path string, src Source) error {
	src, err := NormalizeSource(src)
	if err != nil {
		return err
	}
	return leasefile.WithGuard(path+".lock", func() error {
		cfg, err := LoadSettings(path)
		if err != nil {
			return err
		}
		for _, old := range cfg.Sources {
			if old.Provider == src.Provider && old.Root == src.Root && old.File == src.File {
				a, _ := json.Marshal(old)
				b, _ := json.Marshal(src)
				if string(a) == string(b) {
					return nil
				}
				if src.File != "" {
					return errors.New("selected transcript already configured with a different scope")
				}
				for _, a := range old.CWDs {
					for _, b := range src.CWDs {
						if within(a, b) || within(b, a) {
							return errors.New("overlapping cwd allowlists; consolidate or remove the old source before changing scope")
						}
					}
				}
			}
		}
		cfg.Sources = append(cfg.Sources, src)
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		return atomicio.WriteDurable(path, append(b, '\n'), 0600)
	})
}
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func allowed(src Source, cwd string) bool {
	if src.File != "" && len(src.CWDs) == 0 {
		return true
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return false
	}
	for _, root := range src.CWDs {
		if within(root, real) {
			return true
		}
	}
	return false
}
