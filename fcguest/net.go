package main

// net.go — L3 networking for internet=full microVM groups (guest side).
//
// When the daemon starts a full group it (a) attaches a gVisor gateway on host
// vsock 9003 (fc.go / fcnet.go) and (b) sends the init op with net="l3". On
// that signal the agent creates a TAP device, gives it the group's fixed L3
// address, and pumps ethernet frames between the TAP and vsock 9003 using the
// same framing the host gateway speaks (Qemu protocol: a 4-byte big-endian
// length prefix per frame). The result is a real NIC in the guest — arbitrary
// outbound TCP/UDP/ICMP, NAT'd and DNS-resolved by the host gateway.
//
// none groups never receive net="l3", so no TAP and no route exist — the
// default "no NIC = no egress" isolation is untouched.

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

const (
	portNet      = 9003
	netTAPName   = "eth0"
	netGuestCIDR = "192.168.127.2/24"
	netGatewayIP = "192.168.127.1"
	netGuestMAC  = "5a:94:ef:e4:0c:02" // must match fcNetGuestMAC in fcnet.go
	netMTU       = 1500
	netFrameMax  = netMTU + 64 // ethernet header + slack
)

// tapFile keeps the TAP alive: a non-persistent TUN/TAP interface exists only
// while its fd is open, so this global holds it for the agent's lifetime.
var tapFile *os.File

// netUp creates and configures the guest L3 interface, then starts the frame
// pump. Called once from handleInit when net="l3".
func netUp() error {
	f, err := openTAP(netTAPName)
	if err != nil {
		return err
	}
	tapFile = f
	// Configure via iproute2 (in the rootfs) — clearer and less brittle than
	// hand-rolled netlink, and the agent is PID 1 (root). Order matters: set
	// MAC/MTU, bring up, then address + default route.
	for _, a := range [][]string{
		{"link", "set", netTAPName, "address", netGuestMAC},
		{"link", "set", netTAPName, "mtu", strconv.Itoa(netMTU)},
		{"link", "set", netTAPName, "up"},
		{"addr", "add", netGuestCIDR, "dev", netTAPName},
		{"route", "add", "default", "via", netGatewayIP},
	} {
		if err := runReaped("ip", a...); err != nil {
			return err
		}
	}
	// Point DNS at the gateway. The rootfs is read-only, so /etc/resolv.conf is
	// a symlink to /run/resolv.conf (a writable tmpfs); we just write the
	// target. none groups never call netUp, leaving the symlink dangling — no
	// DNS, which is correct for the no-egress default.
	if err := os.WriteFile("/run/resolv.conf", []byte("nameserver "+netGatewayIP+"\n"), 0o644); err != nil {
		logf("write resolv.conf: %v", err)
	}
	go netPump()
	return nil
}

// openTAP opens /dev/net/tun and binds a named TAP interface (no packet info
// header, so frames on the fd are raw ethernet — exactly the gateway's wire
// format).
func openTAP(name string) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "tap"), nil
}

// netPump relays ethernet frames between the TAP and host vsock 9003. One vsock
// connection at a time; on a broken connection it redials (the FC vsock link
// lives for the VM's lifetime, so this is rare). Mirrors logForward's model.
func netPump() {
	for {
		conn := dialRetry(portNet)
		// vsock → TAP: read one length-prefixed frame, write it to the TAP.
		//
		// CLOSING the shared connection on the way out is what makes the
		// redial below reachable (audit 2026-09-11 L139). This goroutine used
		// to just return: the TAP loop beneath it blocks in a read that only
		// completes when the GUEST sends a packet, so after a host-side
		// gateway or daemon restart an idle guest sat there with a dead
		// connection and no path to conn.Close() — its networking gone until
		// something inside it happened to transmit, which for an agent
		// waiting on a reply is never. Closing makes the next Write fail, the
		// loop break, and the outer for redial.
		dead := make(chan struct{})
		go func(c *vconn) {
			defer func() {
				c.Close()
				close(dead)
			}()
			hdr := make([]byte, 4)
			for {
				if _, err := io.ReadFull(c, hdr); err != nil {
					return
				}
				n := binary.BigEndian.Uint32(hdr)
				if n == 0 || n > netFrameMax {
					return
				}
				frame := make([]byte, n)
				if _, err := io.ReadFull(c, frame); err != nil {
					return
				}
				_, _ = tapFile.Write(frame)
			}
		}(conn)
		// TAP → vsock: frame each read and length-prefix it.
		//
		// The read carries a DEADLINE so this loop can notice `dead` (audit
		// 2026-09-11 L139). Without one it blocks until the guest transmits,
		// which for a VM waiting on inbound traffic — a published port's
		// server, a TCP retransmit, anything the agent is waiting to receive
		// — may be never: the connection was gone and nothing redialled it.
		// A TAP fd is pollable, so the deadline is real; the wakeup costs one
		// timed-out read per second on an idle link.
		buf := make([]byte, netFrameMax)
		hdr := make([]byte, 4)
		for {
			_ = tapFile.SetReadDeadline(time.Now().Add(time.Second))
			n, err := tapFile.Read(buf)
			if errors.Is(err, os.ErrDeadlineExceeded) {
				select {
				case <-dead:
					err = io.EOF // the host side is gone; redial
				default:
					continue
				}
			}
			if n > 0 {
				binary.BigEndian.PutUint32(hdr, uint32(n))
				if _, werr := conn.Write(hdr); werr != nil {
					break
				}
				if _, werr := conn.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		_ = tapFile.SetReadDeadline(time.Time{})
		conn.Close()
		<-dead // let the receive goroutine retire before redialling
	}
}
