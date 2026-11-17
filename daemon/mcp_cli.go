package main

// `koto ctl mcp` — the operator CLI over the admin-only Mcp RPC.
//
// The upstream auth header never goes on argv (it would sit in ps output
// and shell history): --auth-file reads one "Header-Name: value" line from
// a file, or from stdin with "-".

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"koto-protocol/pb"
)

const mcpUsage = `usage: koto ctl mcp ls
       koto ctl mcp show <conn>
       koto ctl mcp add <conn> --url <https://...> --tools a,b [--class read|write]
                    [--rules <file.json>] [--auth-file <file>|-]
       koto ctl mcp rm <conn>
       koto ctl mcp allow <conn> <group>        whitelist a group (no tools yet)
       koto ctl mcp deny  <conn> <group>        remove it and its grants
       koto ctl mcp grant  <conn> <group> <tool> [--args '<json>']
       koto ctl mcp revoke <conn> <group> <tool>
       koto ctl mcp rate <conn> <group> <per_minute>   (0 clears)
       koto ctl mcp effective <group> [<conn>]

  effective = connection ceiling ∩ group grant; a grant never widens it.
  --rules: {"<tool>":{"args":{"<arg>":{"type":"string","maxLength":64}}}}
  --auth-file: one line "Header-Name: value"; the value is never shown.
  admin-only: the daemon refuses any other role.
`

// mcpCliParse turns argv into a request (pure apart from reading the
// --rules/--auth-file inputs through readFile; tested).
func mcpCliParse(args []string, readFile func(string) ([]byte, error)) (*pb.McpReq, error) {
	var pos []string
	req := &pb.McpReq{}
	var rulesFile, authFile string
	needVal := func(i int, flag string) (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", flag)
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		flag, val, hasEq := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		if a == "--help" {
			return nil, errors.New("help")
		}
		if !hasEq {
			v, err := needVal(i, flag)
			if err != nil {
				return nil, err
			}
			val = v
			i++
		}
		switch flag {
		case "--url":
			req.Url = val
		case "--class":
			req.Cls = val
		case "--tools":
			for _, t := range strings.Split(val, ",") {
				if t = strings.TrimSpace(t); t != "" {
					req.Tools = append(req.Tools, t)
				}
			}
		case "--rules":
			rulesFile = val
		case "--auth-file":
			authFile = val
		case "--args":
			req.RuleJson = val
		default:
			return nil, fmt.Errorf("unknown flag %s", flag)
		}
	}
	if len(pos) == 0 {
		pos = []string{"ls"}
	}
	req.Op = pos[0]
	want := map[string][]*string{
		"ls":        {},
		"show":      {&req.Conn},
		"add":       {&req.Conn},
		"rm":        {&req.Conn},
		"allow":     {&req.Conn, &req.Group},
		"deny":      {&req.Conn, &req.Group},
		"grant":     {&req.Conn, &req.Group, &req.Tool},
		"revoke":    {&req.Conn, &req.Group, &req.Tool},
		"rate":      {&req.Conn, &req.Group, nil},
		"effective": {&req.Group, &req.Conn},
		"help":      nil,
	}
	slots, ok := want[req.Op]
	if !ok || req.Op == "help" {
		if req.Op == "help" {
			return nil, errors.New("help")
		}
		return nil, fmt.Errorf("unknown mcp op %q", req.Op)
	}
	rest := pos[1:]
	optional := req.Op == "effective" // conn is optional there
	if len(rest) > len(slots) || (len(rest) < len(slots) && !(optional && len(rest) == len(slots)-1)) {
		return nil, fmt.Errorf("mcp %s: wrong number of arguments", req.Op)
	}
	for i, v := range rest {
		if slots[i] == nil { // rate's number
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return nil, errors.New("per_minute must be a non-negative integer")
			}
			req.PerMinute = int32(n)
			continue
		}
		*slots[i] = v
	}
	if req.Op != "add" && (req.Url != "" || len(req.Tools) > 0 || rulesFile != "" || authFile != "" || req.Cls != "") {
		return nil, fmt.Errorf("--url/--tools/--class/--rules/--auth-file only apply to mcp add")
	}
	if req.Op != "grant" && req.RuleJson != "" {
		return nil, errors.New("--args only applies to mcp grant")
	}
	if req.Op == "add" {
		if req.Url == "" {
			return nil, errors.New("mcp add needs --url")
		}
		if rulesFile != "" {
			b, err := readFile(rulesFile)
			if err != nil {
				return nil, fmt.Errorf("--rules: %v", err)
			}
			req.RulesJson = string(b)
		}
		if authFile != "" {
			b, err := readFile(authFile)
			if err != nil {
				return nil, fmt.Errorf("--auth-file: %v", err)
			}
			line := strings.TrimRight(strings.SplitN(string(b), "\n", 2)[0], "\r")
			name, value, ok := strings.Cut(line, ":")
			name, value = strings.TrimSpace(name), strings.TrimSpace(value)
			if !ok || name == "" || value == "" {
				return nil, errors.New(`--auth-file must hold one line "Header-Name: value"`)
			}
			req.AuthName, req.AuthValue = name, value
		}
	}
	return req, nil
}

func mcpReadInput(p string) ([]byte, error) {
	if p == "-" {
		return io.ReadAll(io.LimitReader(bufio.NewReader(os.Stdin), 1<<20))
	}
	return os.ReadFile(p)
}

func mcpCliMain(args []string) {
	req, err := mcpCliParse(args, mcpReadInput)
	if err != nil {
		fmt.Fprint(os.Stderr, mcpUsage)
		if err.Error() == "help" {
			os.Exit(0)
		}
		ctlFatal(2, "%v", err)
	}
	cl := ctlClient()
	ctx, cancel := ctlCtx()
	defer cancel()
	resp, err := cl.Mcp(ctx, req)
	ctlPrint(resp, err)
}
