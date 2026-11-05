# Security

wamcp holds a copy of your WhatsApp account and, with `--mcp-allow-send`, can send from it. Security reports matter.

## Reporting

Report privately: [open a security advisory](https://github.com/mi4uu/wamcp/security/advisories/new). Do not open a public issue. Include the version (`wamcp version`), how wamcp runs, and the steps.

## Supported versions

Only the latest release gets fixes.

## Scope

In scope: anything that lets someone other than you read the archive, send messages, sign in to the web client, get an MCP or OAuth token, or bypass the send policy (allowlist, rate limit, idempotency, audit). The security model is in the README under "Data and security".

Out of scope: an attacker who already runs code as your OS user, and WhatsApp's own protocol.
