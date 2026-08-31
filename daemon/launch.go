package main

// `koto launch` — the installed-mode counterpart of host/run-host.sh. The
// systemd unit's ExecStart calls it; it computes the podman arguments (which
// are conditional in ways an ExecStart line cannot express: /dev/kvm only when
// present, --cpus only when capped, -p only when published) and then EXECs
// podman rather than spawning it.
//
// The exec matters. Because podman replaces this process, podman is the unit's
// main process, so `--sdnotify=conmon` satisfies Type=notify and systemd's
// SIGTERM reaches podman, which forwards it to the daemon as PID 1 in the
// container, which stops every microVM so each guest sync+umounts its
// workspace image. A spawn-and-wait wrapper would break that chain and leave
// dirty workspace images on every reboot.
//
// Dev mode still uses run-host.sh: it bind-mounts the source tree for the
// live-rebuild loop, which an installed service deliberately does not do.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
)

const (
	installedContainerName = "koto"
	defaultStateDir        = "/var/lib/koto"
	defaultImage           = "localhost/koto:latest"
	envFilePath            = "/etc/koto/koto.env"
	unitPath               = "/etc/systemd/system/koto.service"
)

func launchMain(args []string) {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: koto launch   (internal: systemd ExecStart)")
		os.Exit(2)
	}
	podman, err := exec.LookPath("podman")
	if err != nil {
		ctlFatal(1, "podman not found: %v", err)
	}
	home := envOr("KOTO_HOME", defaultStateDir)
	image := envOr("KOTO_IMAGE", defaultImage)

	argv := []string{"podman", "run", "--rm", "--replace",
		"--name", installedContainerName,
		"--network", "koto-net",
		"--sdnotify=conmon",
		"--security-opt", "label=disable",
		// The host view of the cgroup tree, so the daemon can place each VMM
		// in its own leaf (fccgroup.go); it degrades to cgroup=off without.
		"--cgroupns=host", "-v", "/sys/fs/cgroup:/sys/fs/cgroup:rw",
		// Matching-path mount: the daemon builds absolute paths for the VMM's
		// jail and asset arguments, so the state dir must exist at the same
		// path inside and out.
		"-v", home + ":" + home,
		// The proxy's OAuth refresh runs `claude`, which reads
		// ~/.claude/.credentials.json — CRED_PATH's default.
		"-v", filepath.Join(home, "creds") + ":/root/.claude",
		"-v", "/etc/localtime:/etc/localtime:ro",
		"-w", home,
		"-e", "KOTO_HOME=" + home,
	}
	if _, err := os.Stat("/dev/kvm"); err == nil {
		// keep-groups carries our supplementary groups past the userns mapping,
		// so a host user in the kvm group keeps it inside the container. It is
		// NOT sufficient on its own where /dev/kvm is 0660 root:kvm (Ubuntu):
		// the process that actually opens the device is the jailed VMM, which
		// drops to a per-VM uid with an EMPTY group set (fcjail.go), so it
		// matches neither owner nor group and needs the world bits. That is a
		// host-policy fix (udev, see checkKVM's remediation), not something we
		// can set from here — this flag only covers the daemon itself and the
		// KOTO_FC_NOJAIL=1 path.
		argv = append(argv, "--device", "/dev/kvm", "--group-add", "keep-groups")
	}
	if cpus := hostCPUs(); cpus != "" {
		argv = append(argv, "--cpus", cpus)
	}
	if pub := os.Getenv("KOTO_PUBLISH"); pub != "" {
		port := envOr("KOTO_PORT", "8443")
		argv = append(argv, "-p", pub+":"+port+":"+port)
	}
	for _, k := range []string{"KOTO_BIND", "KOTO_PORT", "KOTO_HOST_MEM_MIB", "ANTHROPIC_API_KEY", "TERM"} {
		if v := os.Getenv(k); v != "" {
			argv = append(argv, "-e", k+"="+v)
		}
	}
	argv = append(argv, image)

	if err := syscall.Exec(podman, argv, os.Environ()); err != nil {
		ctlFatal(1, "exec podman: %v", err)
	}
}

// hostCPUs mirrors run-host.sh: default nproc-1 so the host stays responsive
// whatever the fleet does; "0" means unlimited (no --cpus argument).
func hostCPUs() string {
	v := os.Getenv("KOTO_HOST_CPUS")
	if v == "0" {
		return ""
	}
	if v != "" {
		return v
	}
	n := runtime.NumCPU() - 1
	if n < 1 {
		n = 1
	}
	return strconv.Itoa(n)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
