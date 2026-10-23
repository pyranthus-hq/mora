package loopbackhttp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

type httpConfig struct {
	Token        string `json:"token"`
	TokenVersion int    `json:"token_version"`
}

func LoadOrCreateToken(configDir string) (string, error) {
	token, _, err := LoadOrCreateTokenWithRotation(configDir)
	return token, err
}

// LoadOrCreateTokenWithRotation replaces tokens from builds that exposed the
// unauthenticated landing page. token_version 2 marks this one-time migration.
// The bool reports a replaced legacy token, not initial token creation.
func LoadOrCreateTokenWithRotation(configDir string) (string, bool, error) {
	path := filepath.Join(configDir, "http.json")
	var hc httpConfig
	fields := make(map[string]json.RawMessage)
	rotated := false
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &hc) == nil {
		if hc.Token != "" && hc.TokenVersion >= 2 {
			return hc.Token, false, nil
		}
		rotated = hc.Token != ""
		// Preserve other fields when upgrading a valid configuration.
		_ = json.Unmarshal(b, &fields)
	}
	if fields == nil {
		fields = make(map[string]json.RawMessage)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", false, err
	}
	token := hex.EncodeToString(raw)
	fields["token"], _ = json.Marshal(token)
	fields["token_version"] = json.RawMessage("2")
	b, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return "", false, err
	}
	if err := atomicio.Write(path, b, 0o600); err != nil {
		return "", false, err
	}
	return token, rotated, nil
}
