package mora

import (
	"path/filepath"
	"time"
)

// briefArtifactPath returns the dated vault artifact path for a brief:
// <VaultDir>/briefs/<YYYY-MM-DD>-brief.md.
//
// The date comes from the INJECTED now (never a fresh time.Now()) so date-based
// tests are deterministic and the artifact date matches the same now already
// threaded through buildDigest / saveBriefSnapshot (D13-3). now is canonicalized
// to UTC so a late-local-evening run lands on a stable UTC day — the file for a
// given logical day is the same regardless of the runner's zone.
//
// briefs/ is a NEW top-level vault subdir, a sibling of sources/, under VaultDir.
// It is the durable, greppable, re-openable artifact — distinct from the Phase-12
// watermark, which lives at <StateDir>/brief/ (singular). Do NOT conflate the two:
// this path never touches StateDir.
func briefArtifactPath(cfg Config, now time.Time) string {
	return filepath.Join(cfg.VaultDir, "briefs", now.UTC().Format("2006-01-02")+"-brief.md")
}

// writeBriefArtifact renders an already-built Digest to the dated vault artifact
// at briefArtifactPath(cfg, now) and returns the path written.
//
// The body is EXACTLY renderDigest(d, cfg.contextDefaultTokens()*charsPerToken) — the
// same Markdown the human brief and MCP digest emit, so there is one source of
// truth for brief rendering. The write goes through atomicWriteDurable (synced
// temp + rename + parent-directory sync) so a crash mid-write never leaves a
// torn or post-checkpoint-missing brief (T-13-01), and a same-day re-run
// OVERWRITES that day's file — one file per day, no proliferation (SC#4).
//
// Mode 0644: the artifact is human-readable vault content (like memories under
// sources/), NOT secret like the 0600 watermark.
//
// This is LOCAL-ONLY (zero egress — no net/* import here) and is INDEPENDENT of
// the Phase-12 watermark: it never calls saveBriefSnapshot / acquireBriefLock, so
// persisting the artifact does NOT advance the delta. The watermark stays gated on
// --advance (D13-3 / SC#4).
func writeBriefArtifact(cfg Config, d Digest, now time.Time) (string, error) {
	return writeBriefArtifactAt(cfg, d, now, cfg.contextDefaultTokens()*charsPerToken)
}

// writeBriefArtifactAt persists a Digest at an EXPLICIT budget. The scheduled
// --advance transaction (advanceBrief, issue #62 defect 1) passes the SAME budget it
// used to compute the survivor set, and a digest already budgeted to that budget, so
// renderDigest here is idempotent — the persisted artifact contains exactly the items
// the watermark commit reflects, never more, never fewer.
func writeBriefArtifactAt(cfg Config, d Digest, now time.Time, budgetChars int) (string, error) {
	path := briefArtifactPath(cfg, now)
	body := renderDigest(d, budgetChars)
	if err := atomicWriteDurable(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
