package main

import (
	"fmt"
	"os"
)

// kotoVersion is stamped by the build (-ldflags "-X main.kotoVersion=...");
// "dev" means an unstamped `go build`/`go run` from the clone.
var kotoVersion = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: koto daemon|ctl|setup|install|tui|pki|version")
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
	case "setup":
		// First-run wizard: host checks, image + guest-asset builds, PKI,
		// credentials, install, smoke test. Safe to re-run — every step
		// detects whether it is already satisfied.
		setupMain(os.Args[2:])
	case "install":
		// Install (or upgrade) koto as a systemd service backed by a state
		// directory, so it survives a reboot and outlives this clone.
		installMain(os.Args[2:])
	case "tui":
		// Attach the terminal UI to an installed daemon (the `make tui`
		// counterpart for a non-clone install).
		tuiMain(os.Args[2:])
	case "userns-check":
		// Verify this host can give the jailer what it needs (ns-root over a
		// subuid range) without a container. Also the integration test's
		// entry point.
		usernsProbeMain()
	case "pki":
		// Private CA / server cert / client identities, in Go — the
		// openssl-free counterpart of the Makefile's pki-* targets.
		pkiCliMain(os.Args[2:])
	case "version":
		fmt.Println(kotoVersion)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		os.Exit(2)
	}
}
