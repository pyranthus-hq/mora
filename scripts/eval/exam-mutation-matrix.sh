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
  run "consequence/$gate" "$GO" test ./internal/mora/exam \
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

kill_mutant "production/classifyMeetingBriefEvidence" \
  internal/mora/meetingbrief.go \
  'kind := classifyMeetingBriefEvidence(m, cfg, at)' \
  'kind := meetingBriefOpenLoops' \
  ./internal/mora '^TestSabotageGibberishNeverRenders$'

kill_mutant "production/isMeetingNotification" \
  internal/mora/meetingbrief.go \
  $'if isMeetingNotification(m) {\n\t\treturn ""\n\t}' \
  $'if false {\n\t\treturn ""\n\t}' \
  ./internal/mora '^TestSabotageGibberishNeverRenders$'

kill_mutant "production/assignedToThirdParty" \
  internal/mora/meetingbrief.go \
  $'if assignedToThirdParty(signalText(m), selfNameTokens(selfEmails(cfg))) {\n\t\treturn ""\n\t}' \
  $'if false {\n\t\treturn ""\n\t}' \
  ./internal/mora '^TestSabotageGibberishNeverRenders$'

kill_mutant "production/memoryIsServiceOnly" \
  internal/mora/digest.go \
  $'\treturn true\n}\n\n// isLowSignalItem' \
  $'\treturn false\n}\n\n// isLowSignalItem' \
  ./internal/mora '^TestExamServiceOnlyGateIsAssembled$'

kill_mutant "production/userOwnedOpenLoop" \
  internal/mora/meetingbrief.go \
  $'if userOwnedOpenLoop(m, cfg) {\n\t\treturn meetingBriefOpenLoops\n\t}' \
  $'if true {\n\t\treturn meetingBriefOpenLoops\n\t}' \
  ./internal/mora '^TestSabotageGibberishNeverRenders$'

kill_mutant "production/meetingBriefIsTwoPartyExchange" \
  internal/mora/meetingbrief.go \
  $'if isGmailMemory(m) && !meetingBriefIsTwoPartyExchange(m, self, roster...) {\n\t\t\t\tcontinue\n\t\t\t}' \
  $'if false {\n\t\t\t\tcontinue\n\t\t\t}' \
  ./internal/mora '^TestSabotageGibberishNeverRenders$'

kill_mutant "production/relationalEvidenceIDs" \
  internal/mora/meetingbrief.go \
  $'if rel, _ := e["rel"].(string); rel == graphRelMentions {\n\t\t\tcontinue\n\t\t}' \
  $'if false {\n\t\t\tcontinue\n\t\t}' \
  ./internal/mora '^TestMeetingBriefRejectsMentionOnlyEvidenceAsObligation$'

kill_mutant "production/meetingBriefResolveAttribution" \
  internal/mora/meetingbrief.go \
  'candidate, unambiguous := meetingBriefResolveAttribution(associationsByMemory[id], lineDecisions)' \
  $'candidate, unambiguous := associationsByMemory[id][0], true\n\t\t_ = lineDecisions' \
  ./internal/mora '^TestMeetingBriefDropsAmbiguousOutboundGroupAttribution$'

kill_mutant "production/stripURLs" \
  internal/mora/meetingbrief.go \
  'text = unwrapHardWraps(stripURLs(text))' \
  'text = unwrapHardWraps(text)' \
  ./internal/mora '^TestSabotageGibberishNeverRenders$'

printf 'MUTANT %-43s HOLE issue #139 expires 2026-07-21\n' "production/unwrapHardWraps"

kill_mutant "production/senderAuthoredBody" \
  internal/mora/meetingbrief.go \
  'body := senderAuthoredBody(stripFromLine(m.Text))' \
  'body := stripFromLine(m.Text)' \
  ./internal/mora '^TestExamAuthoredToQuotedDisappearsFromTheRealBrief$'

kill_mutant "production/stripSpeakerPrefix" \
  internal/mora/meetingbrief.go \
  $'func stripSpeakerPrefix(segment string) string {\n\treturn strings.TrimSpace(speakerPrefix.ReplaceAllString(segment, ""))\n}' \
  $'func stripSpeakerPrefix(segment string) string {\n\treturn segment\n}' \
  ./internal/mora '^TestExamIMessageSpeakerPrefixIsNotProductText$'

printf 'MUTANT %-43s HOLE issue #139 expires 2026-07-21\n' "production/isForwardedSubject"
printf 'MUTANT %-43s HOLE issue #139 expires 2026-07-21\n' "production/isLeadInFragment"

kill_mutant "production/stripNoiseTokens" \
  internal/mora/meetingbrief.go \
  'segment := stripNoiseTokens(rawSegment)' \
  'segment := strings.TrimSpace(rawSegment)' \
  ./internal/mora '^TestExamCorrectionFlywheel$'

printf 'MUTANT %-43s HOLE issue #139 expires 2026-07-21\n' "production/gmailActionableAsk"

kill_mutant "production/containsPhrase" \
  internal/mora/meetingbrief.go \
  $'func containsPhrase(text, phrase string) bool {\n\tif phrase == ""' \
  $'func containsPhrase(text, phrase string) bool {\n\treturn strings.Contains(text, phrase)\n\tif phrase == ""' \
  ./internal/mora '^TestSabotageGibberishNeverRenders$'

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

printf 'Obligation-exam mutation audit complete; dated holes are listed above.\n'
