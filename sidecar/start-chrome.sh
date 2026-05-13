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
nohup "${AGENT_BROWSER_EXECUTABLE_PATH:-/usr/local/bin/agent-browser-chrome}" \
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
