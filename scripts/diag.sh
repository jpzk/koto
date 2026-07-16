#!/bin/sh
# diag.sh — quick health snapshot of the guest microVM.
# Runs as node (uid 1000) in /workspace. POSIX sh only.
echo "=== identity ==="
id
echo "=== host / kernel ==="
uname -a
echo "=== os ==="
( . /etc/os-release 2>/dev/null && echo "$PRETTY_NAME" ) || echo "unknown"
echo "=== uptime / load ==="
uptime
echo "=== memory ==="
free -h 2>/dev/null || cat /proc/meminfo | head -3
echo "=== workspace disk ==="
df -h /workspace 2>/dev/null | tail -1
echo "=== egress probe ==="
curl -sS -m 5 -o /dev/null -w "https reachable: http=%{http_code}\n" https://example.com 2>&1 \
  || echo "no egress (network=none, or blocked)"
echo "=== done ==="
