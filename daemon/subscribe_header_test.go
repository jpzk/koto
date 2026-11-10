package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"koto-protocol/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// headerStream is a SubscribeGroup server stream that records, at the moment
// the handler sends its headers, whether the subscriber was already
// registered — the ordering `koto ctl ask` relies on.
type headerStream struct {
	grpc.ServerStream
	ctx        context.Context
	g          string
	headerSent chan bool // registered-at-SendHeader
}

func (h *headerStream) Context() context.Context { return h.ctx }
func (h *headerStream) Send(*pb.Event) error     { return nil }
func (h *headerStream) SendHeader(metadata.MD) error {
	subsLock.Lock()
	n := len(subscribers[h.g])
	subsLock.Unlock()
	h.headerSent <- n > 0
	return nil
}

// e2e 2026-11-10: `koto ctl ask` subscribed, then sent — but a server stream
// is returned to the client before the handler registers, so the Send's
// prompt frame could be emitted to nobody and the ask hung until timeout
// (claude harness, first turn). The handler must send headers only AFTER the
// subscriber is registered, and the client waits for them.
func TestSubscribeGroupSendsHeaderAfterRegistering(t *testing.T) {
	fcHarness(t)
	const g = "peer"
	if err := os.MkdirAll(filepath.Dir(groupLogPath(g)), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hs := &headerStream{ctx: ctx, g: g, headerSent: make(chan bool, 1)}
	errc := make(chan error, 1)
	go func() { errc <- (&kotoServer{}).SubscribeGroup(&pb.SubscribeReq{Group: g}, hs) }()

	select {
	case reg := <-hs.headerSent:
		if !reg {
			t.Fatal("headers sent before the subscriber was registered")
		}
	case err := <-errc:
		t.Fatalf("handler returned without sending headers: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("SubscribeGroup never sent headers — ctl ask would block or race")
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return on cancel")
	}
	subsLock.Lock()
	n := len(subscribers[g])
	subsLock.Unlock()
	if n != 0 {
		t.Errorf("subscriber left registered after disconnect (%d)", n)
	}
}

type failHeaderStream struct{ headerStream }

func (f *failHeaderStream) SendHeader(metadata.MD) error { return errors.New("gone") }

// A failed header send must not leave the subscriber registered.
func TestSubscribeGroupHeaderFailureDeregisters(t *testing.T) {
	fcHarness(t)
	const g = "peer"
	_ = os.MkdirAll(filepath.Dir(groupLogPath(g)), 0o755)
	fs := &failHeaderStream{headerStream{ctx: context.Background(), g: g}}
	if err := (&kotoServer{}).SubscribeGroup(&pb.SubscribeReq{Group: g}, fs); err == nil {
		t.Fatal("want error from failed SendHeader")
	}
	subsLock.Lock()
	n := len(subscribers[g])
	subsLock.Unlock()
	if n != 0 {
		t.Errorf("subscriber left registered after header failure (%d)", n)
	}
}
