# clawson-tui: dedicated container for the Ink TUI.
# Network: none. Only access is the bind-mounted clawson.sock.
FROM docker.io/oven/bun:1.2-alpine
WORKDIR /app
COPY tui/package.json tui/bunfig.toml tui/tsconfig.json ./
# Install with frozen lockfile if present, otherwise resolve under bunfig minimumReleaseAge.
COPY tui/bun.lock* ./
RUN bun install --no-progress
COPY tui/src ./src
ENV SOCK_PATH=/sock
ENTRYPOINT ["bun","--hot","run","src/index.tsx"]
