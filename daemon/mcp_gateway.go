package main

// Host side of the MCP gateway (daemon/mcpgw): one HTTP server per running
// group on vsock 9005, reached from the guest through the fc-agent bridge on
// 127.0.0.1:18889. Off unless the daemon runs with KOTO_MCP_GATEWAY=1.
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
// Default off: without it no 9005 listener exists and turns carry no MCP
// servers, so the registry is inert.
func mcpGatewayEnabled() bool {
	v := strings.TrimSpace(os.Getenv("KOTO_MCP_GATEWAY"))
	return v == "1" || strings.EqualFold(v, "true")
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
}

// mcpServersFor lists the connections group g is whitelisted on (names only)
// for the turn's harness config. Empty when the gateway is off.
func mcpServersFor(g string) []string {
	if !mcpGatewayEnabled() {
		return nil
	}
	return mcpPolicy().ConnsFor(g)
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

// mcpOwnedListener closes the group's gateway server together with its vsock
// listener, so fcStop (which closes vm.listeners) tears both down.
type mcpOwnedListener struct {
	net.Listener
	stop func()
}

func (l *mcpOwnedListener) Close() error {
	l.stop()
	return l.Listener.Close()
}

// fcStartMcpListener opens <base>_9005 for g when the gateway is enabled.
// nil listener, nil error = gateway off.
func fcStartMcpListener(g, base string) (net.Listener, error) {
	if !mcpGatewayEnabled() {
		return nil, nil
	}
	ln, err := listenUnix(fmt.Sprintf("%s_%d", base, fcPortMcp))
	if err != nil {
		return nil, err
	}
	handle, stop := mcpServeGroup(g, ln.Addr())
	go fcAcceptLoop(g, ln, handle)
	return &mcpOwnedListener{Listener: ln, stop: stop}, nil
}
