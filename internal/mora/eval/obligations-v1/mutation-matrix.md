# Obligation exam mutation matrix — 2026-07-20

Command: `scripts/eval/exam-mutation-matrix.sh`

Re-anchored 2026-07-28 (#205, PR #208): the Gate 3 surface rework (#196) moved
render authority to the materialized commitment inventory, so 15 rows' named
witnesses decayed — their planted mutants survived the named test while every
protection remained live. A broad-witness sweep proved all 23 mutants still die
(zero real holes); each decayed row was then re-probed and re-anchored to the
first named test that kills it, mostly `TestExamIntegrityExit` (the Gate 1
trust-leg audit, which pins product scores across all three corpora). The
2026-07-20 run below records the pre-#196 witnesses.

Re-run 2026-07-20 after the auditor-facing leak fix (neutral subjects, date
interleave, in-world supersession evidence): all 23 planted mutants KILLED and
all audit groups CLOSED against the re-cut corpus. The fix changed no mutant and
no gate; it only removed gold-label tells from auditor-visible fields.

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
| `classifyMeetingBriefEvidence` | CLOSED — `TestExamRealPredictionsPin` goes red |
| `isMeetingNotification` | CLOSED — `TestExamIntegrityExit` (Gate 1 trust legs) goes red |
| `assignedToThirdParty` | CLOSED — `TestExamIntegrityExit` (Gate 1 trust legs) goes red |
| `memoryIsServiceOnly` | CLOSED — `TestExamServiceOnlyGateIsAssembled` goes red |
| `userOwnedOpenLoop` | CLOSED — `TestExamRealPredictionsPin` goes red |
| `meetingBriefIsTwoPartyExchange` | CLOSED — `TestExamIntegrityExit` (Gate 1 trust legs) goes red |
| `relationalEvidenceIDs` | CLOSED — `TestExamIntegrityExit` (Gate 1 trust legs) goes red |
| `meetingBriefResolveAttribution` | CLOSED — `TestExamIntegrityExit` (Gate 1 trust legs) goes red |
| `stripURLs` | CLOSED — `TestExamIntegrityExit` (Gate 1 trust legs) goes red |
| `unwrapHardWraps` | CLOSED — `TestExamHardWrapJoinsBeforeSegmenting` goes red |
| `senderAuthoredBody` | CLOSED — `TestExamIntegrityExit` (Gate 1 trust legs) goes red |
| `stripSpeakerPrefix` | CLOSED — `TestExamIntegrityExit` (Gate 1 trust legs) goes red |
| `isForwardedSubject` | CLOSED — `TestExamIntegrityExit` (Gate 1 trust legs) goes red |
| `isLeadInFragment` | CLOSED — `TestExamIntegrityExit` (Gate 1 trust legs) goes red |
| `stripNoiseTokens` | CLOSED — `TestExamCorrectionFlywheel` goes red |
| `gmailActionableAsk` | CLOSED — `TestExamIntegrityExit` (Gate 1 trust legs) goes red |
| `containsPhrase` | CLOSED — `TestExamIntegrityExit` (Gate 1 trust legs) goes red |

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
| CLI daily cap drifts from the pinned surface | CLOSED — `TestExamIntegrityExit` (Gate 1 trust legs) goes red |
| Post arm omits `merge confirm` governance write | CLOSED — `TestExamCorrectionFlywheel` goes red |
| `canonicalizePersons` RULE 3 is neutered | CLOSED — `TestExamCorrectionFlywheel` goes red |
| Pre arm is already merged/correct | CLOSED — `TestExamCorrectionFlywheel` goes red with `EVAL_BROKEN` |
| Scorer uses post graph output for both states | CLOSED — red-team row `t_graph_state_insensitive` goes red |
| Daily typed obligation lane | HOLE — issue #154, expires 2026-10-14 |
| iMessage commitment outside final turn | HOLE — issue #156, expires 2026-10-14 |

### Ratchet and exit branches

These branches landed with exam PR 4 (#139, closed 2026-07-24). The dated
holes below are closed by named gates on main.

| Branch | Result |
|---|---|
| Measured-floor ratchet comparison | CLOSED — `assertGate3MeetingRatchet`/`assertGate3DailyRatchet` (`exam_surfaces_test.go`) go red on any floor regression |
| Integrity-exit audit-state branch | CLOSED — `TestExamIntegrityExit` goes red |
| Product-target comparison and `wantRED` must-flip arm | RETIRED — the strict target went green on 2026-07-24 (#204); `TestExamProductTarget` pins `wantRED = false` and fails if a dated pin is re-introduced without a reviewed decision |
