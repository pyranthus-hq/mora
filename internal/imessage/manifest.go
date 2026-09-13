package imessage

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

type ChatMark struct {
	MaxDate int64  `json:"max_date"`
	MinDate int64  `json:"min_date"`
	Hash    string `json:"hash"`
}

type Manifest struct {
	Version     int                 `json:"version"`
	WindowStart int64               `json:"window_start"`
	Chats       map[string]ChatMark `json:"chats"`
}

func LoadManifest(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, nil
	}
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, err
	}
	if m.Version != 1 {
		return Manifest{}, nil
	}
	return m, nil
}

func SaveManifest(path string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(path, b, 0600)
}

// A chat is skipped only when all three hold: the manifest has an entry for its GUID, the chat's current `MAX(message.date)` is not greater than `MaxDate`, and `MinDate >= windowStart` (no message rendered last time has aged out of the window). A manifest whose `Version` is not 1 is discarded. Skipped chats produce no `Item`; the vault file stays as it is. Rendered chats update their `ChatMark` with the new max, min, and hash. The aggregate is one indexed query per chat:
func (m Manifest) NeedsRender(guid string, maxDate int64, windowStart int64) bool {
	mark, ok := m.Chats[guid]
	return m.Version != 1 || !ok || maxDate > mark.MaxDate || mark.MinDate < windowStart
}

type FetchOptions struct {
	Manifest *Manifest
	Full     bool
}

func (f *LiveFetcher) SetFetchOptions(o FetchOptions) {
	if o.Manifest != nil {
		if o.Manifest.Version != 1 {
			*o.Manifest = Manifest{Version: 1}
		}
		if o.Manifest.Chats == nil {
			o.Manifest.Chats = map[string]ChatMark{}
		}
	}
	f.opts = o
}
