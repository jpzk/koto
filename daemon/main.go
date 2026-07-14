package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: clawson daemon")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "daemon":
		daemonMain()
	case "fcjail":
		// Internal: the re-exec'd Firecracker jailer shim. Runs in fresh
		// namespaces created by fcJailCommand, sets up the chroot, drops
		// privilege, and execs Firecracker. Never returns on success.
		fcjailMain()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		os.Exit(2)
	}
}
