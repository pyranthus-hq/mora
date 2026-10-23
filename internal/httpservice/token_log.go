package httpservice

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

// ScrubLegacyTokenLog removes only the old stdout banner's "  token: <hex>"
// lines. Both launchd and systemd used this file; Windows tasks did not redirect
// output. Call only after rotating a legacy token, before serving or printing it.
func ScrubLegacyTokenLog(stateDir string) error {
	path := filepath.Join(stateDir, "serve-http.out.log")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	var kept bytes.Buffer
	for _, line := range bytes.SplitAfter(b, []byte("\n")) {
		text := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if suffix, ok := bytes.CutPrefix(text, []byte("  token: ")); ok && len(suffix) == 64 {
			if _, err := hex.DecodeString(string(suffix)); err == nil {
				continue
			}
		}
		kept.Write(line)
	}
	if bytes.Equal(b, kept.Bytes()) {
		return nil
	}
	return atomicio.Write(path, kept.Bytes(), info.Mode().Perm())
}
