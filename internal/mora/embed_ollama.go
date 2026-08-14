package mora

import (
	embedpkg "github.com/pyranthus-hq/mora/internal/embed"
)

var errEmbedderUnavailable = embedpkg.ErrUnavailable

func chooseEmbedderFor(cfg Config) (Embedder, error) { return embedpkg.ChooseFor(cfg) }
func chooseEmbedder() (Embedder, error)              { return embedpkg.Choose() }
