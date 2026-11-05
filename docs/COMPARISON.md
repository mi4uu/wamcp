# whatsupd compared with other WhatsApp MCP servers

As of 2026-11-05. Stars, last push and license come from the GitHub API on that date. Everything else comes from reading each project's source or README at that date; cells taken only from a README say so. Projects change fast, so check the links before you rely on a cell.

## The short version

- **lharries/whatsapp-mcp** is the one everyone knows (6.8k stars). It works, it can send text, files and voice notes, and it has not had a commit since 2025-07-13. Its Go bridge serves an unauthenticated REST API on all interfaces, sends are on with no allowlist, there are no tests.
- **delltrak/wamcp** is the most feature-rich: 169 tools, Baileys or the official Cloud API, OAuth 2.1, Postgres, a real test suite and CI. It needs Node 24, Redis and Docker, sends by default, keeps messages 30 days by default, ships no LICENSE file, and has the name this project had until 2026-11-06.
- **finereli/whatsapp-mcp** and **felipeadeildo/whatsapp-mcp** run on Cloudflare Workers with OAuth, so they plug into claude.ai with one click. Your messages then live in a Cloudflare account (yours or, for finereli's hosted beta, the operator's).
- **whatsupd** is one Go binary that keeps your whole WhatsApp history on your own disk with full-text search, is read-only by default and can send only when you switch it on, with an allowlist, a rate limit, retry de-duplication and an audit log. Remote clients sign in with an MCP-only token or, opt-in, OAuth 2.1. Nobody has starred it yet.

## Table

