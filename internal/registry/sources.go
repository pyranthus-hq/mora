package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func LoadSources(cfg config.Config) ([]memory.Source, error) {
	path := filepath.Join(cfg.ConfigDir, "sources.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var sources []memory.Source
	if err := json.Unmarshal(b, &sources); err != nil {
		return nil, err
	}
	// Grandfather migration (D-12): a missing `enabled` key means a pre-Enabled
	// binary wrote this source, i.e. the user had already explicitly added it —
	// treat absence as prior consent and normalize nil => true. An explicit
	// `false` is preserved as disabled (it is non-nil, so the loop skips it).
	for i := range sources {
		if sources[i].Enabled == nil {
			sources[i].Enabled = genericutil.Ptr(true)
		}
	}
	return sources, nil
}

func SaveSources(cfg config.Config, sources []memory.Source) error {
	b, err := json.MarshalIndent(sources, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(filepath.Join(cfg.ConfigDir, "sources.json"), append(b, '\n'), 0o600)
}

func LoadSourcesOrEmpty(cfg config.Config) []memory.Source {
	sources, err := LoadSources(cfg)
	if err != nil {
		return nil
	}
	return sources
}
