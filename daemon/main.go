package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: koto daemon")
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
	case "ctl":
		// Host-side CLI client for the daemon's gRPC API — the surface
		// coding agents drive. Same binary, dials the running daemon; auth
		// and authorization are the mTLS + bearer token + role ACL every
		// other client goes through.
		ctlCliMain(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		os.Exit(2)
	}
}
