module clawson-fcagent

go 1.24

// x/sys pinned per the 6-week dependency-lag rule: v0.33.0 released
// 2025-05-02 (verified via proxy.golang.org/golang.org/x/sys/@v/v0.33.0.info).
require golang.org/x/sys v0.33.0
