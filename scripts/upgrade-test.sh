#!/usr/bin/env bash
# Upgrade test: a wamcp install (the name before the rename) running as a
# systemd user service, then `whatsupd service install`. Needs a systemd user
# session and a home without ~/.wamcp or ~/.whatsupd.
# Usage: scripts/upgrade-test.sh OLD_WAMCP_BINARY NEW_WHATSUPD_BINARY
set -euo pipefail

old_bin=$1 new_bin=$2
export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
fail() { echo "FAIL: $*" >&2; exit 1; }

[ -e ~/.wamcp ] || [ -e ~/.whatsupd ] && fail "~/.wamcp or ~/.whatsupd already exists; run this in a clean home"
install -d ~/.local/bin
install -m 0755 "$old_bin" ~/.local/bin/wamcp
install -m 0755 "$new_bin" ~/.local/bin/whatsupd
units=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user

echo "== old install"
~/.local/bin/wamcp service install --mcp-allow-send --mcp-send-to me
sleep 3
[ "$(systemctl --user is-active wamcp.service)" = active ] || fail "old service is not running"
token=$(cat ~/.wamcp/token)
mcp_token=$(cat ~/.wamcp/mcp-token)

echo "== upgrade"
~/.local/bin/whatsupd service install --mcp-allow-send --mcp-send-to me | tee /tmp/upgrade.out
grep -q "removed the old wamcp service" /tmp/upgrade.out || fail "old service not reported as removed"
sleep 3
[ ! -e "$units/wamcp.service" ] || fail "old unit file still there"
[ "$(systemctl --user is-active wamcp.service || true)" != active ] || fail "old service still running"
[ "$(systemctl --user is-active whatsupd.service)" = active ] || fail "new service is not running"
grep -q -- "--data-dir $HOME/.wamcp " "$units/whatsupd.service" || fail "new service does not use ~/.wamcp"
[ ! -e ~/.whatsupd ] || fail "~/.whatsupd was created"
[ "$(cat ~/.wamcp/token)" = "$token" ] || fail "access token changed"
[ "$(cat ~/.wamcp/mcp-token)" = "$mcp_token" ] || fail "MCP token changed"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:8765/mcp \
  -H "Authorization: Bearer $mcp_token" -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"upgrade-test","version":"1"}}}')
[ "$code" = 200 ] || fail "/mcp with the old MCP token: HTTP $code"

if timeout 10 ~/.local/bin/wamcp serve >/tmp/old-serve.out 2>&1; then fail "old binary started a second daemon"; fi
grep -q "already running" /tmp/old-serve.out || fail "old binary did not report the running daemon: $(cat /tmp/old-serve.out)"

~/.local/bin/whatsupd service uninstall
echo "upgrade test passed"
