package main

// Host side of the MCP gateway (daemon/mcpgw): one HTTP server per running
// group on vsock 9005, reached from the guest through the fc-agent bridge on
// 127.0.0.1:18889. On by default (KOTO_MCP_GATEWAY=0 turns it off), and only
// for groups with a non-empty effective set (mcpLive).
//
// The group identity is the vsock listener the connection arrived on — the
// guest never names itself — so a group can only ever act as itself. Policy
// is re-read from the registry store on every request (mcpgw re-evaluates
// the intersection each time), so allow/deny/grant/revoke apply to the next
// request without a restart.

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"koto/mcpgw"
	"koto/mcpreg"
)

// fcPortMcp is the guest→host vsock port of the MCP gateway.
const fcPortMcp = 9005

// mcpGatewayEnabled is the daemon flag (env, like the other KOTO_* knobs).
// Default ON. KOTO_MCP_GATEWAY=0 (or false/off/no) is the kill switch: no
// 9005 listener for any group and no MCP config in any turn, whatever the
// registry says. Even when on, a group with an empty effective set gets
// neither (mcpServersFor), so an unused registry is inert.
func mcpGatewayEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KOTO_MCP_GATEWAY"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

var (
	mcpGwOnce sync.Once
	mcpGwVal  *mcpgw.Gateway
)

// mcpPolicy is the current registry, or nil (deny everything) when the store
// cannot be opened.
func mcpPolicy() *mcpreg.Registry {
	s, err := mcpStore()
	if err != nil {
		return nil
	}
	return s.Current()
}

func mcpGateway() *mcpgw.Gateway {
	mcpGwOnce.Do(func() {
		mcpGwVal = &mcpgw.Gateway{
			Policy: mcpPolicy,
			Audit:  mcpAudit,
		}
	})
	return mcpGwVal
}

// mcpAudit writes one line per gateway decision: who, which connection and
// tool, the verdict and why. Never arguments, results or credentials.
func mcpAudit(e mcpgw.AuditEntry) {
	verdict := "deny"
	if e.Allowed {
		verdict = "allow"
	}
	tool := e.Tool
	if tool == "" {
		tool = "-"
	}
	emitLogfG("mcp", e.Group, "info", "[%s] mcp %s conn=%s method=%s tool=%s reason=%q dur=%s",
		e.Group, verdict, e.Conn, e.Method, tool, e.Reason, e.Dur.Round(time.Millisecond))
	mcpAuditCall(e)
}

