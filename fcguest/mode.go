package main

import (
	"fmt"
	"strings"
)

// Per-session permission modes (host-authoritative). The mode arrives on
// every turn as MsgReq.mode, stamped by the daemon from its host-only session
// registry. It is NEVER read from config.json or anything else the agent can
// write: the only thing the guest does with it is pick the worker launch.
const (
	modeYolo    = "yolo"
	modePlan    = "plan"
	modeConfirm = "confirm"
)

// normTurnMode maps the wire value to a known mode. "" is a pre-modes daemon
// (or a yolo session, which the daemon leaves unset) and means yolo. Anything
// else unknown is refused rather than guessed: version skew must fail closed,
// never fall back to the unrestricted launch.
func normTurnMode(m string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "", modeYolo:
		return modeYolo, nil
	case modePlan:
		return modePlan, nil
	case modeConfirm:
		return modeConfirm, nil
	}
	return "", fmt.Errorf("unknown permission mode %q (host/guest version skew?) — refusing the turn", m)
}

// turnHarness names the worker runTurn will dispatch to for a provider. It
// mirrors runTurn's switch (default = claude).
func turnHarness(provider string) string {
	switch provider {
	case "venice":
		return "venice"
	case "openai":
		return "codex"
	case "minimax":
		return "pi"
	}
	return "claude"
}

// harnessModeErr: only the claude harness implements plan/confirm in v1.
// codex, pi and venice refuse a restricted turn outright — a silent yolo
// launch would run tools the operator asked to gate. The provider comes from
// the guest-writable config.json, so an agent that rewrites it to escape a
// restricted mode lands here and gets a refusal, not a yolo turn.
func harnessModeErr(provider, mode string) error {
	if mode == modeYolo {
		return nil
	}
	if h := turnHarness(provider); h != "claude" {
		return fmt.Errorf("permission mode %s is not supported for this harness yet (%s) — refusing the turn instead of running it unrestricted", mode, h)
	}
	return nil
}

// claudeIsolation keeps agent-written settings out of a restricted launch.
// The bypass spike (goal session-modes-claude, spike/RESULT.md C1) showed
// --bare alone still loads /workspace/.claude/settings*.json and .claude.json,
// whose allow rules pre-approve tools. An empty --setting-sources loads none of
// them; --strict-mcp-config ignores agent-written MCP configs.
var claudeIsolation = []string{"--setting-sources=", "--strict-mcp-config"}

// claudePlanTools is the read-only tool set for plan mode. Under --bare the
// CLI (2.1.280) exposes only Bash/Edit/Read, so Read is the whole list;
// ExitPlanMode is not in the set and is additionally disallowed so that a CLI
// that does ship it can never use it to leave plan mode.
const claudePlanTools = "Read"

// claudeBaseArgs is the launch prefix for a mode. yolo is byte-identical to
// the pre-modes launch. confirm is wired in by the approval round trip; until
// a guest implements it the turn is refused (fail closed).
func claudeBaseArgs(mode string) ([]string, error) {
	out := []string{"--output-format", "stream-json", "--include-partial-messages", "--verbose"}
	switch mode {
	case modeYolo:
		return append([]string{"-p", "--bare", "--dangerously-skip-permissions"}, out...), nil
	case modePlan:
		a := []string{"-p", "--bare"}
		a = append(a, claudeIsolation...)
		a = append(a, "--permission-mode", "plan", "--permission-prompts", "none",
			"--tools", claudePlanTools, "--disallowedTools", "ExitPlanMode")
		return append(a, out...), nil
	}
	return nil, fmt.Errorf("permission mode %s is not implemented by this guest — refusing the turn", mode)
}
