#!/bin/sh
# start-chrome: launch headless Chrome with CDP on :PORT (default 9222).
# Idempotent — if Chrome is already on PORT, exits 0 immediately.
# Designed for the sidecar container (no X, no GPU, --no-sandbox required
# since chrome's sandbox needs root/user-ns caps we don't have).
set -e
PORT=${1:-9222}
if curl -sf "http://localhost:${PORT}/json/version" >/dev/null 2>&1; then
  echo "chrome already listening on :${PORT}"
  exit 0
fi
PROFILE=/tmp/cdp-prof-${PORT}
mkdir -p "$PROFILE"
# setsid (not nohup): Chrome is meant to persist across turns, so it must leave
# the turn's session/process group. The per-turn `timeout -s KILL` watchdog in
# entrypoint.sh group-kills the turn's process group on a hang; nohup only
# ignores SIGHUP and stays in that group, so a single turn timeout would
# silently kill a persistent Chrome. setsid puts it in its own session, out of
# reach of any turn-scoped group/session kill. This is the detach contract:
# anything meant to outlive a turn detaches; everything else is turn-scoped.
setsid "${AGENT_BROWSER_EXECUTABLE_PATH:-/usr/local/bin/agent-browser-chrome}" \
  --headless=new --no-sandbox --disable-setuid-sandbox \
  --disable-dev-shm-usage --disable-gpu \
  --remote-debugging-port="${PORT}" \
  --user-data-dir="$PROFILE" \
  about:blank > "/tmp/chrome-${PORT}.log" 2>&1 &
for _ in $(seq 1 50); do
  if curl -sf "http://localhost:${PORT}/json/version" >/dev/null 2>&1; then
    echo "chrome up on :${PORT}"
    exit 0
  fi
  sleep 0.2
done
echo "chrome failed to start; tail /tmp/chrome-${PORT}.log" >&2
exit 1
