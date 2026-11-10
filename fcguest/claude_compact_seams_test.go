package main

import (
	"testing"
	"time"
)

func setClaudeProjectsDir(t *testing.T, d string) {
	prev := claudeProjectsDir
	claudeProjectsDir = d
	t.Cleanup(func() { claudeProjectsDir = prev })
}

func setClaudeCompactPersistTimeout(t *testing.T, d time.Duration) {
	prev := claudeCompactPersistTimeout
	claudeCompactPersistTimeout = d
	t.Cleanup(func() { claudeCompactPersistTimeout = prev })
}
