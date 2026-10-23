package loopbackhttp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

func LoadOrCreateToken(configDir string) (string, error) {
	token, _, err := LoadOrCreateTokenWithRotation(configDir)
	return token, err
}

// LoadOrCreateTokenWithRotation replaces tokens from builds that exposed the
// unauthenticated landing page. token_version 2 marks this one-time migration.
// The bool reports a replaced legacy token, not initial token creation.
func LoadOrCreateTokenWithRotation(configDir string) (string, bool, error) {
	path := filepath.Join(configDir, "http.json")
	fields := make(map[string]json.RawMessage)
	rotated := false
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &fields) == nil {
		var token string
		var version float64
		for key, value := range fields {
			switch {
			case strings.EqualFold(key, "token"):
				_ = json.Unmarshal(value, &token)
			case strings.EqualFold(key, "token_version"):
				_ = json.Unmarshal(value, &version)
			}
		}
		if token != "" && version >= 2 {
			return token, false, nil
		}
		rotated = token != ""
	} else {
		fields = nil
	}
	if fields == nil {
		fields = make(map[string]json.RawMessage)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", false, err
	}
	token := hex.EncodeToString(raw)
	for key := range fields {
		if strings.EqualFold(key, "token") || strings.EqualFold(key, "token_version") {
			delete(fields, key)
		}
	}
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
