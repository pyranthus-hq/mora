#!/usr/bin/env bash
# Bootstrap the "Mora — Product Release" GitHub Project (Projects v2).
#
# A reusable product-release template: roadmap + PR pipeline + setup checklist +
# review findings on one board. Idempotent-ish — re-running creates a NEW project
# (Projects v2 has no natural unique key from the CLI), so run once.
#
# Requires: gh authed with the `project` scope.  Grant it with:
#     gh auth refresh -s project
#
# Usage: sh scripts/bootstrap-release-project.sh [owner]
set -euo pipefail

OWNER="${1:-pyranthus-hq}"
TITLE="Mora — Product Release"

command -v gh  >/dev/null || { echo "gh not found"; exit 1; }
command -v jq  >/dev/null || { echo "jq not found (brew install jq)"; exit 1; }

# Fail early with a friendly message if the project scope is missing.
if ! gh project list --owner "$OWNER" >/dev/null 2>&1; then
  echo "ERROR: gh is missing the 'project' scope. Run:  gh auth refresh -s project"
  exit 1
fi

echo "Creating project '$TITLE' under @$OWNER ..."
PROJECT_JSON=$(gh project create --owner "$OWNER" --title "$TITLE" --format json)
NUMBER=$(echo "$PROJECT_JSON" | jq -r '.number')
PROJECT_ID=$(echo "$PROJECT_JSON" | jq -r '.id')
URL=$(echo "$PROJECT_JSON" | jq -r '.url')
echo "  -> #$NUMBER  $URL"

echo "Creating custom fields ..."
gh project field-create "$NUMBER" --owner "$OWNER" --name "Priority" \
  --data-type SINGLE_SELECT --single-select-options "P0,P1,P2" >/dev/null
gh project field-create "$NUMBER" --owner "$OWNER" --name "Phase" \
  --data-type SINGLE_SELECT \
  --single-select-options "Done,NOW,D1 CI-CD,I1 Graph,I2 Retrieval,I3 Think,I4 Proactive,W1 Wiki,Breadth,Setup,Review-fix" >/dev/null
gh project field-create "$NUMBER" --owner "$OWNER" --name "Kind" \
  --data-type SINGLE_SELECT --single-select-options "PR,Setup,Review-finding,Decision" >/dev/null

FIELDS=$(gh project field-list "$NUMBER" --owner "$OWNER" --format json)
field_id()  { echo "$FIELDS" | jq -r --arg n "$1" '.fields[] | select(.name==$n) | .id'; }
option_id() { echo "$FIELDS" | jq -r --arg n "$1" --arg o "$2" \
              '.fields[] | select(.name==$n) | .options[] | select(.name==$o) | .id'; }

PHASE_F=$(field_id Phase); PRIO_F=$(field_id Priority); KIND_F=$(field_id Kind)

# add "<title>" "<body>" "<phase>" "<priority>" "<kind>"
add() {
  local title="$1" body="$2" phase="$3" prio="$4" kind="$5" id
  id=$(gh project item-create "$NUMBER" --owner "$OWNER" --title "$title" --body "$body" --format json | jq -r '.id')
  gh project item-edit --id "$id" --project-id "$PROJECT_ID" --field-id "$PHASE_F" --single-select-option-id "$(option_id Phase  "$phase")" >/dev/null
  gh project item-edit --id "$id" --project-id "$PROJECT_ID" --field-id "$PRIO_F"  --single-select-option-id "$(option_id Priority "$prio")"  >/dev/null
  gh project item-edit --id "$id" --project-id "$PROJECT_ID" --field-id "$KIND_F"  --single-select-option-id "$(option_id Kind  "$kind")"  >/dev/null
  echo "  + [$phase/$prio/$kind] $title"
}

echo "Adding items ..."

# --- Release pipeline (PRs) -------------------------------------------------
add "PR0 — Merge the iMessage decode fix" \
    "feat/imessage-connector -> main. URGENT, no CI needed. Neil's current build silently drops most messages. Merge + re-ship by hand today." \
    "NOW" "P0" "PR"
add "PR1 — CI foundation (PR gate)" \
    "ci/pr-gate. ci.yml: go test -race / vet / gofmt / golangci-lint / cross-arch build matrix / gitleaks / size. Turn on branch protection." \
    "D1 CI-CD" "P1" "PR"
add "PR2 — Release pipeline (GoReleaser + Homebrew cask)" \
    "ci/release. .goreleaser.yaml + release.yml + create pyranthus-hq/homebrew-tap + install.sh. Tag v0.2.0 -> binaries + checksums + cosign + cask. Replaces hand-zipping." \
    "D1 CI-CD" "P1" "PR"
add "PR3 — mora upgrade self-update" \
    "feat/self-update. go-selfupdate vs GitHub Releases; atomic same-dir swap + re-exec; refuse on Homebrew installs. Needs var version/commit/date in cmd/mora. The 'auto-update like Claude Code' piece." \
    "D1 CI-CD" "P1" "PR"
add "PR4 — AI review agents" \
    "ci/ai-review. claude.yml (on-demand) + AGENTS.md + connect the Codex GitHub app. Codex auto (free), Claude on-demand." \
    "D1 CI-CD" "P2" "PR"