| | whatsupd (this repo) | [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) | [delltrak/wamcp](https://github.com/delltrak/wamcp) |
|---|---|---|---|
| Stars / last push | new / this repo | 6814 / 2025-07-13 | 27 / 2026-11-02 |
| License | AGPL-3.0 | MIT | no LICENSE file (`package.json` says MIT) |
| Stack | Go, whatsmeow, SQLite | Go bridge (whatsmeow) + Python MCP server | TypeScript, Baileys or WhatsApp Cloud API, BullMQ |
| To install | one static binary or a .deb, .rpm or Arch package; or build with Go 1.26 | Go, Python 3.11+, uv; cgo on Windows | Node 24+, Redis; Docker Compose recommended |
| MCP tools | 13 read tools, plus `send_message` and `send_file` when switched on | 12 (3 send) | 169, plus 3 opt-in |
| Transports | stdio, streamable HTTP | stdio | stdio, streamable HTTP |
| Auth on HTTP | token always on (MCP-only token or main token); OAuth 2.1 with PKCE opt-in | no MCP HTTP; bridge REST on `:8080` has no auth | none unless configured; API key, Basic or OAuth 2.1 |
| Sends by default | no | yes | yes |
| Send safeguards | opt-in switch, allowlist, hourly limit, idempotency, audit table | none | queue rate limit per channel, group writes gated off |
| Message history | everything, kept forever, deletes and edits kept | stored in SQLite | 30 days by default (`WA_MESSAGE_RETENTION_DAYS`) |
| Search | SQLite FTS5, accent-insensitive | SQL `LIKE` | full-text search tool |
| Crash safety | messages acked to WhatsApp only after commit | not addressed | inbound de-dup by message ID |
| Tests / CI | Go tests for archive, ingest, MCP, HTTP, send policy; CI workflow | none | 60+ test files, CI with Postgres job |

Other projects with 40+ stars, facts from the GitHub API and their READMEs:

| Project | Stars / last push | License | Runs as | Sends | HTTP auth |
|---|---|---|---|---|---|
| [finereli/whatsapp-mcp](https://github.com/finereli/whatsapp-mcp) | 404 / 2026-10-24 | MIT | Cloudflare Worker, hosted beta or self-host | no, read-only by design | OAuth (Google sign-in) |
| [verygoodplugins/whatsapp-mcp](https://github.com/verygoodplugins/whatsapp-mcp) | 283 / 2026-10-30 | not detected by GitHub | Go bridge + Python, continues lharries | yes, by default | MCP HTTP: none, loopback default; bridge: bearer |
| [felipeadeildo/whatsapp-mcp](https://github.com/felipeadeildo/whatsapp-mcp) | 98 / 2026-11-03 | GPL-3.0 | Cloudflare Worker | yes, by default | OAuth or bearer |
| [jlucaso1/whatsapp-mcp-ts](https://github.com/jlucaso1/whatsapp-mcp-ts) | 75 / 2026-01-23 | none | Node 23, Baileys | yes, by default | stdio only |
| [BrOrlandi/whatsapp-mcp-local](https://github.com/BrOrlandi/whatsapp-mcp-local) | 70 / 2026-11-03 | MIT | desktop app | yes, by default | localhost, no auth (README) |
| [BrOrlandi/whatsapp-mcp](https://github.com/BrOrlandi/whatsapp-mcp) | 64 / 2026-10-09 | MIT | Docker Compose, Evolution Go | yes, by default | setup token (README) |
| [pnizer/wweb-mcp](https://github.com/pnizer/wweb-mcp) | 46 / 2025-07-18 | MIT | Node + headless Chrome (whatsapp-web.js) | yes, by default | optional API key (README) |
| [FelixIsaac/whatsapp-mcp-extended](https://github.com/FelixIsaac/whatsapp-mcp-extended) | 44 / 2026-11-02 | MIT | Go bridge + Python, continues lharries | yes, optional allowlist | stdio |

## lharries/whatsapp-mcp in detail

- **Tools** (`whatsapp-mcp-server/main.py`): `search_contacts`, `list_messages`, `list_chats`, `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats`, `get_last_interaction`, `get_message_context`, `send_message`, `send_file`, `send_audio_message` (converts with ffmpeg), `download_media`.
- **Architecture**: the Go bridge stores messages in `store/messages.db` and the session in `store/whatsapp.db`. The Python server reads SQLite directly and calls the bridge at `http://localhost:8080/api/send` and `/api/download`. The bridge listens with `http.ListenAndServe(":8080", nil)`, which is every interface, with no auth on those handlers. Open issue #215 reports exactly this.
- **Security**: no allowlist, confirmation, rate limit or audit. The README warns about the "lethal trifecta" (private data, untrusted input, ability to send). Open issues #388 (read-only mode) and #394 (loopback bind, token, read-only) ask for what whatsupd already does.
- **Reliability**: whatsmeow handles reconnects and history sync. Sends are not de-duplicated. No tests, no CI.
- **Maintenance**: 96 open issues, 173 open PRs, issue #220 asks for new maintainers. Several forks carry it on (verygoodplugins, FelixIsaac).

## delltrak/wamcp in detail

- **Tools**: 169 registered in `src/tools/*.ts` (172 with `WA_INSTANCE_WEBHOOK_TOOLS_ENABLED`), 10 resources. Messaging (35, every media type, polls, buttons, albums), group admin (28), business and catalog (23), communities (16), instances, contacts, labels, status, scheduling, profile, newsletters, TTS, transcription.
- **Architecture**: a channel adapter picks Baileys or the WhatsApp Cloud API per instance, several instances at once. BullMQ on Redis queues outbound sends, scheduled sends and history fan-out. Drizzle ORM on SQLite or Postgres. A maintenance job deletes messages older than `WA_MESSAGE_RETENTION_DAYS` (30 by default).
- **Security**: `src/server/mcp-auth.ts` supports a bearer API key with constant-time compare, Basic auth with scrypt, and an OAuth 2.1 server with dynamic client registration. With none configured, HTTP is open ("the historical open behavior is preserved"). Group, community and app-state writes are off by default; plain sends are always available. No send allowlist or audit table found.
- **Reliability**: reconnects instances on boot, queue retries (3 attempts), inbound de-dup by `(instanceId, messageId)`.
- **Maintenance**: 362 commits by one author since 2026-02-28, Dependabot, CI on Node 24 and 26 with lint, typecheck, tests, `npm audit` and a Postgres job.
- **Name**: this project was called `wamcp` until 2026-11-06 and was renamed to whatsupd so the two are not mixed up. Old links to `mi4uu/wamcp` redirect here.

## Review of whatsupd: gaps, ranked

Fixed since the first public version:

1. **Web auto sign-in through a tunnel (high).** With `--web-auth local` on a loopback listener, a tunnel such as cloudflared running as the same OS user connects over loopback from a same-user process, so `sameUserPeer` signed in every visitor of the public hostname. A tunnel that rewrites the Host header to `localhost` (the usual workaround for the rebinding check) made this reachable. Now requests with proxy headers are never auto signed in, and `--public-host` forces token sign-in.
2. **No supported way to run behind a tunnel (medium).** The Host check rejected the public name, so remote clients needed a non-loopback `--listen`. Added `--public-host`.
3. **No opt-in send (feature gap vs everyone else).** Added `send_message` behind `--mcp-allow-send` with allowlist, rate limit, idempotency and audit.
4. **Untrusted text not flagged to agents (low).** The MCP instructions now say message text is data, not instructions.
5. **No CI (low).** Added a workflow: gofmt, vet, staticcheck, race tests on Linux and macOS.
6. **One token did everything (medium).** The MCP token also signed in to the web client, which can send without the policy. Added a separate MCP-only token (`whatsupd token --mcp`, `--rotate-mcp`); `whatsupd config` prints it.
7. **No OAuth (high for remote use).** Clients that only speak OAuth could not connect. Added an opt-in OAuth 2.1 server (`--oauth`): metadata discovery, dynamic client registration, authorization code with S256 PKCE, rotating refresh tokens, owner approval with the main token, `whatsupd oauth list|revoke`.
8. **Background service was macOS only (medium).** `whatsupd service` now writes a systemd user unit on Linux, tested on Debian trixie.
9. **No MCP media sending (medium).** Added `send_file`: photos, videos, audio, voice notes and documents under the same policy. Paths are refused unless `--mcp-send-files-from` names a directory, and never leave it.
10. **No prebuilt binaries (medium).** Installing needed Go 1.26. Each release now has static binaries for macOS and Linux, .deb, .rpm and Arch packages, and SHA256SUMS; CI installs every package.
11. **`internal/wa` had no tests (medium).** Tests now cover the ack-after-commit path, redelivery, catch-up, auto-media, state events and media paths. Writing them found two bugs, both fixed: a redelivered message counted as unread twice, and a chat ID of ".." could put media outside the media directory.

Still open, in order of impact:

1. **Fewer tools than delltrak.** 15 versus 169. Most of the gap is group admin, business and scheduling, which a personal archive does not need, but it is a real difference.
