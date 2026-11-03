# Contract receipt

Emit an offline `mora.pilotreplay.receipt` after schema tests:

- `schema_version` (must be `1`)  
- `example_hashes` — SHA-256 of canonical JSON for synthetic positives/negatives  
- `tests_passed`  
- `unresolved_limits` — e.g. case qualification (#542), runner (#543+), freeze-before-exec  
- `claims_efficacy` must be **false**  

Helper: `BuildContractReceipt(generatedAt, testsPassed)` in the Go package.

Do **not** claim that schema tests show memory edits improve coding outcomes,
that a real case is qualified, or that spend is authorized.