// mcpServersFor lists the connections on which group g has at least one
// effective tool (names only, sorted) for the turn's harness config.
// Whitelisted with nothing granted (or a write-class / disabled connection)
// does not count. Empty when the gateway is off.
func mcpServersFor(g string) []string {
	if !mcpGatewayEnabled() {
		return nil
	}
	reg := mcpPolicy()
	var out []string
	for _, c := range reg.ConnsFor(g) {
		if e, err := reg.Effective(c, g); err == nil && len(e.Tools) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// mcpServeGroup serves the gateway for g on the accepted vsock connections
// that fcAcceptLoop hands to the returned function. Each handler call blocks
// until its connection is closed, so fcAcceptLoop's per-group connection cap
// keeps applying to MCP traffic too.
func mcpServeGroup(g string, addr net.Addr) (handle func(net.Conn), stop func()) {
	l := &mcpConnListener{ch: make(chan net.Conn), done: make(chan struct{}), addr: addr}
	srv := &http.Server{
		Handler:           mcpGateway().Handler(g),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() { _ = srv.Serve(l) }()
	handle = func(c net.Conn) {
		tc := &mcpTrackedConn{Conn: c, closed: make(chan struct{})}
		select {
		case l.ch <- tc:
			select {
			case <-tc.closed:
			case <-l.done:
				c.Close()
			}
		case <-l.done:
			c.Close()
		}
	}
	return handle, func() { _ = srv.Close() }
}

// mcpConnListener adapts fcAcceptLoop's push model to http.Server's pull model.
type mcpConnListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
	addr net.Addr
}

func (l *mcpConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *mcpConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *mcpConnListener) Addr() net.Addr { return l.addr }

type mcpTrackedConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *mcpTrackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// mcpLive is a group's 9005 listener slot for one boot. It sits in
// vm.listeners (so every teardown path closes it) and opens or closes the
// real <base>_9005 listener to follow the group's effective set: a group
// with nothing granted has no 9005 socket at all, an allow+grant opens it
// before the next turn is sent, and a revoke/deny/disable closes it (and the
// connections on it) as soon as the registry write lands.
type mcpLive struct {
	g    string
	path string
	uid  int // jail uid the socket is handed to; <0 = leave ownership alone (tests)

	mu     sync.Mutex
	ln     net.Listener
	stop   func()
	closed bool
}

// fcStartMcpListener creates g's slot for this boot and syncs it once.
func fcStartMcpListener(g, base string, uid int) (*mcpLive, error) {
	m := &mcpLive{g: g, path: fmt.Sprintf("%s_%d", base, fcPortMcp), uid: uid}
	if err := m.Sync(); err != nil {
		return nil, err
	}
	return m, nil
}

// Open reports whether the real 9005 listener currently exists.
func (m *mcpLive) Open() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ln != nil
}

// Sync opens or closes the 9005 listener to match mcpServersFor(g).
func (m *mcpLive) Sync() error {
	want := len(mcpServersFor(m.g)) > 0
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.closed:
		return nil
	case want && m.ln == nil:
		ln, err := listenUnix(m.path)
		if err != nil {
			return err
		}
		if m.uid >= 0 {
			// Same hand-off fcJailFixupPerms does at boot: the VMM connects
			// as the jail uid (a socket opened after boot was not there then).
			if err := os.Chown(m.path, m.uid, m.uid); err != nil {
				ln.Close()
				return fmt.Errorf("mcp listener chown: %w", err)
			}
		}
		handle, stop := mcpServeGroup(m.g, ln.Addr())
		go fcAcceptLoop(m.g, ln, handle)
		m.ln, m.stop = ln, stop
		emitLogfG("mcp", m.g, "info", "[%s] mcp gateway listener open", m.g)
	case !want && m.ln != nil:
		m.closeLocked()
		emitLogfG("mcp", m.g, "info", "[%s] mcp gateway listener closed (no effective grants)", m.g)
	}
	return nil
}

func (m *mcpLive) closeLocked() {
	if m.ln == nil {
		return
	}
	m.stop()
	_ = m.ln.Close() // unlinks the socket
	m.ln, m.stop = nil, nil
}

// Close ends the slot for good (VM teardown): no later Sync reopens it.
func (m *mcpLive) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.closeLocked()
	return nil
}

// Accept is never called: fcAcceptLoop runs on the inner listener. The slot
// implements net.Listener only so it can live in vm.listeners.
func (m *mcpLive) Accept() (net.Conn, error) { return nil, net.ErrClosed }

func (m *mcpLive) Addr() net.Addr { return &net.UnixAddr{Name: m.path, Net: "unix"} }

// mcpSyncGroup re-syncs g's running VM (no-op when it is not running).
func mcpSyncGroup(g string) {
	fcMu.Lock()
	vm := fcVMs[g]
	var m *mcpLive
	if vm != nil {
		m = vm.mcp
	}
	fcMu.Unlock()
	if m == nil {
		return
	}
	if err := m.Sync(); err != nil {
		emitLogfG("mcp", g, "warn", "[%s] mcp gateway listener: %v", g, err)
	}
}

// mcpSyncAll re-syncs every running VM after a registry write.
func mcpSyncAll() {
	fcMu.Lock()
	gs := make([]string, 0, len(fcVMs))
	for g := range fcVMs {
		gs = append(gs, g)
	}
	fcMu.Unlock()
	for _, g := range gs {
		mcpSyncGroup(g)
	}
}
