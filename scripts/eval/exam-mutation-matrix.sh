#!/usr/bin/env bash
set -euo pipefail

# Dated, manual audit for the obligation exam. This deliberately stays out of CI:
# it runs permanent typed mutants first, then rewrites disposable source copies and
# requires each planted mutant to make its named assembled-surface test fail.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GO="${GO:-go}"

run() {
  printf 'AUDIT %-44s ' "$1"
  shift
  (cd "$ROOT" && "$@" >/dev/null)
  printf 'CLOSED\n'
}

production_gates=(
  classifyMeetingBriefEvidence
  isMeetingNotification
  assignedToThirdParty
  memoryIsServiceOnly
  userOwnedOpenLoop
  meetingBriefIsTwoPartyExchange
  relationalEvidenceIDs
  meetingBriefResolveAttribution
  stripURLs
  unwrapHardWraps
  senderAuthoredBody
  stripSpeakerPrefix
  isForwardedSubject
  isLeadInFragment
  stripNoiseTokens
  gmailActionableAsk
  containsPhrase
)

for gate in "${production_gates[@]}"; do
  run "production/$gate" "$GO" test ./internal/mora/exam \
    -run "^TestScorerRedTeam/meeting/s_gate_disable_sweep/$gate$" -count=1
done

run "exam/validator-rules" "$GO" test ./internal/mora/exam \
  -run '^TestScoreRefusesEveryInvalidLedgerClass$' -count=1
run "exam/metric-relations" "$GO" test ./internal/mora/exam \
  -run '^(TestEveryMetricHasASabotageCase|TestEveryRegisteredSabotageMovesItsMetric|TestMetricRegistryCoversEveryScorecardField)$' -count=1
run "exam/red-team-manifest" "$GO" test ./internal/mora/exam \
  -run '^(TestScorerRedTeam|TestRedTeamManifestIsComplete)$' -count=1
run "exam/determinism" "$GO" test ./internal/mora/exam \
  -run '^(TestExamDeterminismGuard|TestManifestCompleteness|TestValidatorRuleWalkRejectsErrorWrapperIndirection)$' -count=1
run "exam/identity-lint-and-hash" "$GO" test ./internal/mora \
  -run '^(TestExamCorpusNoRealIdentities|TestExamCorpusHashesMatch|TestExamCorpusHashesRequireEverySourceArtifact)$' -count=1
run "exam/current-surfaces" "$GO" test ./internal/mora \
  -run '^(TestExamSurfaces|TestExamSurfaceClockGuard|TestDailyBriefHasNoObligationContract)$' -count=1
run "exam/correction-flywheel" "$GO" test ./internal/mora \
  -run '^TestExamCorrectionFlywheel$' -count=1

kill_mutant() {
  local name="$1" file="$2" old="$3" new="$4" pkg="$5" test_re="$6"
  local tmp work log
  tmp="$(mktemp -d)"
  work="$tmp/repo"
  log="$tmp/test.log"
  mkdir "$work"
  git -C "$ROOT" archive HEAD | tar -x -C "$work"
  FILE="$work/$file" OLD="$old" NEW="$new" python3 - <<'PY'
import os
from pathlib import Path

path = Path(os.environ["FILE"])
text = path.read_text()
old = os.environ["OLD"]
new = os.environ["NEW"]
count = text.count(old)
if count != 1:
    raise SystemExit(f"{path}: mutation anchor count = {count}, want 1")
path.write_text(text.replace(old, new))
PY
  printf 'MUTANT %-43s ' "$name"
  if (cd "$work" && "$GO" test "$pkg" -run "$test_re" -count=1 >"$log" 2>&1); then
    printf 'SURVIVED\n'
    cat "$log"
    rm -rf "$tmp"
    return 1
  fi
  printf 'KILLED\n'
  rm -rf "$tmp"
}

kill_mutant "surface/direct-wall-clock" \
  internal/mora/mora.go \
  'now := briefClock()
	added, err := syncTasks' \
  'now := time.Now()
	added, err := syncTasks' \
  ./internal/mora '^TestExamSurfaceClockGuard$'

kill_mutant "surface/daily-cap-drift" \
  internal/mora/digest.go \
  'digestDefaultCap   = 8' \
  'digestDefaultCap   = 9' \
  ./internal/mora '^TestExamSurfaces$'

kill_mutant "flywheel/delete-governance-arm" \
  internal/mora/exam_flywheel_test.go \
  $'\trunExamCLI(t, "merge", "confirm", "--handle", "+15550100137", "--email", "dana@example.net")\n\n\tpostPredictions := examMeetingPredictions(runExamEventCLI(t, event.EventID, at))' \
  $'\tpostPredictions := examMeetingPredictions(runExamEventCLI(t, event.EventID, at))' \
  ./internal/mora '^TestExamCorrectionFlywheel$'

kill_mutant "flywheel/neuter-rule-3" \
  internal/mora/graph.go \
  'for _, cm := range confirmedSorted {' \
  'for _, cm := range []confirmedMerge{} {' \
  ./internal/mora '^TestExamCorrectionFlywheel$'

kill_mutant "flywheel/pre-merge-already-correct" \
  internal/mora/exam_flywheel_test.go \
  $'\tprePredictions := examMeetingPredictions(runExamEventCLI(t, event.EventID, at))' \
  $'\trunExamCLI(t, "merge", "confirm", "--handle", "+15550100137", "--email", "dana@example.net")\n\tprePredictions := examMeetingPredictions(runExamEventCLI(t, event.EventID, at))' \
  ./internal/mora '^TestExamCorrectionFlywheel$'

kill_mutant "flywheel/graph-blind-scorer" \
  internal/mora/exam/baseline.go \
  'Before:     clonePredictions(in.FlywheelPre),' \
  'Before:     clonePredictions(in.FlywheelPost),' \
  ./internal/mora/exam '^TestScorerRedTeam/meeting/t_graph_state_insensitive$'

printf 'All obligation-exam mutation rows CLOSED.\n'
