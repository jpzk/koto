// Command otlp-recv is the OTLP/gRPC receiver behind scripts/e2e-otel.sh.
// It is test tooling and is never shipped with koto.
//
//	otlp-recv serve -addr 127.0.0.1:4317 -out capture.jsonl
//	    receive traces/metrics/logs until SIGTERM/SIGINT, appending one
//	    JSON line per record to -out (Ready line on stdout once listening)
//	otlp-recv check -in capture.jsonl -group G -marker M
//	    print one PASS/FAIL line per check of that group's turn; exit 1 on
//	    any FAIL (and on an unreadable or corrupt capture)
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"koto/otlprecv"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "check":
		os.Exit(check(os.Args[2:]))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: otlp-recv serve -addr A -out FILE | check -in FILE -group G -marker M")
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:4317", "listen address (loopback)")
	out := fs.String("out", "", "JSON-lines capture file")
	_ = fs.Parse(args)
	if *out == "" {
		usage()
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "otlp-recv:", err)
		os.Exit(1)
	}
	r, err := otlprecv.Start(*addr, f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "otlp-recv:", err)
		os.Exit(1)
	}
	fmt.Printf("otlp-recv: listening on %s, capture %s\n", r.Addr, *out)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	r.Stop()
	_ = f.Sync()
	_ = f.Close()
}

func check(args []string) int {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	in := fs.String("in", "", "capture file written by serve")
	group := fs.String("group", "", "group whose turn is checked")
	marker := fs.String("marker", "", "string the turn's output contains")
	_ = fs.Parse(args)
	if *in == "" || *group == "" || *marker == "" {
		usage()
	}
	f, err := os.Open(*in)
	if err != nil {
		fmt.Printf("  FAIL  read capture: %v\n", err)
		return 1
	}
	defer f.Close()
	c, err := otlprecv.ReadCapture(f)
	if err != nil {
		fmt.Printf("  FAIL  parse capture: %v\n", err)
		return 1
	}
	fmt.Printf("  capture: %d spans, %d metric points, %d logs\n", len(c.Spans), len(c.Points), len(c.Logs))
	fail := 0
	for _, r := range otlprecv.CheckTurn(c, *group, *marker) {
		st := "PASS"
		if !r.OK {
			st = "FAIL"
			fail++
		}
		fmt.Printf("  %s  %s  (%s)\n", st, r.Name, r.Detail)
	}
	if fail > 0 {
		return 1
	}
	return 0
}
