package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: clawson <daemon|proxy>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "daemon":
		daemonMain()
	case "proxy":
		proxyMain()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		os.Exit(2)
	}
}
