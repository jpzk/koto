# Sidecar image: hosts the claude-code node process per group.
# Debian bookworm-slim base picked over alpine for glibc compatibility
# (so apt-installed tools and any native node modules just work) and the
# wider apt ecosystem for agent-flexibility tooling. ~30MB larger than the
# alpine variant but the trade is worth it for the broader tool surface.
FROM node:22-bookworm-slim

# agent-browser pinned per 6-week dependency lag rule. 0.23.1 released
# 2026-03-30 (verified via registry.npmjs.org). Bump alongside Chrome —
# `agent-browser install` downloads a version-matched Chrome for Testing.
ARG AGENT_BROWSER_VERSION=0.23.1

# Agent tooling: claude-code's Bash tool can shell out to any of these.
# Kept lean — anything not on this list is a deliberate choice to leave
# out of the sandbox. The lib* packages are Chrome runtime deps (the
# rootless dpkg-deb workaround in groups/<g>/AGENT_BROWSER_SETUP.md
# isn't needed at build time — we're root here).
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash \
      ca-certificates \
      curl \
      git \
      jq \
      python3 \
      ripgrep \
      libglib2.0-0 \
      libnss3 \
      libnspr4 \
      libatk1.0-0 \
      libatk-bridge2.0-0 \
      libatspi2.0-0 \
      libcups2 \
      libxkbcommon0 \
      libxcomposite1 \
      libxdamage1 \
      libxfixes3 \
      libxrandr2 \
      libxext6 \
      libx11-6 \
      libxcb1 \
      libgbm1 \
      libpango-1.0-0 \
      libcairo2 \
      libasound2 \
      libdbus-1-3 \
      fonts-liberation \
 && rm -rf /var/lib/apt/lists/* \
 && npm i -g @anthropic-ai/claude-code "agent-browser@${AGENT_BROWSER_VERSION}" \
 && npm cache clean --force \
 && mkdir -p /opt/agent-browser \
 && HOME=/opt/agent-browser agent-browser install \
 && chmod -R a+rX /opt/agent-browser \
 && CHROME_BIN=$(ls /opt/agent-browser/.agent-browser/browsers/chrome-*/chrome | head -n1) \
 && ln -sf "$CHROME_BIN" /usr/local/bin/agent-browser-chrome

ENV AGENT_BROWSER_EXECUTABLE_PATH=/usr/local/bin/agent-browser-chrome

COPY start-chrome.sh /usr/local/bin/start-chrome
COPY entrypoint.sh /e.sh
# The daemon bind-mounts entrypoint.sh and stream_filter.js into the
# container, overlaying the COPY'd /e.sh for hot-reload. The COPY remains
# so the image is runnable standalone (e.g. for `podman run -it
# --entrypoint sh clawson` smoke tests).

WORKDIR /workspace

USER node
ENTRYPOINT ["/bin/sh","/e.sh"]
