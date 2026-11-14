# Implementation Plan: Governed friction feedback pilot

**Input**: [Release scope](../mora-0.18.md) and the existing single-report
pilot evidence. Use the [project isolation](../006-project-setup/spec.md)
case as the control pattern.

## Delivery order

1. Start by checking the existing import and its eligibility. Prepare
   a compact, redacted report in a disposable vault for any write or
   repeat-import test. The active agent reviews scope, recovery evidence,
   and redaction before submitting through Mora policy. If a proposal is
   held, only the configured owner review can approve it.
2. Reuse Mora's configured write policy and scoped sharing. No new
   importer is required to run DR1. Prove direct read-back and fresh
   explicit retrieval separately, including devbox and wrong-project
   controls during refresh. If the existing authorized share excludes the
   report, record the gap and stop the devbox check.
3. Record whether the next agent took the verified recovery step. Keep
   experimental measurement in the existing experiment harness. Revisit bulk import only
   after recall and scope pass and the owner approves expansion.
