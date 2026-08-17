package sharing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// GenSeqWidth zero-pads commit names so lexical and numeric order agree.
const GenSeqWidth = 10

// GenRetain is the number of superseded generations retained for readers.
const GenRetain = 3

// Commit binds one published generation to both immutable served artifacts.
type Commit struct {
	Seq          int    `json:"seq"`
	Gen          string `json:"gen"`
	RunID        string `json:"run_id"`
	SourceRev    string `json:"source_rev"`
	BucketFloor  int    `json:"bucket_floor,omitempty"`
	BuiltAt      string `json:"built_at"`
	CorpusDigest string `json:"corpus_digest"`
	IndexDigest  string `json:"index_digest"`
	Count        int    `json:"count"`
}

// GenerationStore owns the immutable subscription-generation layout and read-only commit resolver.
type GenerationStore struct {
	DataDir  string
	ReadDir  func(string) ([]os.DirEntry, error)
	ReadFile func(string) ([]byte, error)
}

func (s GenerationStore) readDir(path string) ([]os.DirEntry, error) {
	if s.ReadDir != nil {
		return s.ReadDir(path)
	}
	return os.ReadDir(path)
}
func (s GenerationStore) readFile(path string) ([]byte, error) {
	if s.ReadFile != nil {
		return s.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (s GenerationStore) Root(name string) string        { return SubscriptionRoot(s.DataDir, name) }
func (s GenerationStore) GensDir(name string) string     { return filepath.Join(s.Root(name), "gens") }
func (s GenerationStore) GenDir(name, gen string) string { return filepath.Join(s.GensDir(name), gen) }
func (s GenerationStore) CorpusDir(name, gen string) string {
	return filepath.Join(s.GenDir(name, gen), "corpus")
}
func (s GenerationStore) IndexPath(name, gen string) string {
	return filepath.Join(s.GenDir(name, gen), "index.db")
}
func (s GenerationStore) CommitsDir(name string) string {
	return filepath.Join(s.Root(name), "commits")
}
func (s GenerationStore) CommitPath(name string, seq int) string {
	return filepath.Join(s.CommitsDir(name), fmt.Sprintf("%0*d", GenSeqWidth, seq))
}
func (s GenerationStore) AttemptPath(name string) string {
	return filepath.Join(s.Root(name), "attempt.json")
}
func (s GenerationStore) ImportLockPath(name string) string {
	return filepath.Join(s.Root(name), "import.lock")
}
func (s GenerationStore) MigratedLatchPath(name string) string {
	return filepath.Join(s.Root(name), "migrated")
}
func (s GenerationStore) FetchDir(name, runID string) string {
	return filepath.Join(s.Root(name), "fetch-"+runID)
}

// RunID returns the import run id encoded by a generation name.
func RunID(gen string) string { return strings.TrimPrefix(gen, "gen-") }

// Resolve picks the highest numeric commit record and fails closed on its read or decode error.
func (s GenerationStore) Resolve(name string) (Commit, bool, error) {
	dir := s.CommitsDir(name)
	entries, err := s.readDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return Commit{}, false, nil
	}
	if err != nil {
		return Commit{}, false, err
	}
	maxSeq, maxName := -1, ""
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		seq, parseErr := strconv.Atoi(strings.TrimSpace(entry.Name()))
		if parseErr == nil && seq > maxSeq {
			maxSeq, maxName = seq, entry.Name()
		}
	}
	if maxSeq < 0 {
		return Commit{}, false, nil
	}
	body, err := s.readFile(filepath.Join(dir, maxName))
	if err != nil {
		return Commit{}, false, err
	}
	var commit Commit
	if err := json.Unmarshal(body, &commit); err != nil {
		return Commit{}, false, fmt.Errorf("share %q: commit record %s is corrupt: %w", name, maxName, err)
	}
	return commit, true, nil
}

// ReadAll reads every parseable numeric commit for claim and garbage-collection policy.
func (s GenerationStore) ReadAll(name string) ([]Commit, error) {
	dir := s.CommitsDir(name)
	entries, err := s.readDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Commit
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimSpace(entry.Name())); err != nil {
			continue
		}
		body, err := s.readFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var commit Commit
		if json.Unmarshal(body, &commit) != nil {
			continue
		}
		out = append(out, commit)
	}
	return out, nil
}

// CorpusDigest hashes sorted "<sha256>  <filename>" rows for regular Markdown entries.
func CorpusDigest(corpusDir string) (string, error) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		return "", err
	}
	lines := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(corpusDir, entry.Name()))
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(body)
		lines = append(lines, hex.EncodeToString(sum[:])+"  "+entry.Name())
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

// FileDigest returns a whole-file SHA-256 digest.
func FileDigest(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
