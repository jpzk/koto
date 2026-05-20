# Sidecar image: hosts the claude-code node process per group.
# Switched from node:22-bookworm-slim to fedora:44 so we can run rootless
# podman-in-podman (opt-in per-group via config.json "pip": true). Debian
# bookworm's podman is 4.3.1, which can't initialize its graphroot inside
# the --userns=keep-id namespace (storage chown fails: only one uid is
# mapped). Fedora 44 ships podman 5.x which handles that correctly.
FROM fedora:44

# agent-browser pinned per 6-week dependency lag rule. 0.23.1 released
# 2026-03-30 (verified via registry.npmjs.org). Bump alongside Chrome —
# `agent-browser install` downloads a version-matched Chrome for Testing.
ARG AGENT_BROWSER_VERSION=0.23.1

# Agent tooling + Chrome runtime libs + rootless podman-in-podman support.
# shadow-utils provides newuidmap/newgidmap (needed by podman bootstrap
# when subuid is set). crun is the default runtime. slirp4netns +
# fuse-overlayfs are the rootless networking + overlay drivers.
RUN dnf install -y --setopt=install_weak_deps=False \
      bash \
      ca-certificates \
      curl \
      git \
      jq \
      openssh-clients \
      python3 \
      ripgrep \
      nodejs \
      npm \
      podman \
      fuse-overlayfs \
      slirp4netns \
      crun \
      shadow-utils \
      tar \
      gzip \
      which \
      alsa-lib \
      atk \
      at-spi2-atk \
      at-spi2-core \
      cairo \
      cups-libs \
      dbus-libs \
      expat \
      glib2 \
      libdrm \
      libX11 \
      libXcomposite \
      libXdamage \
      libXext \
      libXfixes \
      libXrandr \
      libxcb \
      libxkbcommon \
      mesa-libgbm \
      nspr \
      nss \
      pango \
      liberation-fonts \
 && dnf clean all \
 && npm i -g @anthropic-ai/claude-code "agent-browser@${AGENT_BROWSER_VERSION}" \
 && npm cache clean --force \
 && mkdir -p /opt/agent-browser \
 && HOME=/opt/agent-browser agent-browser install \
 && chmod -R a+rX /opt/agent-browser \
 && CHROME_BIN=$(ls /opt/agent-browser/.agent-browser/browsers/chrome-*/chrome | head -n1) \
 && ln -sf "$CHROME_BIN" /usr/local/bin/agent-browser-chrome

ENV AGENT_BROWSER_EXECUTABLE_PATH=/usr/local/bin/agent-browser-chrome

# Rootless podman-in-podman support (opt-in per-group via config.json "pip": true).
# - subuid/subgid: gives `node` a 50k-id range starting at 10000. The range
#   MUST fit within the sidecar's outer userns mapping (0..65535 under
#   --userns=keep-id), or newuidmap fails EPERM: the kernel can only map
#   source uids the writer already owns. The old 100000:65536 default
#   broke this — 100000 isn't in the sidecar's userns.
# - newuidmap/newgidmap need setuid: fedora ships them without (uses file
#   caps, but those don't survive image build the way setuid does). Setuid
#   to root-in-userns lets them write a child's uid_map with CAP_SETUID.
# - storage.conf: vfs driver avoids needing /dev/fuse passthrough on the outer
#   run; trade-off is slower image extraction and more disk. Flip to
#   "overlay" with fuse-overlayfs if/when we accept the /dev/fuse mount.
# - containers.conf: cgroupfs cgroup manager because no systemd is running
#   inside the sidecar; userns=host so inner containers don't try to nest
#   another user namespace (we already burn one level of nesting on bootstrap).
# - registries.conf: docker.io as the unqualified default; matches what most
#   `podman pull foo` invocations expect.
# Create node user (uid 1000) — fedora doesn't have one by default; uid 1000
# is required so --userns=keep-id maps host uid 1000 → container `node`.
RUN useradd -u 1000 -m -s /bin/bash node \
 && echo 'node:10000:50000' > /etc/subuid \
 && echo 'node:10000:50000' > /etc/subgid \
 && chmod u+s /usr/bin/newuidmap /usr/bin/newgidmap \
 && mkdir -p /etc/containers \
 && printf '[storage]\ndriver = "vfs"\nrunroot = "/tmp/containers"\ngraphroot = "/workspace/.local/share/containers/storage"\n\n[storage.options]\nignore_chown_errors = "true"\n' > /etc/containers/storage.conf \
 && printf '[containers]\nuserns = "host"\n\n[engine]\ncgroup_manager = "cgroupfs"\nevents_logger = "file"\n' > /etc/containers/containers.conf \
 && printf 'unqualified-search-registries = ["docker.io"]\n' > /etc/containers/registries.conf

COPY start-chrome.sh /usr/local/bin/start-chrome
COPY entrypoint.sh /e.sh
# The daemon bind-mounts entrypoint.sh and stream_filter.js into the
# container, overlaying the COPY'd /e.sh for hot-reload. The COPY remains
# so the image is runnable standalone (e.g. for `podman run -it
# --entrypoint sh clawson` smoke tests).

WORKDIR /workspace

USER node
ENTRYPOINT ["/bin/sh","/e.sh"]
