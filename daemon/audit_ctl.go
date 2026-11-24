package main

import (
	"encoding/json"
	"sort"
	"strings"
)

// ctl-plane audit (docs/audit-events.md). ctlDispatch wraps the verb
// switch (ctlDispatchInner) so EVERY ctl call — every verb, allowed or
// refused, including unknown verbs and malformed envelopes — produces
// exactly one audit record. Doing it at the single choke point instead of
// per case means a new verb cannot ship un-audited.

// auditCtlScalar are request fields recorded verbatim (bounded, redacted,
// sanitized): names, ids, schedule specs, knobs. Everything else is either
// a body (auditCtlBody: length + sha256 only) or recorded by KEY NAME only.
var auditCtlScalar = map[string]bool{
	"group": true, "session": true, "from_session": true, "name": true,
	"id": true, "cron": true, "enabled": true, "severity": true,
	"model": true, "effort": true, "provider": true, "plan": true,
	"max_iterations": true, "reply": true, "op": true, "n": true,
	"accepted": true, "status": true, "rc": true, "job": true,
	"conn": true,
}

// auditCtlBody are guest/agent-authored free text. NEVER raw.
var auditCtlBody = map[string]bool{
	"msg": true, "text": true, "criteria": true, "note": true,
	"title": true, "question": true, "prompt": true, "reason": true,
	"summary": true, "output": true, "tail": true, "answer": true,
	"instructions": true,
}

// ctlDispatch serves one ctl request from owner and audits it. The steer
// verbs go through steerAudit (coord_steer.go), which adds who/what was
// steered to the same record and writes it — still one record per call.
func ctlDispatch(owner string, line []byte) any {
	resp := ctlDispatchInner(owner, line)
	ev := auditCtlEvent(owner, line, resp)
	if ev.Verb == "steer" || ev.Verb == "goal_steer" {
		steerAudit(ev, line, resp)
		return resp
	}
	audit(ev)
	return resp
}

// auditCtlEvent derives the audit record for one ctl call from the raw
// request and the response the daemon chose. Pure: unit-testable.
func auditCtlEvent(owner string, line []byte, resp any) auditEvent {
	ev := auditEvent{Group: owner, Target: owner, Args: map[string]string{}}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		ev.Verb = "malformed"
		auditBody(ev.Args, "request", string(line))
	} else {
		var verb string
		_ = json.Unmarshal(fields["cmd"], &verb)
		ev.Verb = auditVerbName(verb)
		if ev.Verb != verb {
			ev.Args["cmd"] = auditScalar(verb)
		}
		auditCtlArgs(ev.Args, fields)
		var sess string
		if json.Unmarshal(fields["session"], &sess) == nil {
			ev.Session = auditScalar(sess)
		}
		var g string
		if json.Unmarshal(fields["group"], &g) == nil && g != "" {
			if owner == ctlMainGroup {
				ev.Target = auditScalar(g)
			} else if g != owner {
				// Non-main callers are always self-targeted: the daemon
				// forces/ignores the field. Record the ATTEMPT.
				ev.Args["group_forced"] = "true"
			}
		}
		if verb == "config_set" {
			var req configReq
			if json.Unmarshal(line, &req) == nil {
				if k := configPostureKey(req); k != "" {
					ev.Args["posture_key"] = k
				}
			}
		}
	}
	ev.Outcome, ev.Reason = auditCtlOutcome(resp)
	return ev
}

// auditVerbName bounds the verb to a safe event-name suffix: a guest picks
// the cmd string, and it lands in event.name.
func auditVerbName(v string) string {
	if v == "" {
		return "unknown"
	}
	if len(v) > 32 {
		return "unknown"
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return "unknown"
		}
	}
	return v
}

func auditCtlArgs(args map[string]string, fields map[string]json.RawMessage) {
	var other []string
	for k, raw := range fields {
		switch {
		case k == "cmd":
		case auditCtlBody[k]:
			var s string
			if json.Unmarshal(raw, &s) != nil {
				s = string(raw)
			}
			auditBody(args, k, s)
		case auditCtlScalar[k]:
			var s string
			if json.Unmarshal(raw, &s) != nil {
				s = string(raw) // number / bool / null: the JSON literal
			}
			args[k] = auditScalar(s)
		default:
			other = append(other, auditScalar(k))
		}
	}
	if len(other) > 0 {
		sort.Strings(other)
		args["keys"] = truncateRunes(strings.Join(other, ","), auditValueMax)
	}
}

// auditCtlOutcome classifies a response. Any error response is a refusal
// (the daemon declined to do it) unless it is a malformed request or an
// internal failure, which is "error".
func auditCtlOutcome(resp any) (string, string) {
	b, err := json.Marshal(resp)
	if err != nil {
		return AuditError, "unencodable response"
	}
	var r struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(b, &r)
	if r.Error == "" {
		return AuditAllowed, ""
	}
	if strings.HasPrefix(r.Error, "json:") || strings.Contains(r.Error, "internal") {
		return AuditError, r.Error
	}
	return AuditRefused, r.Error
}

// auditLifecycle records a daemon-originated group VM event.
func auditLifecycle(verb, g string, err error) {
	ev := auditEvent{Verb: verb, Group: auditDaemonGroup, Target: g, Outcome: AuditAllowed}
	if err != nil {
		ev.Outcome, ev.Reason = AuditError, err.Error()
	}
	audit(ev)
}
