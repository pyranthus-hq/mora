# Security policy

Mora reads email, calendars, messages, files, and OAuth credentials. Please do
not report a suspected vulnerability in a public issue, discussion, or pull
request.

## Report privately

Use [GitHub private vulnerability reporting](https://github.com/pyranthus-hq/mora/security/advisories/new).
Include the affected Mora version and platform, the security boundary you believe
is crossed, a minimal reproduction, and the impact. Redact tokens, message text,
email addresses, phone numbers, filesystem paths, and anything from a real vault.

If a reproduction needs data, use a synthetic vault and an isolated
`MORA_CONFIG_DIR`. Do not send a real `chat.db`, Calendar database, OAuth token,
vault archive, or unredacted `mora doctor --json` output.

## Supported versions

Security fixes target the latest release and `main`. Older releases may be asked
to upgrade before a report can be reproduced or fixed.

## Security boundaries

Reports are especially useful when they involve:

- a connector writing to a read-only source;
- unexpected network egress or telemetry;
- credential, vault, or source-content disclosure;
- path traversal or writes outside Mora-owned directories;
- bypass of identity, deletion, sharing, or source-scope controls;
- unsigned update or installer-integrity failures;
- MCP or loopback HTTP authorization bypass;
- corruption that silently presents stale data as current.

Product-quality mistakes such as a weak search result belong in the
[quality-report form](https://github.com/pyranthus-hq/mora/issues/new?template=quality.yml)
unless they cross a confidentiality, integrity, or authorization boundary.
