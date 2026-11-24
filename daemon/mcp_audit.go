package main

// MCP events on the hash-chained audit spool (docs/audit-events.md):
//
//   - mcp_call: one record per gateway decision (tools/list, tools/call,
//     not-whitelisted probes). Group is the group whose vsock 9005
//     listener carried the request, as the daemon resolved it. Recorded
//     are the connection, method, tool, the gateway-authored Code as the
//     reason, and the duration. Never arguments, results, credentials, or
//     the free-text Reason (it can quote upstream bodies or argument
//     values); that stays in the group log.
//   - mcp_admin: one record per operator registry op (admin gRPC Mcp,
//     `koto ctl mcp`), allowed or refused. Group is the daemon ("koto",
//     args.via=admin). Only op/conn/group/tool names are recorded; the URL,
//     auth header/value, OAuth config and rules never are, and a refusal's
//     error text is kept as reason.len/.sha256 only.
//
// Names a guest controls (the tool name in tools/call, the conn path
// segment) are recorded by value only when they pass the registry's name
// syntax; anything else becomes <key>.len/.sha256.

import (
	"strconv"

	"koto/mcpgw"
	"koto/mcpreg"
)

// mcpAuditName records a guest-supplied name by value only if valid.
func mcpAuditName(args map[string]string, key, v string, ok func(string) bool) {
	switch {
	case v == "":
	case ok(v):
		args[key] = v
	default:
		auditBody(args, key, v)
	}
}

func mcpAuditCall(e mcpgw.AuditEntry) {
	ev := auditEvent{Verb: "mcp_call", Group: e.Group, Outcome: AuditRefused, Reason: e.Code,
		Args: map[string]string{"method": e.Method, "dur_ms": strconv.FormatInt(e.Dur.Milliseconds(), 10)}}
	switch {
	case e.Allowed:
		ev.Outcome = AuditAllowed
	case e.Code == mcpgw.CodeUpstream:
		// An upstream failure is an error, not a policy refusal.
		ev.Outcome = AuditError
	}
	mcpAuditName(ev.Args, "conn", e.Conn, mcpreg.ValidName)
	mcpAuditName(ev.Args, "tool", e.Tool, mcpreg.ValidTool)
	audit(ev)
}

// mcpAdmin runs one registry op and audits it (mcp_admin).
func mcpAdmin(r mcpAdminReq) (map[string]any, error) {
	view, err := mcpAdminRun(r)
	op := r.Op
	if op == "" {
		op = "ls"
	}
	ev := auditEvent{Verb: "mcp_admin", Group: auditDaemonGroup, Outcome: AuditAllowed,
		Args: map[string]string{"via": "admin", "op": auditScalar(mcpSafe(op))}}
	mcpAuditName(ev.Args, "conn", r.Conn, mcpreg.ValidName)
	mcpAuditName(ev.Args, "group", r.Group, mcpreg.ValidGroup)
	mcpAuditName(ev.Args, "tool", r.Tool, mcpreg.ValidTool)
	if err != nil {
		ev.Outcome, ev.Reason = AuditRefused, "refused"
		auditBody(ev.Args, "error", err.Error())
	}
	audit(ev)
	return view, err
}
