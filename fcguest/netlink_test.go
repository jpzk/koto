package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// netUp must not depend on any binary in the image (1.9.0 regression: `ip`
// is absent from the fedora OCI base and guest.img).
func TestNetUpDoesNotExecIP(t *testing.T) {
	src, err := os.ReadFile("net.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), `runReaped("ip"`) {
		t.Fatal(`net.go still shells out to "ip"`)
	}
}

func TestNlMsgLayout(t *testing.T) {
	m := nlMsg(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL, 7, ifAddrMsg(24, 3),
		nlAttr{unix.IFA_LOCAL, []byte{192, 168, 127, 2}},
		nlAttr{unix.IFA_LABEL, []byte("eth0\x00")}) // 5 bytes -> padded to 8
	want := unix.SizeofNlMsghdr + unix.SizeofIfAddrmsg + (4 + 4) + (4 + 8)
	if len(m) != want {
		t.Fatalf("len=%d want %d", len(m), want)
	}
	if got := binary.NativeEndian.Uint32(m[0:4]); int(got) != len(m) {
		t.Fatalf("nlmsg_len=%d want %d", got, len(m))
	}
	if typ := binary.NativeEndian.Uint16(m[4:6]); typ != unix.RTM_NEWADDR {
		t.Fatalf("type=%d", typ)
	}
	fl := binary.NativeEndian.Uint16(m[6:8])
	for _, f := range []uint16{unix.NLM_F_REQUEST, unix.NLM_F_ACK, unix.NLM_F_CREATE, unix.NLM_F_EXCL} {
		if fl&f == 0 {
			t.Fatalf("flags %#x missing %#x", fl, f)
		}
	}
	if seq := binary.NativeEndian.Uint32(m[8:12]); seq != 7 {
		t.Fatalf("seq=%d", seq)
	}
	// Round-trip through the stdlib parsers.
	msgs, err := syscall.ParseNetlinkMessage(m)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("parse: %v (%d msgs)", err, len(msgs))
	}
	if msgs[0].Data[0] != unix.AF_INET || msgs[0].Data[1] != 24 ||
		binary.NativeEndian.Uint32(msgs[0].Data[4:8]) != 3 {
		t.Fatalf("ifaddrmsg wrong: %v", msgs[0].Data[:8])
	}
	attrs, err := syscall.ParseNetlinkRouteAttr(&msgs[0])
	if err != nil || len(attrs) != 2 {
		t.Fatalf("attrs: %v (%d)", err, len(attrs))
	}
	if attrs[0].Attr.Type != unix.IFA_LOCAL || net.IP(attrs[0].Value).String() != "192.168.127.2" {
		t.Fatalf("attr0 %+v", attrs[0])
	}
	if attrs[1].Attr.Type != unix.IFA_LABEL || string(attrs[1].Value) != "eth0\x00" {
		t.Fatalf("attr1 %+v", attrs[1])
	}
}

func TestRtMsgDefaultRoute(t *testing.T) {
	r := rtMsg()
	if len(r) != unix.SizeofRtMsg || r[0] != unix.AF_INET || r[1] != 0 ||
		r[4] != unix.RT_TABLE_MAIN || r[6] != unix.RT_SCOPE_UNIVERSE || r[7] != unix.RTN_UNICAST {
		t.Fatalf("rtmsg %v", r)
	}
}

func errMsg(seq uint32, errno int32) []byte {
	b := make([]byte, unix.SizeofNlMsghdr+4+unix.SizeofNlMsghdr)
	binary.NativeEndian.PutUint32(b[0:4], uint32(len(b)))
	binary.NativeEndian.PutUint16(b[4:6], unix.NLMSG_ERROR)
	binary.NativeEndian.PutUint32(b[8:12], seq)
	binary.NativeEndian.PutUint32(b[16:20], uint32(errno))
	return b
}

func TestNlCheckAck(t *testing.T) {
	if err := nlCheckAck(errMsg(4, 0), 4); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := nlCheckAck(errMsg(4, -int32(unix.EEXIST)), 4); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("want EEXIST, got %v", err)
	}
	if err := nlCheckAck(errMsg(3, 0), 4); !errors.Is(err, errNoAck) {
		t.Fatalf("other seq: want errNoAck, got %v", err)
	}
}

// TestNetConfigureLive runs the real configuration against a throwaway TAP.
// Needs /dev/net/tun + CAP_NET_ADMIN in a private netns (e.g. podman
// --device /dev/net/tun --cap-add NET_ADMIN --network=none); skips otherwise.
func TestNetConfigureLive(t *testing.T) {
	const name, mac, cidr, gw = "kotonl0", "5a:94:ef:e4:0c:02", "10.250.7.2/24", "10.250.7.1"
	f, err := openTAP(name)
	if err != nil {
		t.Skipf("no TAP here (%v)", err)
	}
	defer f.Close()
	if err := netConfigure(name, mac, 1400, cidr, gw); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("no CAP_NET_ADMIN: %v", err)
		}
		t.Fatalf("netConfigure: %v", err)
	}
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		t.Fatal(err)
	}
	if ifc.HardwareAddr.String() != mac || ifc.MTU != 1400 || ifc.Flags&net.FlagUp == 0 {
		t.Fatalf("link: mac=%s mtu=%d flags=%v", ifc.HardwareAddr, ifc.MTU, ifc.Flags)
	}
	addrs, _ := ifc.Addrs()
	found := false
	for _, a := range addrs {
		if a.String() == cidr {
			found = true
		}
	}
	if !found {
		t.Fatalf("addr %s not in %v", cidr, addrs)
	}
	// /proc/net/route: Iface Destination Gateway (little-endian hex on x86/arm).
	rf, err := os.Open("/proc/net/route")
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	gwIP := net.ParseIP(gw).To4()
	wantGW := strings.ToUpper(hexLE(gwIP))
	sc := bufio.NewScanner(rf)
	ok := false
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) > 2 && fs[0] == name && fs[1] == "00000000" && fs[2] == wantGW {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("no default route via %s on %s", gw, name)
	}
	// A second run must fail loudly (EEXIST), not silently succeed.
	if err := netConfigure(name, mac, 1400, cidr, gw); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("rerun: want EEXIST, got %v", err)
	}
	t.Logf("live: %s mac=%s mtu=%d up addr=%s default via %s", name, mac, ifc.MTU, cidr, gw)
}

func hexLE(ip net.IP) string {
	const h = "0123456789ABCDEF"
	var s []byte
	for i := 3; i >= 0; i-- {
		s = append(s, h[ip[i]>>4], h[ip[i]&15])
	}
	return string(s)
}
