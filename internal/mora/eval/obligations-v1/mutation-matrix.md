# Obligation exam mutation matrix — 2026-07-14

Command: `scripts/eval/exam-mutation-matrix.sh`

`CLOSED` means the named permanent mutant or planted disposable-copy source
mutation makes the cited test red. `HOLE` means there is no load-bearing gate
yet; every hole is issue-owned and expires. This is a dated manual audit, not a
CI job.

## Matrix 1 — production exclusion gates

Each row first runs its
`TestScorerRedTeam/meeting/s_gate_disable_sweep/<gate>` consequence control.
The `CLOSED` rows additionally rewrite the real production call site or body in
a disposable source copy and require the named assembled `internal/mora` test
to fail. A synthetic consequence control alone is not production mutation
coverage.

| Production gate | Result |
|---|---|
| `classifyMeetingBriefEvidence` | CLOSED — `TestSabotageGibberishNeverRenders` goes red |
| `isMeetingNotification` | CLOSED — `TestSabotageGibberishNeverRenders` goes red |
| `assignedToThirdParty` | CLOSED — `TestSabotageGibberishNeverRenders` goes red |
| `memoryIsServiceOnly` | CLOSED — `TestExamServiceOnlyGateIsAssembled` goes red |
| `userOwnedOpenLoop` | CLOSED — `TestSabotageGibberishNeverRenders` goes red |
| `meetingBriefIsTwoPartyExchange` | CLOSED — `TestSabotageGibberishNeverRenders` goes red |
| `relationalEvidenceIDs` | CLOSED — `TestMeetingBriefRejectsMentionOnlyEvidenceAsObligation` goes red |
| `meetingBriefResolveAttribution` | CLOSED — `TestMeetingBriefDropsAmbiguousOutboundGroupAttribution` goes red |
| `stripURLs` | CLOSED — `TestSabotageGibberishNeverRenders` goes red |
| `unwrapHardWraps` | HOLE — issue #139, expires 2026-07-21 |
| `senderAuthoredBody` | CLOSED — `TestExamAuthoredToQuotedDisappearsFromTheRealBrief` goes red |
| `stripSpeakerPrefix` | CLOSED — `TestExamIMessageSpeakerPrefixIsNotProductText` goes red |
| `isForwardedSubject` | HOLE — issue #139, expires 2026-07-21 |
| `isLeadInFragment` | HOLE — issue #139, expires 2026-07-21 |
| `stripNoiseTokens` | CLOSED — `TestExamCorrectionFlywheel` goes red |
| `gmailActionableAsk` | HOLE — issue #139, expires 2026-07-21 |
| `containsPhrase` | CLOSED — `TestSabotageGibberishNeverRenders` goes red |

## Matrix 2 — exam machinery

### Validator and corpus integrity

| Gate | Result |
|---|---|
| Rule `identity` | CLOSED — `TestScoreRefusesEveryInvalidLedgerClass/identity` goes red |
| Rule `timestamp` | CLOSED — `TestScoreRefusesEveryInvalidLedgerClass/timestamp` goes red |
| Rule `transition` | CLOSED — `TestScoreRefusesEveryInvalidLedgerClass/transition` goes red |
| Rule `direction` | CLOSED — `TestScoreRefusesEveryInvalidLedgerClass/direction` goes red |
| Rule `closure` | CLOSED — `TestScoreRefusesEveryInvalidLedgerClass/closure` goes red |
| Rule `evidence_span` | CLOSED — `TestScoreRefusesEveryInvalidLedgerClass/evidence_span` goes red |
| Rule `reply_chain_quotes` | CLOSED — `TestScoreRefusesEveryInvalidLedgerClass/reply_chain_quotes` goes red |
| Rule `self_is_attendee` | CLOSED — `TestScoreRefusesEveryInvalidLedgerClass/self_is_attendee` goes red |
| Rule `one_defect_per_artifact` | CLOSED — `TestScoreRefusesEveryInvalidLedgerClass/one_defect_per_artifact` goes red |
| Rule `class_balance` | CLOSED — `TestScoreRefusesEveryInvalidLedgerClass/class_balance` goes red |
| Rule `persona_hygiene` | CLOSED — `TestScoreRefusesEveryInvalidLedgerClass/persona_hygiene` goes red |
| Rule `channel_grain_connector_survivability` | CLOSED — `TestScoreRefusesEveryInvalidLedgerClass/channel_grain_connector_survivability` goes red |
| Rule vocabulary/wrapper walk | CLOSED — `TestManifestCompleteness` and `TestValidatorRuleWalkRejectsErrorWrapperIndirection` go red |
| Ledger real-identity lint | CLOSED — planted identity makes `TestExamCorpusNoRealIdentities` go red |
| Rendered-byte real-identity lint | CLOSED — planted body domain makes `TestExamCorpusNoRealIdentities` go red |
| Corpus byte changed | CLOSED — `TestExamCorpusHashesMatch` goes red |
| Ledger changed without render | CLOSED — `TestExamCorpusMatchesLedger` goes red |
| Hash manifest deleted/incomplete | CLOSED — `TestExamCorpusHashesRequireEverySourceArtifact` goes red |
| Determinism (`time.Now`, randomness, forbidden imports) | CLOSED — `TestExamDeterminismGuard` goes red |

