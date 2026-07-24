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
# Known RED through 2026-10-14: the strict target is tracked by #138/#154.
run_red "exam/product-target-strict (#138)" env MORA_EXAM_PRODUCT_TARGET=1 "$GO" test ./internal/mora \
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

kill_mutant "production/unwrapHardWraps" \
  internal/mora/meetingbrief.go \
  $'func unwrapHardWraps(text string) string {\n\tlines := strings.Split(text, "\\n")\n\tvar out strings.Builder\n\tfor i, line := range lines {\n\t\tout.WriteString(line)\n\t\tif i == len(lines)-1 {\n\t\t\tbreak\n\t\t}\n\t\ttrimmed := strings.TrimRight(line, " \\t")\n\t\tnext := strings.TrimLeft(lines[i+1], " \\t")\n\t\tif continuesSentence(trimmed, next) {\n\t\t\tout.WriteByte(\' \')\n\t\t\tcontinue\n\t\t}\n\t\tout.WriteByte(\'\\n\')\n\t}\n\treturn out.String()\n}' \
  $'func unwrapHardWraps(text string) string {\n\treturn text\n}' \
  ./internal/mora '^TestExamHardWrapJoinsBeforeSegmenting$'

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

kill_mutant "production/isForwardedSubject" \
  internal/mora/meetingbrief.go \
  $'func isForwardedSubject(title string) bool {\n\tlower := strings.ToLower(strings.TrimSpace(title))\n\treturn strings.HasPrefix(lower, "fwd:") || strings.HasPrefix(lower, "fw:")\n}' \
  $'func isForwardedSubject(title string) bool {\n\treturn false\n}' \
  ./internal/mora '^TestExamForwardedSubjectNeverBecomesEvidence$'

kill_mutant "production/isLeadInFragment" \
  internal/mora/meetingbrief.go \
  $'func isLeadInFragment(text string) bool {\n\tt := strings.TrimSpace(text)\n\tif t == "" {\n\t\treturn true\n\t}\n\tif strings.HasSuffix(t, ":") {\n\t\treturn true\n\t}\n\t// A "sentence" of one or two words is a header, not a statement.\n\treturn len(strings.Fields(t)) < 3\n}' \
  $'func isLeadInFragment(text string) bool {\n\treturn false\n}' \
  ./internal/mora '^TestExamLeadInFragmentNeverBecomesEvidence$'

kill_mutant "production/stripNoiseTokens" \
  internal/mora/meetingbrief.go \
  'segment := stripNoiseTokens(rawSegment)' \
  'segment := strings.TrimSpace(rawSegment)' \
  ./internal/mora '^TestExamCorrectionFlywheel$'

kill_mutant "production/gmailActionableAsk" \
  internal/mora/meetingbrief.go \
  $'func gmailActionableAsk(text string) bool {\n\tif !actionableQuestion(text) {\n\t\treturn false\n\t}\n\tlower := strings.ToLower(text)\n\treturn containsAnyPhrase(lower, interrogativeOpeners) || containsAnyPhrase(lower, directRequestPhrases)\n}' \
  $'func gmailActionableAsk(text string) bool {\n\treturn actionableQuestion(text)\n}' \
  ./internal/mora '^TestExamGmailBareQuestionNeedsRealInterrogative$'

kill_mutant "production/containsPhrase" \
  internal/mora/meetingbrief.go \
  $'func containsPhrase(text, phrase string) bool {\n\tif phrase == ""' \
  $'func containsPhrase(text, phrase string) bool {\n\treturn strings.Contains(text, phrase)\n\tif phrase == ""' \
  ./internal/mora '^TestSabotageGibberishNeverRenders$'

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
