package main

// netlink.go — configure the guest L3 interface over rtnetlink.
//
// koto 1.9.0 regression: netUp used to shell out to `ip` (iproute2), which the
// fedora OCI base and guest.img do not ship, so network=wan/full groups booting
// an image came up with a TAP but no address/route (DNS_RC=2). The agent is
// PID 1 (root) and already links golang.org/x/sys/unix, so it now speaks
// rtnetlink directly: no binary in the image is required.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"

	"syscall"

	"golang.org/x/sys/unix"
)

// nlAttr is one rtattr (type + payload); nlMsg packs header + attrs.
type nlAttr struct {
	typ  uint16
	data []byte
}

func nlAlign(n int) int { return (n + unix.NLMSG_ALIGNTO - 1) &^ (unix.NLMSG_ALIGNTO - 1) }

// nlMsg builds a complete netlink request: nlmsghdr, the fixed family header
// (ifinfomsg / ifaddrmsg / rtmsg, already serialized), then rtattrs.
func nlMsg(typ, flags uint16, seq uint32, fixed []byte, attrs ...nlAttr) []byte {
	b := make([]byte, unix.SizeofNlMsghdr, 128)
	b = append(b, fixed...)
	for len(b)%unix.NLMSG_ALIGNTO != 0 {
		b = append(b, 0)
	}
	for _, a := range attrs {
		l := unix.SizeofRtAttr + len(a.data)
		var h [unix.SizeofRtAttr]byte
		binary.NativeEndian.PutUint16(h[0:2], uint16(l))
		binary.NativeEndian.PutUint16(h[2:4], a.typ)
		b = append(b, h[:]...)
		b = append(b, a.data...)
		for len(b)%unix.RTA_ALIGNTO != 0 {
			b = append(b, 0)
		}
	}
	binary.NativeEndian.PutUint32(b[0:4], uint32(len(b)))
	binary.NativeEndian.PutUint16(b[4:6], typ)
	binary.NativeEndian.PutUint16(b[6:8], flags|unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	binary.NativeEndian.PutUint32(b[8:12], seq)
	// pid (b[12:16]) = 0: the kernel assigns ours.
	return b
}

// ifInfoMsg serializes struct ifinfomsg.
func ifInfoMsg(index int32, flags, change uint32) []byte {
	b := make([]byte, unix.SizeofIfInfomsg)
	b[0] = unix.AF_UNSPEC
	binary.NativeEndian.PutUint32(b[4:8], uint32(index))
	binary.NativeEndian.PutUint32(b[8:12], flags)
	binary.NativeEndian.PutUint32(b[12:16], change)
	return b
}

// ifAddrMsg serializes struct ifaddrmsg for an IPv4 address.
func ifAddrMsg(prefix uint8, index int32) []byte {
	b := make([]byte, unix.SizeofIfAddrmsg)
	b[0] = unix.AF_INET
	b[1] = prefix
	b[3] = unix.RT_SCOPE_UNIVERSE
	binary.NativeEndian.PutUint32(b[4:8], uint32(index))
	return b
}

// rtMsg serializes struct rtmsg for an IPv4 default unicast route.
func rtMsg() []byte {
	b := make([]byte, unix.SizeofRtMsg)
	b[0] = unix.AF_INET
	// b[1] dst_len = 0 (default), b[2] src_len = 0, b[3] tos = 0
	b[4] = unix.RT_TABLE_MAIN
	b[5] = unix.RTPROT_BOOT
	b[6] = unix.RT_SCOPE_UNIVERSE
	b[7] = unix.RTN_UNICAST
	return b
}

func u32(v uint32) []byte { b := make([]byte, 4); binary.NativeEndian.PutUint32(b, v); return b }

// errNoAck: the reply batch held no NLMSG_ERROR for our sequence number.
var errNoAck = errors.New("netlink: no ack for request")

// nlCheckAck parses the kernel's reply to one request: an NLMSG_ERROR whose
// errno is 0 is the ACK; anything else is the failure.
func nlCheckAck(resp []byte, seq uint32) error {
	msgs, err := syscall.ParseNetlinkMessage(resp)
	if err != nil {
		return fmt.Errorf("netlink: parse reply: %w", err)
	}
	for _, m := range msgs {
		if m.Header.Seq != seq || m.Header.Type != unix.NLMSG_ERROR {
			continue
		}
		if len(m.Data) < 4 {
			return fmt.Errorf("netlink: short NLMSG_ERROR")
		}
		if e := int32(binary.NativeEndian.Uint32(m.Data[0:4])); e != 0 {
			return syscall.Errno(-e)
		}
		return nil
	}
	return errNoAck
}

// nlConn is a bound NETLINK_ROUTE socket issuing one request at a time.
type nlConn struct {
	fd  int
	seq uint32
}

func nlDial() (*nlConn, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("netlink bind: %w", err)
	}
	return &nlConn{fd: fd}, nil
}

func (c *nlConn) Close() error { return unix.Close(c.fd) }

// do sends one request and waits for its ACK.
func (c *nlConn) do(what string, typ, flags uint16, fixed []byte, attrs ...nlAttr) error {
	c.seq++
	req := nlMsg(typ, flags, c.seq, fixed, attrs...)
	if err := unix.Sendto(c.fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("%s: send: %w", what, err)
	}
	buf := make([]byte, 8192)
	for {
		n, _, err := unix.Recvfrom(c.fd, buf, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: recv: %w", what, err)
		}
		if err := nlCheckAck(buf[:n], c.seq); err != nil {
			if errors.Is(err, errNoAck) {
				continue // unrelated/stale message; keep reading
			}
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}
}

// netConfigure gives interface name its MAC and MTU, brings it up, assigns
// cidr and installs a default route via gw. Same order as the old iproute2
// sequence: MAC/MTU, up, address, route.
func netConfigure(name, mac string, mtu int, cidr, gw string) error {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return fmt.Errorf("bad MAC %q: %v", mac, err)
	}
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("bad CIDR %q: %v", cidr, err)
	}
	gwIP := net.ParseIP(gw).To4()
	if gwIP == nil {
		return fmt.Errorf("bad gateway %q", gw)
	}
	prefix, _ := ipnet.Mask.Size()
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("interface %s: %w", name, err)
	}
	idx := int32(ifc.Index)

	c, err := nlDial()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.do("set mac/mtu", unix.RTM_NEWLINK, 0, ifInfoMsg(idx, 0, 0),
		nlAttr{unix.IFLA_ADDRESS, []byte(hw)},
		nlAttr{unix.IFLA_MTU, u32(uint32(mtu))}); err != nil {
		return err
	}
	if err := c.do("link up", unix.RTM_NEWLINK, 0, ifInfoMsg(idx, unix.IFF_UP, unix.IFF_UP)); err != nil {
		return err
	}
	ip4 := []byte(ip.To4())
	bcast := make([]byte, 4)
	for i := range bcast {
		bcast[i] = ipnet.IP.To4()[i] | ^ipnet.Mask[i]
	}
	if err := c.do("addr add", unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL, ifAddrMsg(uint8(prefix), idx),
		nlAttr{unix.IFA_LOCAL, ip4},
		nlAttr{unix.IFA_ADDRESS, ip4},
		nlAttr{unix.IFA_BROADCAST, bcast}); err != nil {
		return err
	}
	if err := c.do("default route", unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_EXCL, rtMsg(),
		nlAttr{unix.RTA_GATEWAY, []byte(gwIP)},
		nlAttr{unix.RTA_OIF, u32(uint32(idx))}); err != nil {
		return err
	}
	return nil
}
