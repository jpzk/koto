# Sidecar image: hosts the claude-code node process per group.
# Debian bookworm-slim base picked over alpine for glibc compatibility
# (so apt-installed tools and any native node modules just work) and the
# wider apt ecosystem for agent-flexibility tooling. ~30MB larger than the
# alpine variant but the trade is worth it for the broader tool surface.
FROM node:22-bookworm-slim

# Agent tooling: claude-code's Bash tool can shell out to any of these.
# Kept lean — anything not on this list is a deliberate choice to leave
# out of the sandbox. Add via apt-get install in a /workspace/.cs script
# if a specific group needs more.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash \
      ca-certificates \
      curl \
      git \
      jq \
      python3 \
      ripgrep \
 && rm -rf /var/lib/apt/lists/* \
 && npm i -g @anthropic-ai/claude-code \
 && npm cache clean --force

WORKDIR /workspace
COPY entrypoint.sh /e.sh
# nc.py also bind-mounts entrypoint.sh and stream_filter.js into the
# container, overlaying the COPY'd /e.sh for hot-reload. The COPY remains
# so the image is runnable standalone (e.g. for `podman run -it
# --entrypoint sh clawson` smoke tests).

USER node
ENTRYPOINT ["/bin/sh","/e.sh"]
