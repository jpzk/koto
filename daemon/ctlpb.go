package main

// ctlpb.go — the protobuf face of the in-guest control plane (vsock 9002,
// protocol/guest.proto → CtlRequest/CtlResponse).
//
// ctlDispatch (ctl.go) stays the typed core: its verb handlers, the
// authorization rules and every existing test speak the JSON envelope the
// plane was born with. This adapter sits between the wire and that core:
//
//   CtlRequest (decoded, so already shape-validated: closed verb set via the
//   oneof, typed fields, bytes as bytes) → the equivalent JSON envelope →
//   ctlDispatch → the verb's response struct → CtlResponse.
//
// The JSON hop is host-internal and post-validation — the daemon never
// parses guest-authored JSON on this plane any more; the guest agent turns
// the shell tools' JSON lines into a CtlRequest with strict protojson before
// they leave the guest, and anything that doesn't fit the schema is refused
// there (fcguest/ctl.go). Folding the handlers onto the pb types directly is
// a mechanical follow-up; this adapter is what the tests pin.

import (
	"encoding/json"
	"fmt"

	"koto-protocol/pb"

	"google.golang.org/protobuf/encoding/protojson"
)

// ctlReqEnvelope renders a decoded CtlRequest as the JSON line ctlDispatch
// consumes: the oneof's field name is the verb, its message the fields.
// protojson emits proto names (the JSON keys the shell tools already write)
// and bytes as base64 — exactly the encoding the verb handlers expect.
func ctlReqEnvelope(req *pb.CtlRequest) ([]byte, string, error) {
	ref := req.ProtoReflect()
	fd := ref.WhichOneof(ref.Descriptor().Oneofs().ByName("cmd"))
	if fd == nil {
		return nil, "", fmt.Errorf("ctl: empty request")
	}
	verb := string(fd.Name())
	inner := ref.Get(fd).Message().Interface()
	body, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(inner)
	if err != nil {
		return nil, verb, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, verb, err
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	m["cmd"], _ = json.Marshal(verb)
	line, err := json.Marshal(m)
	return line, verb, err
}

// ctlDispatchPB serves one control-plane request from group owner.
func ctlDispatchPB(owner string, req *pb.CtlRequest) *pb.CtlResponse {
	line, verb, err := ctlReqEnvelope(req)
	if err != nil {
		return &pb.CtlResponse{Error: err.Error()}
	}
	emitLogfG("ctl", owner, "info", "[%s] %s", owner, string(line))
	resp, err := ctlRespToPB(ctlDispatch(owner, line))
	if err != nil {
		emitLogfG("ctl", owner, "error", "[%s] %s: %v", owner, verb, err)
		return &pb.CtlResponse{Error: "ctl: internal: " + err.Error()}
	}
	return resp
}

// ctlRespToPB maps every response type ctlDispatch can return onto
// CtlResponse. An unmapped type is an error, not a silent empty reply —
// TestCtlPBEveryVerbMaps pins that each verb's response has a case here.
func ctlRespToPB(v any) (*pb.CtlResponse, error) {
	switch r := v.(type) {
	case baseResp:
		return &pb.CtlResponse{Ok: r.OK, Error: r.Error}, nil
	case spawnResp:
		return &pb.CtlResponse{Ok: r.OK, Error: r.Error, Result: &pb.CtlResponse_Port{Port: int32(r.Port)}}, nil
	case listResp:
		groups := map[string]*pb.GroupInfo{}
		for g, gi := range r.Groups {
			groups[g] = toPBGroupInfo(gi)
		}
		return &pb.CtlResponse{Ok: r.OK, Error: r.Error,
			Result: &pb.CtlResponse_List{List: &pb.ListResp{Ok: r.OK, Error: r.Error, Groups: groups}}}, nil
	case resourcesResp:
		return &pb.CtlResponse{Ok: r.OK, Error: r.Error,
			Result: &pb.CtlResponse_Resources{Resources: toPBResources(r)}}, nil
	case configResp:
		return &pb.CtlResponse{Ok: r.OK, Error: r.Error,
			Result: &pb.CtlResponse_Config{Config: &pb.ConfigResp{Ok: r.OK, Error: r.Error, Config: toStruct(r.Config)}}}, nil
	case ctlTailResp:
		return &pb.CtlResponse{Ok: r.OK, Error: r.Error,
			Result: &pb.CtlResponse_Tail{Tail: &pb.CtlTailResp{Text: r.Text}}}, nil
	case goalResp:
		return &pb.CtlResponse{Ok: r.OK, Error: r.Error,
			Result: &pb.CtlResponse_Goal{Goal: &pb.GoalResp{Ok: r.OK, Error: r.Error, Item: toPBGoalItem(r.Item)}}}, nil
	case goalListResp:
		out := &pb.GoalListResp{Ok: r.OK, Error: r.Error}
		for _, it := range r.Goals {
			out.Goals = append(out.Goals, toPBGoalItem(it))
		}
		return &pb.CtlResponse{Ok: r.OK, Error: r.Error, Result: &pb.CtlResponse_Goals{Goals: out}}, nil
	case schedAddResp:
		return &pb.CtlResponse{Ok: r.OK, Error: r.Error,
			Result: &pb.CtlResponse_Sched{Sched: &pb.SchedAddResp{Ok: r.OK, Error: r.Error, Item: toPBScheduleItem(r.Item)}}}, nil
	case schedListResp:
		out := &pb.SchedListResp{Ok: r.OK, Error: r.Error}
		for _, it := range r.Schedules {
			out.Schedules = append(out.Schedules, toPBScheduleItem(it))
		}
		return &pb.CtlResponse{Ok: r.OK, Error: r.Error, Result: &pb.CtlResponse_Scheds{Scheds: out}}, nil
	}
	return nil, fmt.Errorf("unmapped ctl response type %T", v)
}

// toPBResources converts the ctl plane's resources answer (with its
// precomputed ratios — see resourcesCtlResp) to the wire message.
func toPBResources(r resourcesResp) *pb.ResourcesResp {
	out := &pb.ResourcesResp{
		Ok: r.OK, Error: r.Error,
		Host: &pb.HostResources{
			FsTotalBytes:     r.Host.FSTotalBytes,
			FsFreeBytes:      r.Host.FSFreeBytes,
			FsUsedPct:        r.Host.FSUsedPct,
			AllocTotalBytes:  r.Host.AllocTotalBytes,
			ProvisionedBytes: r.Host.ProvisionedBytes,
			Groups:           r.Host.Groups,
			RunningGroups:    r.Host.RunningGroups,
		},
	}
	for _, g := range r.Groups {
		out.Groups = append(out.Groups, &pb.GroupResources{
			Group:               g.Group,
			Running:             g.Running,
			AllocBytes:          g.AllocBytes,
			DeclaredBytes:       g.DeclaredBytes,
			AllocPct:            g.AllocPct,
			GrowthBytesPerHour:  g.GrowthBytesPerHour,
			GrowthSpanSeconds:   g.GrowthSpanSeconds,
			RssBytes:            g.RSSBytes,
			CpuPct:              g.CPUPct,
			Vcpus:               g.Vcpus,
			MemMib:              g.MemMiB,
			GuestMemTotalBytes:  g.GuestMemTotalBytes,
			GuestMemAvailBytes:  g.GuestMemAvailBytes,
			GuestMemUsedPct:     g.GuestMemUsedPct,
			GuestDiskTotalBytes: g.GuestDiskTotalBytes,
			GuestDiskAvailBytes: g.GuestDiskAvailBytes,
			GuestDiskUsedBytes:  g.GuestDiskUsedBytes,
			GuestDiskUsedPct:    g.GuestDiskUsedPct,
		})
	}
	return out
}

// ctlRequestFromJSON is the guest agent's conversion, mirrored host-side for
// tests and for `koto ctl`-style tooling: a {"cmd":"<verb>", ...fields} line
// becomes a CtlRequest via STRICT protojson (unknown field, wrong type, or
// unknown verb → error). Kept byte-compatible with fcguest/ctl.go.
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
