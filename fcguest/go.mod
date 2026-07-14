module clawson-fcagent

go 1.24.0

// x/sys pinned per the 6-week dependency-lag rule: v0.40.0 released
// 2025-12-19 (verified via proxy.golang.org/golang.org/x/sys/@v/v0.40.0.info).
// v0.40.0 is the workspace-wide selection (`go work sync` — daemon and tui
// already pin it), keeping module-mode and workspace-mode builds identical.
require golang.org/x/sys v0.40.0