add "PR5 — Finish Phase 2 (iMessage)" \
    "feat/imessage-connector cont. Plans 02-03..02-06: AddressBook render, attachments, lookback/deny-list, mora wiring, MCP search. Ships through the new pipeline." \
    "NOW" "P1" "PR"
add "PR6 — I1 Entity graph foundation" \
    "feat/entity-graph. 3 sub-PRs: (6a) entities/edges schema + bi-temporal cols + parseGraph() over the vault; (6b) gazetteer/alias body matching + handle/email resolution; (6c) get_entity MCP tool." \
    "I1 Graph" "P1" "PR"
add "PR7 — I2 Hybrid retrieval" \
    "feat/hybrid-retrieval. (7a) pure-Go potion/model2vec embeddings; (7b) RRF fusion FTS5+vector; (7c) 1-2-hop graph expansion; (7d) Ollama opt-in embedder." \
    "I2 Retrieval" "P2" "PR"
add "PR8 — I3 think + gap analysis" \
    "feat/think. Upgrade context_memory to a synthesis envelope: deterministic retrieve+gap-detect in Go, prose via MCP sampling. \$0 synthesis." \
    "I3 Think" "P2" "PR"
add "PR9 — I4 Proactive surfacing" \
    "feat/proactive. Session-start briefing (MCP resource) + scheduled digest + 'you should know' nudges with cooldown ledger." \
    "I4 Proactive" "P2" "PR"
add "PR10 — W1 Wiki adoption (replace gbrain)" \
    "feat/wiki-adoption. Entity-typed taxonomy + Productivity/memory conventions; point vault_dir at the real wiki. The 'actually replace gbrain' PR." \
    "W1 Wiki" "P2" "PR"
add "PR11+ — Breadth: C1 MCP client -> C2 PostHog/Linear -> C3 BetterStack/GitHub -> C4 digest" \
    "Deferred behind the brain; still ships to Neil." \
    "Breadth" "P2" "PR"

# --- Setup checklist (user-owned) ------------------------------------------
add "Setup — Add repo secret ANTHROPIC_API_KEY" \
    "Or run 'claude setup-token' for CLAUDE_CODE_OAUTH_TOKEN on the Max plan. Needed by claude.yml." \
    "Setup" "P1" "Setup"
add "Setup — Add repo secret HOMEBREW_TAP_TOKEN" \
    "Fine-grained PAT with contents:write on pyranthus-hq/homebrew-tap. Needed by the release job." \
    "Setup" "P1" "Setup"
add "Setup — Create pyranthus-hq/homebrew-tap repo" \
    "GoReleaser publishes the cask here -> brew install pyranthus-hq/tap/mora." \
    "Setup" "P1" "Setup"
add "Setup — Connect the Codex GitHub app + enable Automatic reviews" \
    "chatgpt.com/codex/settings/code-review. Free under the ChatGPT plan; reads AGENTS.md." \
    "Setup" "P2" "Setup"

# --- Open decisions --------------------------------------------------------
add "Decide — Blocking vs advisory AI review" \
    "Default: advisory (deterministic gates are the only hard blockers). Allow Claude/Codex to block on a P0?" \
    "Setup" "P2" "Decision"
add "Decide — Both agents auto, or Codex-auto + Claude-on-demand" \
    "Default: Codex-auto + Claude-on-demand (avoids 2x comments)." \
    "Setup" "P2" "Decision"
add "Decide — License: Apache-2.0 vs MIT" \
    "Placeholder Apache-2.0 in .goreleaser.yaml + AGENTS.md. Needed for the cask + SPDX headers." \
    "Setup" "P1" "Decision"
add "Decide — CI/CD lands on main directly, or staged on a review branch first" \
    "PR1/PR2 target. Straight-to-main vs a branch you review first." \
    "Setup" "P2" "Decision"

# --- Cross-model review findings (Codex, 2026-06-03) -----------------------
add "Review — Verify action majors vs GitHub Node24 forcing" \
    "Codex P1: checkout@v4 / setup-go@v5 / goreleaser-action@v6 / golangci-lint-action@v6 are Node20-era. Verify whether Node24-compatible majors exist and bump before mid-2026 forcing. Verify, don't blind-bump." \
    "Review-fix" "P1" "Review-finding"
add "Review — .goreleaser name_template vs go-selfupdate matching" \
    "Codex P1: name_template embeds {{.Version}} (mora_0.2.0_darwin_arm64.tar.gz). Confirm creativeprojects/go-selfupdate matches this, or add a filter / drop the version segment so 'mora upgrade' finds assets." \
    "Review-fix" "P1" "Review-finding"
add "Review — homebrew_casks: binary vs binaries (list)" \
    "Codex P1: cask uses 'binary: mora'; current GoReleaser v2 cask schema may want 'binaries:' as a list. Verify against the v2 schema; fix if it fails validation." \
    "Review-fix" "P1" "Review-finding"

echo
echo "Done. Project #$NUMBER -> $URL"
echo "Next: open it in the browser, add a Board view grouped by Status and a Table view grouped by Phase."
