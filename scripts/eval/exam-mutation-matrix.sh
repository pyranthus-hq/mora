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

run_red() {
  printf 'AUDIT %-44s ' "$1"
  shift
  if (cd "$ROOT" && "$@" >/dev/null 2>&1); then
    printf 'UNEXPECTEDLY GREEN\n'
    return 1
  fi
  printf 'RED\n'
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
run "exam/integrity-exit" "$GO" test ./internal/mora \
  -run '^TestExamIntegrityExit$' -count=1
run "exam/current-surfaces" "$GO" test ./internal/mora \
  -run '^(TestExamSurfaces|TestExamSurfacesV2|TestExamSurfaceClockGuard|TestDailyBriefHasNoObligationContract)$' -count=1
run "product/open-loop-lane-reconciliation (#155)" "$GO" test ./internal/mora \
  -run '^(TestOpenLoopLanesNeverContradict|TestThinkOpenLoopsEvidenceIsAuthoritative)$' -count=1
# Strict target went green on 2026-07-24 (#204); wantRED is retired and the
# ratchet in TestExamProductTarget now fails on any strict regression.
run "exam/product-target-strict (#138)" env MORA_EXAM_PRODUCT_TARGET=1 "$GO" test ./internal/mora \
  -run '^TestExamProductTarget$' -count=1
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
  internal/meeting/classify.go \
  'return Unresolved' \
  'return OpenLoops' \
  ./internal/meeting '^TestClassifyEvidencePolicy$'


kill_mutant "production/isMeetingNotification" \
  internal/meeting/classify.go \
  'return ContainsAnyPhrase(strings.ToLower(m.Text), meetingNotificationBodyMarkers)' \
  'return false' \
  ./internal/meeting '^TestMeetingNotificationMailIsNotEvidence$'


kill_mutant "production/assignedToThirdParty" \
  internal/meeting/classify.go \
  'if !selfNames[assignee[0]] {' \
  'if false {' \
  ./internal/meeting '^TestThirdPartyActionItemIsNotTheUsersOpenLoop$'


kill_mutant "production/memoryIsServiceOnly" \
  internal/mora/digest.go \
  $'\treturn true\n}\n\n// isLowSignalItem' \
  $'\treturn false\n}\n\n// isLowSignalItem' \
  ./internal/mora '^TestExamServiceOnlyGateIsAssembled$'

kill_mutant "production/userOwnedOpenLoop" \
  internal/meeting/classify.go \
  'if UserOwnedOpenLoop(m, in.SignalText, in.Self) {' \
  'if false {' \
  ./internal/meeting '^TestClassifyEvidencePolicy$'


kill_mutant "production/meetingBriefIsTwoPartyExchange" \
  internal/meeting/classify.go \
  'if key != "" && !inRoom[key] {' \
  'if false {' \
  ./internal/meeting '^TestInboundGroupThreadIsNotTwoPartyBusiness$'


kill_mutant "production/relationalEvidenceIDs" \
  internal/mora/meetingbrief.go \
  $'if rel, _ := e["rel"].(string); rel == graphRelMentions {\n\t\t\tcontinue\n\t\t}' \
  $'if false {\n\t\t\tcontinue\n\t\t}' \
  ./internal/mora '^TestExamIntegrityExit$'


kill_mutant "production/meetingBriefResolveAttribution" \
  internal/mora/meetingbrief.go \
  'candidate, unambiguous := meetingBriefResolveAttribution(associationsByMemory[id], lineDecisions)' \
  $'candidate, unambiguous := associationsByMemory[id][0], true\n\t\t_ = lineDecisions' \
  ./internal/mora '^TestExamIntegrityExit$'


kill_mutant "production/stripURLs" \
  internal/evidencetext/text.go \
  'text = UnwrapHardWraps(StripURLs(text))' \
  'text = UnwrapHardWraps(text)' \
  ./internal/evidencetext '^TestEvidenceTextHelpers$'


kill_mutant "production/unwrapHardWraps" \
  internal/evidencetext/text.go \
  'if ContinuesSentence(trimmed, next) {' \
  'if false {' \
  ./internal/evidencetext '^TestEvidenceSegmentsDoNotTruncateMidClause$'

kill_mutant "production/senderAuthoredBody" \
  internal/evidencetext/text.go \
  'if quotedReplyLine.MatchString(line) || isSignatureDelimiter(line) {' \
  'if false {' \
  ./internal/evidencetext '^TestForwardedAndQuotedContentIsNotTheSendersWords$'


kill_mutant "production/stripSpeakerPrefix" \
  internal/evidencetext/text.go \
  'return strings.TrimSpace(speakerPrefix.ReplaceAllString(segment, ""))' \
  'return segment' \
  ./internal/evidencetext '^TestEvidenceTextHelpers$'


kill_mutant "production/isForwardedSubject" \
  internal/evidencetext/text.go \
  'return strings.HasPrefix(lower, "fwd:") || strings.HasPrefix(lower, "fw:")' \
  'return false' \
  ./internal/evidencetext '^TestEvidenceTextHelpers$'


kill_mutant "production/isLeadInFragment" \
  internal/evidencetext/text.go \
  'return len(strings.Fields(t)) < 3' \
  'return false' \
  ./internal/evidencetext '^TestEvidenceTextHelpers$'


kill_mutant "production/stripNoiseTokens" \
  internal/evidencetext/text.go \
  'if !TokenIsNoise(tok) {' \
  'if true {' \
  ./internal/evidencetext '^TestStripNoiseTokens$'

kill_mutant "production/gmailActionableAsk" \
  internal/meeting/classify.go \
  'return ContainsAnyPhrase(lower, interrogativeOpeners) || DirectRequest(lower)' \
  'return true' \
  ./internal/meeting '^TestGmailActionableAsk_StrictForEmail$'


kill_mutant "production/containsPhrase" \
  internal/evidencetext/text.go \
  'if okBefore && okAfter {' \
  'if true {' \
  ./internal/evidencetext '^TestEvidenceTextHelpers$'


kill_mutant "surface/direct-wall-clock" \
  internal/mora/mora.go \
  'now := briefClock()
	if *loopID != "" {' \
  'now := time.Now()
	if *loopID != "" {' \
  ./internal/mora '^TestExamSurfaceClockGuard$'

kill_mutant "surface/daily-cap-drift" \
  internal/mora/digest.go \
  'digestDefaultCap   = 8' \
  'digestDefaultCap   = 9' \
  ./internal/mora '^TestExamIntegrityExit$'


kill_mutant "flywheel/delete-governance-arm" \
  internal/mora/exam_flywheel_test.go \
  $'\trunExamCLI(t, "merge", "confirm", "--handle", "+15550100137", "--email", "dana@example.net", "--yes")\n\n\tpostPredictions := examMeetingPredictions(runExamEventCLI(t, event.EventID, at))' \
  $'\tpostPredictions := examMeetingPredictions(runExamEventCLI(t, event.EventID, at))' \
  ./internal/mora '^TestExamCorrectionFlywheel$'

kill_mutant "flywheel/neuter-rule-3" \
  internal/graph/graph.go \
  'for _, cm := range confirmedSorted {' \
  'for _, cm := range []confirmedMerge{} {' \
  ./internal/mora '^TestExamCorrectionFlywheel$'

kill_mutant "flywheel/pre-merge-already-correct" \
  internal/mora/exam_flywheel_test.go \
  $'\tprePredictions := examMeetingPredictions(runExamEventCLI(t, event.EventID, at))' \
  $'\trunExamCLI(t, "merge", "confirm", "--handle", "+15550100137", "--email", "dana@example.net", "--yes")\n\tprePredictions := examMeetingPredictions(runExamEventCLI(t, event.EventID, at))' \
  ./internal/mora '^TestExamCorrectionFlywheel$'

kill_mutant "flywheel/graph-blind-scorer" \
  internal/mora/exam/baseline.go \
  'Before:     clonePredictions(in.FlywheelPre),' \
  'Before:     clonePredictions(in.FlywheelPost),' \
  ./internal/mora/exam '^TestScorerRedTeam/meeting/t_graph_state_insensitive$'

printf 'Obligation-exam mutation audit complete; dated holes are listed above.\n'
