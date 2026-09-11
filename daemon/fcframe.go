package main

// fcframe.go — length-prefixed protobuf framing for the daemon↔guest vsock
// channels (protocol/guest.proto). One uint32 big-endian length, then one
// serialized message. The maximum is enforced on the length BEFORE the
// payload is allocated, so a hostile peer cannot make the reader grow a
// buffer to whatever it sends — the property JSON-lines never had (a line
// ends wherever the writer says it does).
//
// Same code, byte for byte, as fcguest/frame.go; the guest module cannot
// import the daemon's, and protocol/ carries generated code only.

import (
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

const (
	fcFrameMaxCtl   = 1 << 20  // guest→host ctl request (9002)
	fcFrameMaxGuest = 16 << 20 // guest→host agent response / stream frame (10000)
	fcFrameMaxHost  = 64 << 20 // host→guest request (attachments ride in MsgReq)
)

// fcFrameMaxWrite is the largest frame this side will PUT on a channel. Both
// peers refuse a declared length above their channel maximum, and the host's is
// the biggest of the three — so a frame larger than that cannot be read by
// anyone, and writing it produces a peer that hangs up rather than an error
// naming the size (audit M117). Checking here means the caller gets a message
// it can act on, whatever the caller is.
const fcFrameMaxWrite = fcFrameMaxHost

func fcWriteFrame(w io.Writer, m proto.Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > fcFrameMaxWrite {
		return fmt.Errorf("frame of %d bytes exceeds the %d-byte channel maximum", len(b), fcFrameMaxWrite)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	// One write for header+body: vsock is a stream, and interleaving two
	// writers' frames on one connection is the caller's problem, not ours —
	// but a single write keeps the header and its body adjacent under any
	// scheduling.
	_, err = w.Write(append(hdr[:], b...))
	return err
}

func fcReadFrame(r io.Reader, max uint32, m proto.Message) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > max {
		return fmt.Errorf("frame of %d bytes exceeds the %d-byte channel maximum", n, max)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return proto.Unmarshal(b, m)
}
