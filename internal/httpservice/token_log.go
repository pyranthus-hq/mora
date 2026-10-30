package httpservice

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// ScrubLegacyTokenLog removes only the old stdout banner's "  token: <hex>"
// lines. Both launchd and systemd used this file; Windows tasks did not redirect
// output. Call only after rotating a legacy token, before serving or printing it.
func ScrubLegacyTokenLog(stateDir string) error {
	path := filepath.Join(stateDir, "serve-http.out.log")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
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
	// Service managers open stdout before startup. Preserve that inode (and
	// any symlink) so their append descriptors continue writing to this log.
	if _, err := f.WriteAt(kept.Bytes(), 0); err != nil {
		return err
	}
	if err := f.Truncate(int64(kept.Len())); err != nil {
		return err
	}
	return f.Sync()
}
