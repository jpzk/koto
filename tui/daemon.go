package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"

	"clawson-protocol"
)

// Wire types are shared with the daemon via the clawson-protocol module.
// Aliasing under the TUI's existing names (Event, GroupInfo) keeps the
// rest of the package readable. Ts on Event is now float64 (the actual
// wire shape) — call sites that want int64 cast at use.
type (
	Event     = protocol.Event
	GroupInfo = protocol.GroupInfo
)

// daemonCall opens a fresh connection, writes one JSON request, reads one
// JSON response, closes. Suitable for spawn/send/list/history/clear/config/metrics.
func daemonCall(sock, cmd string, extra map[string]any) (map[string]any, error) {
	c, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	req := map[string]any{"cmd": cmd}
	for k, v := range extra {
		req[k] = v
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		return nil, err
	}

	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if ok, _ := resp["ok"].(bool); !ok {
		errStr, _ := resp["error"].(string)
		return resp, fmt.Errorf("daemon: %s", errStr)
	}
	return resp, nil
}

// daemonSubscribe opens a long-lived conn, sends `subscribe`, waits for the
// {"ok":true,"subscribed":group} ack, then returns the conn. Caller reads
// JSON event lines until close.
func daemonSubscribe(sock, group string) (net.Conn, *bufio.Reader, error) {
	c, err := net.Dial("unix", sock)
	if err != nil {
		return nil, nil, err
	}
	req := map[string]any{"cmd": "subscribe", "group": group}
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		c.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	var ack map[string]any
	if err := json.Unmarshal(line, &ack); err != nil {
		c.Close()
		return nil, nil, err
	}
	if ok, _ := ack["ok"].(bool); !ok {
		c.Close()
		errStr, _ := ack["error"].(string)
		return nil, nil, fmt.Errorf("subscribe %s: %s", group, errStr)
	}
	return c, br, nil
}
