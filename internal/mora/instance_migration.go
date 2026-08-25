package mora

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	ingestpkg "github.com/pyranthus-hq/mora/internal/ingest"
	"github.com/pyranthus-hq/mora/internal/memory"
	memfile "github.com/pyranthus-hq/mora/internal/memoryfile"
)

// Issue #495 — the per-instance naming boundary. Connector writes for a
// labeled account stamp the account onto the StableID ("gmail_thread/t1@work",
// see the ingestGoogle write callback), which files the memory under an
// instance-suffixed filename. Vaults that predate per-instance identity still
// hold the UNSUFFIXED file for the same provider object, so one sync leaves two
// memory files describing one provider object; both derive identical commitment
// opening refs (provider-identity message refs, internal/google/gmail.go), and
// `mora index rebuild` dies on the commitments UNIQUE constraint.
//
// migrateLegacyInstanceFile is the sync-time migration: called from the single
// connector-write chokepoint (writeMappedMemoryDetailed) AFTER the canonical
// instance-suffixed file is confirmed on disk, it removes the legacy unsuffixed
// twin — but ONLY when every safety gate passes:
//
//  1. The record actually carries an account suffix ("id@account").
//  2. The canonical suffixed file exists on disk (just published, or
//     content-hash unchanged). No canonical file ⇒ nothing supersedes the
//     legacy evidence and nothing is removed.
//  3. The candidate legacy file parses and its frontmatter proves it is the
//     SAME provider object: same base id, same Provider, same non-empty
//     ProviderID, and no account label of its own. Anything else stays put.
//  4. Currency: a strictly NEWER last_synced on the legacy file keeps it (the
//     vault is user evidence; the migration may only retire evidence that the
//     canonical file carries at least as currently).
//
// Every failure mode is a no-op with a stderr warning, never a sync error:
// migration is best-effort cleanup, and the rebuild-side twin defense
// (insertCommitmentRows) covers any stray pair that survives.
func migrateLegacyInstanceFile(cfg Config, mm memory.MappedMemory) {
	baseID, suffixed := splitInstanceSuffix(mm.StableID, mm.Account)
	if !suffixed {
		return // default-instance write; nothing to migrate
	}
	canonicalPath := ingestpkg.MappedTargetPath(cfg, mm)
	// Lstat + IsRegular on BOTH sides: a symlinked canonical (or legacy) could
	// otherwise alias the same underlying file, and removing the "legacy" path
	// would destroy the only real copy behind a dangling link.
	if fi, err := os.Lstat(canonicalPath); err != nil || !fi.Mode().IsRegular() {
		return // canonical not published as a regular file — never remove evidence without its replacement
	}
	legacyPath := filepath.Join(memfile.SourcesRoot(cfg), mm.Provider,
		memfile.OSSafeBase(memory.SafeFilename(baseID))+".md")
	if legacyPath == canonicalPath {
		return // defensive: must be unreachable for a genuinely suffixed id
	}
	if fi, err := os.Lstat(legacyPath); err != nil {
		return // no legacy file — the common, already-migrated case
	} else if !fi.Mode().IsRegular() {
		warnLegacyInstance("not a regular file; leaving in place", legacyPath, canonicalPath, nil)
		return
	}
	// The canonical file must itself prove it is the suffixed form of this
	// record before anything is retired on its authority.
	if canon, err := parseMemory(canonicalPath); err != nil || canon.ID != mm.StableID {
		warnLegacyInstance("canonical file failed identity check; leaving legacy in place", legacyPath, canonicalPath, err)
		return
	}
	parsed, err := parseMemory(legacyPath)
	if err != nil {
		warnLegacyInstance("unparseable; leaving in place", legacyPath, canonicalPath, err)
		return
	}
	if !isLegacyInstanceTwin(parsed, baseID, mm) {
		// Not provably the same provider object — leave it alone. Two files
		// that are not twins are distinct memories, not duplicates.
		return
	}
	if legacyInstanceNewer(parsed.LastSynced, mm.LastSynced) {
		warnLegacyInstance("legacy file has a newer last_synced; leaving in place", legacyPath, canonicalPath, nil)
		return
	}
	if err := os.Remove(legacyPath); err != nil {
		warnLegacyInstance("remove failed", legacyPath, canonicalPath, err)
		return
	}
	fmt.Fprintf(os.Stderr, "migrated legacy source file %s -> %s (same provider object, per-instance naming)\n", legacyPath, filepath.Base(canonicalPath))
}

// splitInstanceSuffix strips the "@<account>" tail a labeled-account connector
// appended to a StableID. ok=false for default-instance ids (no account).
func splitInstanceSuffix(stableID, account string) (base string, ok bool) {
	if account == "" {
		return "", false
	}
	return strings.CutSuffix(stableID, "@"+account)
}

// isLegacyInstanceTwin reports whether parsed is frontmatter-provable evidence
// of the SAME provider object as mm's pre-suffix form.
func isLegacyInstanceTwin(parsed Memory, baseID string, mm memory.MappedMemory) bool {
	return parsed.ID == baseID &&
		parsed.Provider == mm.Provider &&
		mm.ProviderID != "" &&
		parsed.ProviderID == mm.ProviderID &&
		parsed.Account == ""
}

// legacyInstanceNewer reports whether the legacy file's last_synced parses on
// BOTH sides and is strictly after the incoming record's. Unparseable or empty
// stamps never block migration: the incoming record is fresh connector output
// by construction.
func legacyInstanceNewer(legacySynced, incomingSynced string) bool {
	tLegacy, errLegacy := time.Parse(time.RFC3339, legacySynced)
	tIncoming, errIncoming := time.Parse(time.RFC3339, incomingSynced)
	return errLegacy == nil && errIncoming == nil && tLegacy.After(tIncoming)
}

func warnLegacyInstance(what, legacyPath, canonicalPath string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: legacy instance migration skipped (%s): %s vs %s: %v\n", what, legacyPath, canonicalPath, err)
		return
	}
	fmt.Fprintf(os.Stderr, "warn: legacy instance migration skipped (%s): %s vs %s\n", what, legacyPath, canonicalPath)
}
