package mora

import (
	"crypto/sha256"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// doctor_index.go — the doctor-side helpers for the Gate 2 index arm (B1a / B2).

// enabledSourceCount counts sources whose IsEnabled() is true. loadSources returns
// disabled rows too, so this is the count that actually gates the alarm.
func enabledSourceCount(sources []Source) int {
	n := 0
	for _, s := range sources {
		if s.IsEnabled() {
			n++
		}
	}
	return n
}

// vaultHasConnectorMemories reports whether any connector corpus exists under
// sources/. A freshly-seeded vault has none (so a zero-enabled-sources doctor is
// legitimately green); a vault that DID ingest and then had every source disabled
// has a corpus with no alarm behind it (fail closed).
func vaultHasConnectorMemories(cfg Config) bool {
	found := false
	_ = filepath.WalkDir(sourcesRoot(cfg), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() && filepath.Ext(p) == ".md" {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// disabledCorpusTypes returns each connector type that has a corpus under
// sources/<type>/ but no ENABLED instance — the realistic "disabled ONE connector
// over an existing corpus and silently lost its alarm" case (▸R).
func disabledCorpusTypes(cfg Config, sources []Source) []string {
	enabled := map[string]bool{}
	for _, s := range sources {
		if s.IsEnabled() {
			enabled[s.Type] = true
		}
	}
	entries, err := os.ReadDir(sourcesRoot(cfg))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || enabled[e.Name()] {
			continue
		}
		if dirHasMemoryFile(filepath.Join(sourcesRoot(cfg), e.Name())) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func dirHasMemoryFile(root string) bool {
	found := false
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ".md" {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// indexMatchesVault recomputes the content manifest (B1a) and compares it to the
// committed one. It runs ONLY on the `mora doctor` path (never --pulse, never the
// MCP hot path), where a full vault walk is already paid. Returns (ok, critical):
//
//   - manifest ABSENT / unknown format / no index => (true, false): "unverified",
//     non-critical — a legacy index at the same schema has no manifest keys, and
//     treating absent as dirty would redden every existing user's first doctor.
//   - present + digest matches => (true, false): ok.
//   - present + mismatches, or the vault cannot be walked/read => (false, true):
//     the index provably does not reflect the vault (an out-of-band edit,
//     backup-restore, or a truncated walk) — critical.
//
// Recompute via allMemoryFiles, NEVER dirBytes: dirBytes swallows walk errors and
// would hash a silently-truncated set, reintroducing this very bug.
func indexMatchesVault(cfg Config) (ok bool, critical bool) {
	if _, err := os.Stat(dbPath(cfg)); err != nil {
		return true, false // no index to verify; index_fresh owns absence
	}
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		return true, false
	}
	defer db.Close()
	meta, err := readIndexMeta(db)
	if err != nil {
		return true, false
	}
	storedDigest := meta["vault_manifest_digest"]
	if storedDigest == "" || meta["vault_manifest_algo"] != indexManifestAlgo {
		return true, false // absent / unknown format => unverified, not red-on-upgrade
	}
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return false, true // cannot walk the vault => fail closed
	}
	lines := make([]string, 0, len(files))
	for _, p := range files {
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			continue // mirror the rebuild loop: an unreadable file gets no line
		}
		lines = append(lines, manifestLine(cfg, p, sha256.Sum256(b)))
	}
	if manifestDigestOf(lines) == storedDigest {
		return true, false
	}
	return false, true
}