### Scorecard dimensions

Each registered dimension is required to name a sabotage row, move under that
row, and correspond to a real `Scorecard` field. The combined mutation gate is
`TestEveryMetricHasASabotageCase`,
`TestEveryRegisteredSabotageMovesItsMetric`, and
`TestMetricRegistryCoversEveryScorecardField`.

| Dimension | Result |
|---|---|
| `extraction` | CLOSED — metric relation goes red |
| `recall_uncapped` | CLOSED — metric relation goes red |
| `citation_coverage` | CLOSED — metric relation goes red |
| `citation_correct` | CLOSED — metric relation goes red |
| `counterparty` | CLOSED — metric relation goes red |
| `dedup_cross_artifact` | CLOSED — metric relation goes red |
| `third_party_leaks` | CLOSED — metric relation goes red |
| `closed_leaks` | CLOSED — metric relation goes red |
| `dup_leaks` | CLOSED — metric relation goes red |
| `non_obligation_leaks` | CLOSED — metric relation goes red |
| `critical_identity` | CLOSED — metric relation goes red |
| `critical_direction` | CLOSED — metric relation goes red |
| `direction_scorable` | CLOSED — metric relation goes red |
| `direction` | CLOSED — metric relation goes red |
| `due_time` | CLOSED — metric relation goes red |
| `lifecycle` | CLOSED — metric relation goes red |
| `closure_linkage` | CLOSED — metric relation goes red |
| `loose_matches` | CLOSED — metric relation goes red |
| `unmatched` | CLOSED — metric relation goes red |

### Red-team manifest

Removing any registration makes `TestRedTeamManifestIsComplete` fail by name;
the registered mutant itself is then executed by `TestScorerRedTeam`.

| Row | Result |
|---|---|
| `a_synthetic_gibberish` | CLOSED — manifest/red-team gate goes red |
| `b_empty_brief` | CLOSED — manifest/red-team gate goes red |
| `c_every_question` | CLOSED — manifest/red-team gate goes red |
| `d_copy_the_input` | CLOSED — manifest/red-team gate goes red |
| `e_identity_flip` | CLOSED — manifest/red-team gate goes red |
| `f_direction_flip` | CLOSED — manifest/red-team gate goes red |
| `g_unsupported_citation` | CLOSED — manifest/red-team gate goes red |
| `h_constant_classifier` | CLOSED — manifest/red-team gate goes red |
| `i_daily_empty` | CLOSED — manifest/red-team gate goes red |
| `j_daily_citation` | CLOSED — manifest/red-team gate goes red |
| `k_oracle` (meeting and daily) | CLOSED — manifest/red-team gate goes red |
| `l_closed_as_open` | CLOSED — manifest/red-team gate goes red |
| `m_gold_owner_flip` | CLOSED — manifest/red-team gate goes red |
| `n_citation_span_move` | CLOSED — manifest/red-team gate goes red |
| `o_authored_to_quoted` | CLOSED — manifest/red-team gate goes red |
| `p_removed_source` (meeting and daily) | CLOSED — manifest/red-team gate goes red |
| `q_duplicate_noise` (meeting and daily) | CLOSED — manifest/red-team gate goes red |
| `r_input_order` (meeting and daily) | CLOSED — manifest/red-team gate goes red |
| `s_gate_disable_sweep` | CLOSED — manifest/red-team gate goes red |
| `t_graph_state_insensitive` | CLOSED — post/post disposable mutant makes `TestScorerRedTeam/.../t_graph_state_insensitive` go red |

### Current surfaces and correction flywheel

| Gate/mutation | Result |
|---|---|
| `cmdPulse` bypasses `briefClock` | CLOSED — `TestExamSurfaceClockGuard` goes red |
| CLI daily cap drifts from the pinned surface | CLOSED — `TestExamSurfaces` goes red |
| Post arm omits `merge confirm` governance write | CLOSED — `TestExamCorrectionFlywheel` goes red |
| `canonicalizePersons` RULE 3 is neutered | CLOSED — `TestExamCorrectionFlywheel` goes red |
| Pre arm is already merged/correct | CLOSED — `TestExamCorrectionFlywheel` goes red with `EVAL_BROKEN` |
| Scorer uses post graph output for both states | CLOSED — red-team row `t_graph_state_insensitive` goes red |
| Daily typed obligation lane | HOLE — issue #154, expires 2026-10-14 |
| iMessage commitment outside final turn | HOLE — issue #156, expires 2026-10-14 |

### Ratchet and exit branches

These branches land in exam PR 4, not this PR. They remain explicit rather
than being falsely marked covered by PR 3.

| Future branch | Result |
|---|---|
| Measured-floor ratchet comparison | HOLE — issue #139, expires 2026-07-21 |
| Integrity-exit audit-state branch | HOLE — issue #139, expires 2026-07-21 |
| Product-target comparison and `wantRED` must-flip arm | HOLE — issue #139, expires 2026-07-21 |
