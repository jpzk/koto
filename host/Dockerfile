# cs_host: builds + runs the daemon + proxy from bind-mounted source.
#
# Runtime is the golang toolchain image (not a minimal alpine) so we can
# recompile on every cs_host start without an image rebuild. This trades
# image size (~300MB vs ~15MB) for development ergonomics: edit any .go
# file under the project root, `make host-run`, and the change is live.
# Only rebuild the image (`make host-build`) when changing this Dockerfile
# or the npm-installed claude-code version.
#
# run-host.sh bind-mounts the project root at its matching host path
# (`-v "$HERE:$HERE"`) and sets WORKDIR to that path, so `go run ./daemon`
# finds go.work + daemon/ + protocol/ without anything being COPYed in.
# cwd stays the project root — the daemon resolves its runtime dirs
# (groups/, creds/, fcassets/, run/) relative to cwd, not to its source.
FROM docker.io/library/golang:1.24-alpine
# e2fsprogs: mkfs.ext4 + e2fsck for per-group workspace.img (Firecracker
# runtime, fcEnsureWorkspaceImg). e2fsprogs-extra: resize2fs, used to grow an
# existing workspace.img when a group's size preset increases. tar: skills
# tarball delivery to microVM guests. No podman: groups are microVMs launched
# via the firecracker binary, and the DooD socket (the only thing that used the
# podman CLI here) has been removed — cs_host has no path to any podman daemon.
RUN apk add --no-cache nodejs npm e2fsprogs e2fsprogs-extra tar \
 && npm i -g @anthropic-ai/claude-code
# GOFLAGS explicitly cleared: the golang base image sets GOFLAGS=-mod=mod,
# which workspace mode (go.work at the project root) rejects outright
# ("-mod may only be set to readonly or vendor when in workspace mode").
# go run never needs to rewrite go.mod here anyway.
ENV GOFLAGS=""
# BIND is the proxy's listen address (daemonMain). NC_NETWORK/PROXY_HOST used
# to live here too — they named the podman bridge the sidecars sat on and the
# proxy host they dialed; both died with the podman group runtime and nothing
# has read them since.
ENV BIND=0.0.0.0
# Build-then-EXEC rather than `go run`: the daemon binary must be PID 1 so
# `podman stop`'s SIGTERM reaches its shutdown handler (which stops every
# microVM so guests sync+umount their workspace images). `go run` does NOT
# forward SIGTERM to its child — verified: the go tool dies, the daemon
# lingers orphaned until podman's SIGKILL, and every running workspace.img is
# left dirty. Same recompile-on-start ergonomics (same mounted build cache);
# the binary lands in /tmp, outside the source bind mount.
ENTRYPOINT ["sh","-c","go build -o /tmp/kotod ./daemon && exec /tmp/kotod daemon"]
