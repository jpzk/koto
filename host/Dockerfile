# cs_host: runs the daemon + proxy via `go run` over bind-mounted source.
#
# Runtime is the golang toolchain image (not a minimal alpine) so we can
# recompile on every cs_host start without an image rebuild. This trades
# image size (~300MB vs ~15MB) for development ergonomics: edit any .go
# file under the project root, `make host-run`, and the change is live.
# Only rebuild the image (`make host-build`) when changing this Dockerfile
# or the npm-installed claude-code version.
#
# run-host.sh bind-mounts the project root at its matching host path
# (`-v "$HERE:$HERE"`) and sets WORKDIR to that path, so `go run .` finds
# go.mod + sources + ./protocol/ without anything being COPYed in.
FROM docker.io/library/golang:1.24-alpine
# e2fsprogs: mkfs.ext4 + e2fsck for per-group workspace.img (Firecracker
# runtime, fcEnsureWorkspaceImg). e2fsprogs-extra: resize2fs, used to grow an
# existing workspace.img when a group's size preset increases. tar: skills
# tarball delivery to microVM guests. No podman: groups are microVMs launched
# via the firecracker binary, and the DooD socket (the only thing that used the
# podman CLI here) has been removed — cs_host has no path to any podman daemon.
RUN apk add --no-cache nodejs npm e2fsprogs e2fsprogs-extra tar \
 && npm i -g @anthropic-ai/claude-code
ENV BIND=0.0.0.0 NC_NETWORK=clawson-net PROXY_HOST=cs_host_go \
    GOFLAGS=-mod=mod
ENTRYPOINT ["go","run",".","daemon"]
