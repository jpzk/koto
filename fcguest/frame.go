package main

// frame.go — length-prefixed protobuf framing for the daemon↔guest vsock
// channels (protocol/guest.proto): one uint32 big-endian length, then one
// serialized message, with the maximum enforced on the length BEFORE the
// payload is allocated. Same code as daemon/fcframe.go — this module cannot
// import the daemon's, and protocol/ carries generated code only.

import (
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

const (
	frameMaxHost = 64 << 20 // host→guest request / shell input frame
)

func writeFrame(w io.Writer, m proto.Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	_, err = w.Write(append(hdr[:], b...))
	return err
}

func readFrame(r io.Reader, max uint32, m proto.Message) error {
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
