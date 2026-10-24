package main

import (
	"math"
	"strings"
	"testing"

	"koto-protocol/pb"
)

func TestSaturateInt32Boundaries(t *testing.T) {
	cases := []struct {
		in   int
		want int32
	}{
		{0, 0},
		{1, 1},
		{-1, -1},
		{math.MaxInt32, math.MaxInt32},
		{math.MaxInt32 + 1, math.MaxInt32},
		{math.MaxInt64, math.MaxInt32},
		{math.MinInt32, math.MinInt32},
		{math.MinInt32 - 1, math.MinInt32},
		{math.MinInt64, math.MinInt32},
		{1 << 32, math.MaxInt32}, // would wrap to 0
	}
	for _, c := range cases {
		if got := saturateInt32(c.in); got != c.want {
			t.Errorf("saturateInt32(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestPortToPBBoundaries(t *testing.T) {
	for _, p := range []int{0, 1, 2222, 65535} {
		got, err := portToPB(p)
		if err != nil || got != int32(p) {
			t.Errorf("portToPB(%d) = %d, %v; want %d, nil", p, got, err, p)
		}
	}
	for _, p := range []int{-1, 65536, math.MaxInt32, math.MaxInt32 + 1, 1<<32 + 2222, math.MinInt64} {
		if got, err := portToPB(p); err == nil {
			t.Errorf("portToPB(%d) = %d, nil; want an error", p, got)
		}
	}
}

// C#1: ctlpb.go spawnResp port.
func TestCtlRespToPBSpawnPortBounds(t *testing.T) {
	r, err := ctlRespToPB(spawnResp{BaseResp: baseResp{OK: true}, Port: 65535})
	if err != nil || r.GetPort() != 65535 {
		t.Fatalf("port 65535: got %v, %v", r, err)
	}
	for _, p := range []int{65536, 1<<32 + 2222, -1} {
		if _, err := ctlRespToPB(spawnResp{BaseResp: baseResp{OK: true}, Port: p}); err == nil ||
			!strings.Contains(err.Error(), "out of range") {
			t.Errorf("port %d: err = %v, want out-of-range error", p, err)
		}
	}
}

// C#2: grpc_server.go toPBEvent word count.
func TestToPBEventWordsSaturate(t *testing.T) {
	cases := map[int]int32{
		0:                 0,
		math.MaxInt32:     math.MaxInt32,
		math.MaxInt32 + 1: math.MaxInt32,
		1 << 32:           math.MaxInt32,
		math.MaxInt64:     math.MaxInt32,
	}
	for in, want := range cases {
		if got := toPBEvent(Event{Words: in}).GetWords(); got != want {
			t.Errorf("Words %d -> %d, want %d", in, got, want)
		}
	}
}

// C#3/C#4: Spawn/Restart replies route the port through spawnRespPB.
func TestSpawnRespPBPortBounds(t *testing.T) {
	if r := spawnRespPB(65535); !r.Ok || r.Port != 65535 || r.Error != "" {
		t.Fatalf("65535: %+v", r)
	}
	if r := spawnRespPB(0); !r.Ok || r.Port != 0 {
		t.Fatalf("0: %+v", r)
	}
	for _, p := range []int{65536, -1, 1<<32 + 2222} {
		r := spawnRespPB(p)
		if r.Ok || r.Port != 0 || !strings.Contains(r.Error, "out of range") {
			t.Errorf("port %d: %+v, want !Ok + out-of-range error", p, r)
		}
	}
	var _ *pb.SpawnResp = spawnRespPB(1)
}

// C#5: resources.go host MemCapMiB.
func TestHostMemCapMiBSaturates(t *testing.T) {
	old := fcHostMemCapMiB
	t.Cleanup(func() { fcHostMemCapMiB = old })
	for in, want := range map[int]int32{
		0:                 0,
		math.MaxInt32:     math.MaxInt32,
		math.MaxInt32 + 1: math.MaxInt32,
		1 << 32:           math.MaxInt32,
	} {
		fcHostMemCapMiB = in
		if got := hostMemCapMiBPB(); got != want {
			t.Errorf("fcHostMemCapMiB %d -> %d, want %d", in, got, want)
		}
	}
}
