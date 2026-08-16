package sharing

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const LedgerSchema = 1

type Ledger struct {
	Schema        int            `json:"schema"`
	Publishes     []Publish      `json:"publishes,omitempty"`
	Subscriptions []Subscription `json:"subscriptions,omitempty"`
}
type Publish struct {
	Name       string        `json:"name"`
	Scope      string        `json:"scope"`
	Recipients []string      `json:"recipients"`
	Remote     string        `json:"remote,omitempty"`
	Transport  *TransportRef `json:"transport,omitempty"`
	Owner      string        `json:"owner,omitempty"`
	CreatedAt  string        `json:"created_at"`
}
type Subscription struct {
	Name         string        `json:"name"`
	Remote       string        `json:"remote"`
	Transport    *TransportRef `json:"transport,omitempty"`
	PinnedPubkey []byte        `json:"pinned_pubkey,omitempty"`
	LastVersion  int           `json:"last_version,omitempty"`
	CreatedAt    string        `json:"created_at"`
}
type TransportRef struct {
	Kind   string        `json:"kind,omitempty"`
	Bucket *BucketConfig `json:"bucket,omitempty"`
}
type BucketConfig struct {
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"`
	SecretRef string `json:"secret_ref,omitempty"`
}

func (c BucketConfig) ObjectPrefix() string {
	p := strings.Trim(c.Prefix, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}
func (c BucketConfig) Locator() string {
	return "bucket\x00" + strings.TrimRight(c.Endpoint, "/") + "\x00" + c.Bucket + "\x00" + strings.Trim(c.Prefix, "/")
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var scopeRE = regexp.MustCompile(`^(personal|global|project:[A-Za-z0-9][A-Za-z0-9._-]*)$`)

func ValidName(s string) bool                { return nameRE.MatchString(s) }
func ValidScope(s string) bool               { return scopeRE.MatchString(s) }
func LedgerPath(configDir string) string     { return filepath.Join(configDir, "shares.json") }
func StagingDir(dataDir, name string) string { return filepath.Join(dataDir, "share", "publish", name) }
func SubscriptionRoot(dataDir, name string) string {
	return filepath.Join(dataDir, "share", "subs", name)
}
func RepoDir(dataDir, name string) string {
	return filepath.Join(SubscriptionRoot(dataDir, name), "repo")
}
func CorpusDir(dataDir, name string) string {
	return filepath.Join(SubscriptionRoot(dataDir, name), "corpus")
}
func IndexPath(dataDir, name string) string {
	return filepath.Join(SubscriptionRoot(dataDir, name), "index.db")
}
func Load(configDir string) (Ledger, error) {
	var ledger Ledger
	path := LedgerPath(configDir)
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Ledger{Schema: LedgerSchema}, nil
	}
	if err != nil {
		return ledger, err
	}
	if err := json.Unmarshal(body, &ledger); err != nil {
		return ledger, fmt.Errorf("%s is corrupt (%v) — fix or delete the file; it holds share/subscription registrations", path, err)
	}
	return ledger, nil
}
func Save(configDir string, ledger Ledger) error {
	ledger.Schema = LedgerSchema
	body, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(LedgerPath(configDir), append(body, '\n'), 0o600)
}
func ValidateSubscriptionNameAvailable(ledger Ledger, name string) error {
	for _, existing := range ledger.Subscriptions {
		if existing.Name == name {
			return fmt.Errorf("subscription %q already exists — `mora share pull %s` updates it", name, name)
		}
	}
	for _, existing := range ledger.Publishes {
		if existing.Name == name {
			return fmt.Errorf("%q already names a share you publish — share and subscription names share one namespace", name)
		}
	}
	return nil
}

func (c BucketConfig) Display() string {
	loc := c.Bucket
	if p := strings.Trim(c.Prefix, "/"); p != "" {
		loc += "/" + p
	}
	if c.Endpoint != "" {
		loc = strings.TrimRight(c.Endpoint, "/") + "/" + loc
	}
	return loc
}

const ExportManifestSchema = 1
const MaxMemoryBytes = 4 << 20

var ExportIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type ExportManifest struct {
	Schema    int    `json:"schema"`
	Name      string `json:"name"`
	Scope     string `json:"scope"`
	Owner     string `json:"owner,omitempty"`
	CreatedAt string `json:"created_at"`
	Client    string `json:"client"`
}
