package main

import (
	"fmt"
	"math"

	"koto-protocol/pb"
)

// Narrowing int -> int32 for the protobuf wire. A bare int32(v) wraps
// silently on 64-bit hosts (CodeQL go/incorrect-integer-conversion); every
// such conversion goes through one of these, chosen per call-site meaning:
// counts/sizes saturate, ports must be valid or the reply is an error.

// saturateInt32 clamps v into [math.MinInt32, math.MaxInt32]. For counters
// and sizes (think-block word counts, MiB caps) where "at least this much"
// is the honest answer and a wrapped negative would be a lie.
func saturateInt32(v int) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < math.MinInt32 {
		return math.MinInt32
	}
	return int32(v)
}

// portToPB narrows a TCP port. 0 is allowed (no port / failed spawn);
// anything outside 0..65535 is a bug upstream and is reported, never wrapped.
func portToPB(p int) (int32, error) {
	if p < 0 || p > math.MaxUint16 {
		return 0, fmt.Errorf("port %d out of range 0..65535", p)
	}
	return int32(p), nil
}

// spawnRespPB is the success reply for Spawn/Restart. An out-of-range port
// becomes an error reply rather than a wrapped number the client would dial.
func spawnRespPB(port int) *pb.SpawnResp {
	p, err := portToPB(port)
	if err != nil {
		return &pb.SpawnResp{Error: err.Error()}
	}
	return &pb.SpawnResp{Ok: true, Port: p}
}

// hostMemCapMiBPB reports the fleet memory ceiling (0 = unlimited),
// saturated: a cap past int32 MiB (2 PiB) is effectively unlimited anyway.
func hostMemCapMiBPB() int32 { return saturateInt32(fcHostMemCapMiB) }
