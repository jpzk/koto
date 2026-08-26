package main

// ctl.go — the guest half of the control plane (vsock 9002).
//
// The shell tools (cs-job, cs-notify, cs-subagent) and the model itself keep
// writing one JSON line per command into /workspace/.cs/ctl and reading the
// JSON reply appended to /workspace/.cs/ctl.out — that interface is
// documented in prompts/global.md and unchanged. What changed is what
// crosses vsock: this file turns the line into a protocol/guest.proto
// CtlRequest with STRICT protojson (unknown verb, unknown field, wrong type
// → refused right here with an {"ok":false} line, never sent), frames it,
// and turns the typed CtlResponse back into the flat JSON object the tools
// expect ({"ok":true,"port":..} rather than {"ok":true,"result":{...}}).

import (
	"encoding/json"
	"fmt"

	"koto-protocol/pb"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ctlRequestFromJSON: {"cmd":"<verb>", ...fields} → CtlRequest. The verb
// becomes the oneof field name, the remaining keys its message. bytes fields
// (job output, notify title/msg, report/goal bodies) are base64 in JSON — the
// encoding the tools already emit. Mirrored by daemon/ctlpb.go's copy, which
// pins it under test.
func ctlRequestFromJSON(line []byte) (*pb.CtlRequest, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, err
	}
	var verb string
	if err := json.Unmarshal(m["cmd"], &verb); err != nil || verb == "" {
		return nil, fmt.Errorf("ctl: missing cmd")
	}
	delete(m, "cmd")
	inner, _ := json.Marshal(m)
	env, _ := json.Marshal(map[string]json.RawMessage{verb: inner})
	req := &pb.CtlRequest{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(env, req); err != nil {
		return nil, fmt.Errorf("ctl: %v", err)
	}
	if req.ProtoReflect().WhichOneof(req.ProtoReflect().Descriptor().Oneofs().ByName("cmd")) == nil {
		return nil, fmt.Errorf("ctl: unknown verb %q", verb)
	}
	return req, nil
}

// ctlResponseJSON renders a CtlResponse as the flat ctl.out line: ok/error
// plus the result message's own fields hoisted to the top level (a bare
// scalar result — spawn's port — under its field name).
func ctlResponseJSON(resp *pb.CtlResponse) []byte {
	out := map[string]json.RawMessage{}
	out["ok"], _ = json.Marshal(resp.Ok)
	if resp.Error != "" {
		out["error"], _ = json.Marshal(resp.Error)
	}
	ref := resp.ProtoReflect()
	if fd := ref.WhichOneof(ref.Descriptor().Oneofs().ByName("result")); fd != nil {
		v := ref.Get(fd)
		if fd.Kind() == protoreflect.MessageKind {
			b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(v.Message().Interface())
			var inner map[string]json.RawMessage
			_ = json.Unmarshal(b, &inner)
			for k, val := range inner {
				if k == "ok" || k == "error" {
					continue
				}
				out[k] = val
			}
		} else {
			out[string(fd.Name())], _ = json.Marshal(v.Interface())
		}
	}
	b, _ := json.Marshal(out)
	return append(b, '\n')
}

func ctlErrorJSON(err error) []byte {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	return append(b, '\n')
}
