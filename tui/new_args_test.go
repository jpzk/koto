package main

import (
	"context"
	"reflect"
	"testing"

	"google.golang.org/grpc"

	"koto-protocol/pb"
)

func TestParseNewArgs(t *testing.T) {
	for _, c := range []struct {
		in    string
		group string
		extra map[string]any
	}{
		{"g", "g", map[string]any{}},
		{"g venice m1", "g", map[string]any{"provider": "venice", "model": "m1"}},
		{"g rootfs=kali", "g", map[string]any{"rootfs": "kali"}},
		{"g root=yes", "g", map[string]any{"root": "yes"}},
		{"rootfs=KALI g root=No size=Large", "g", map[string]any{"rootfs": "kali", "root": "no", "size": "large"}},
		{"g claudesdk opus rootfs=fedora root=yes size=small", "g",
			map[string]any{"provider": "claudesdk", "model": "opus", "rootfs": "fedora", "root": "yes", "size": "small"}},
	} {
		g, extra, errText := parseNewArgs(c.in)
		if errText != "" {
			t.Errorf("%q: unexpected error %q", c.in, errText)
			continue
		}
		if g != c.group || !reflect.DeepEqual(extra, c.extra) {
			t.Errorf("%q: got (%q, %v), want (%q, %v)", c.in, g, extra, c.group, c.extra)
		}
	}
	for _, bad := range []string{
		"", "   ", "a b c d", "g nosuchprovider",
		"g rootfs=", "g rootfs=../etc", "g rootfs=a/b", "g rootfs=-x",
		"g root=", "g root=true", "g root=1",
		"g size=huge",
	} {
		if _, _, errText := parseNewArgs(bad); errText == "" {
			t.Errorf("%q: accepted", bad)
		}
	}
}

type spawnCapture struct {
	pb.KotoClient
	got *pb.SpawnReq
}

func (c *spawnCapture) Spawn(_ context.Context, r *pb.SpawnReq, _ ...grpc.CallOption) (*pb.SpawnResp, error) {
	c.got = r
	return &pb.SpawnResp{Ok: true}, nil
}

// The parsed tokens must actually reach the wire, not stop at the map.
func TestNewArgsReachSpawnReq(t *testing.T) {
	g, extra, errText := parseNewArgs("g venice m rootfs=kali root=yes size=medium")
	if errText != "" {
		t.Fatal(errText)
	}
	extra["group"] = g
	cl := &spawnCapture{}
	if _, err := callRPC(context.Background(), cl, "spawn", extra); err != nil {
		t.Fatal(err)
	}
	want := &pb.SpawnReq{Group: "g", Provider: "venice", Model: "m", Size: "medium", Rootfs: "kali", Root: "yes"}
	if cl.got == nil || cl.got.Group != want.Group || cl.got.Provider != want.Provider || cl.got.Model != want.Model ||
		cl.got.Size != want.Size || cl.got.Rootfs != want.Rootfs || cl.got.Root != want.Root {
		t.Fatalf("SpawnReq = %v, want %v", cl.got, want)
	}
}
