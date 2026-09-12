package main

// The AppArmor profile that lets koto create user namespaces on a host where
// Ubuntu has taken them away from everyone.
//
// Ubuntu 23.10+ blocks unprivileged user namespaces by default
// (kernel.apparmor_restrict_unprivileged_userns=1), because handing an
// unprivileged process a full capability set inside a namespace exposes kernel
// surface — mount and filesystem code, netfilter, packet sockets — that
// previously needed real root, and "unprivileged userns → kernel bug in a
// subsystem that assumed root → local privilege escalation" has been one of the
// most productive escalation patterns of recent years. koto needs the feature
// anyway: the VMM jail (fcjail.go) clones into user/mount/pid/net/ipc/uts
// namespaces, and the daemon's own bootstrap (userns.go) needs newuidmap.
//
// There are two ways out and they are not equivalent. The sysctl turns the
// restriction off for EVERY program on the machine. An AppArmor profile grants
// it to this one binary and leaves Ubuntu's protection in place for everything
// else — which is the same capability with none of the reach, so it is what
// koto sets up.
//
// Three things about this profile were established by measurement on Ubuntu
// 24.04.5 and 26.04 LTS (2026-09-12), not by reading documentation:
//
//   - It is ENFORCING, not flags=(unconfined). The remediation koto used to
//     print was unconfined, which grants userns by switching mediation off
//     entirely. A real profile is strictly better and costs nothing here.
//
//   - flags=(attach_disconnected) is LOAD-BEARING and is why the unconfined
//     version looked necessary. fcjail puts each VMM in its own mount
//     namespace, so the guest's vsock socket under run/fc/<g>.jail/vsock/ is a
//     DISCONNECTED PATH from AppArmor's point of view: the name lookup fails
//     before any rule can be consulted, so even a blanket `file,` does not
//     match it. Without the flag the daemon starts, reports healthy, and can
//     never reach a guest — measured, with the audit log showing
//     `operation="connect" ... info="Failed name lookup - disconnected path"`
//     once every 250ms while the group failed to boot.
//
//   - The rules are CLASS-LEVEL on purpose, not a path-by-path policy. This
//     profile's job is to be an allowlist entry for the userns restriction,
//     not to confine koto — the systemd unit already does that far more
//     precisely (ProtectSystem=strict, ProtectHome, DevicePolicy=closed,
//     CapabilityBoundingSet, RestrictNamespaces, RestrictAddressFamilies).
//     Writing a second, weaker confinement in AppArmor's language would
//     duplicate that, and would break on every path koto grows — including the
//     ones this test does not exercise, like the gVisor gateway under
//     network=wan. Being precise about SCOPE (one binary) is the security
//     property worth having; pretending to be precise about behaviour would
//     buy nothing and cost reliability.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// apparmorProfilePath is where the profile lives. The name matches the profile
// name inside it, which is what apparmor_parser and aa-status report.
const apparmorProfilePath = "/etc/apparmor.d/koto"

// installedKotoBin is the binary an AppArmor profile can attach to, and the one
// the daemon actually runs as. A profile naming this path does nothing for a
// koto run out of a clone — which is correct, since the installed binary is the
// one that has to create namespaces.
const installedKotoBin = "/usr/local/bin/koto"

// apparmorUsernsRestricted reports whether this host is an Ubuntu-style one
// that has taken unprivileged user namespaces away. Absence of the sysctl
// (Fedora, Arch, any non-AppArmor distro) reads as "not restricted", which is
// correct: there is nothing for a profile to lift.
func apparmorUsernsRestricted() bool {
	b, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

// apparmorAvailable reports whether a profile can be loaded at all. Without
// apparmor_parser the scoped route is not on the table and the host-wide
// sysctl is the only remedy left to offer.
func apparmorAvailable() bool {
	_, err := exec.LookPath("apparmor_parser")
	return err == nil
}

// apparmorProfileText renders the profile for a koto installed at bin.
//
// The path matters: AppArmor attaches by executable path, so a profile naming
// /usr/local/bin/koto does nothing for a binary run from a clone. That is not a
// flaw — the installed binary is the one that has to create namespaces.
func apparmorProfileText(bin string) string {
	return fmt.Sprintf(`# Managed by `+"`koto install`"+`. Grants unprivileged user namespaces to koto
# alone, so Ubuntu's host-wide restriction can stay on for everything else.
#
# attach_disconnected is required, not cosmetic: the VMM jail gives each
# microVM its own mount namespace, which makes the guest's vsock socket a
# disconnected path. Without this flag the name lookup fails before any rule
# is consulted and the daemon can never reach a guest.
abi <abi/4.0>,
include <tunables/global>

profile koto %s flags=(attach_disconnected) {
  include <abstractions/base>

  # The reason this profile exists.
  userns,

  # Class-level, deliberately: the systemd unit is what confines koto
  # (ProtectSystem=strict, ProtectHome, DevicePolicy=closed,
  # CapabilityBoundingSet, RestrictNamespaces). See daemon/apparmor.go.
  capability,
  file,
  network,
  mount,
  umount,
  pivot_root,
  ptrace,
  signal,
  unix,

  # dbus is not optional and its absence is quiet. koto execs systemctl (the
  # install steps, and installStatus() behind "koto setup --check"), systemctl
  # talks to systemd over D-Bus, and an exec-ed child inherits this profile —
  # so without this rule "systemctl is-active koto" returns an EMPTY string
  # under the profile and the service step reports a healthy daemon as broken.
  # Measured 2026-09-12. Note the audit record says label="koto", not
  # profile="koto", so a denial grep written for file rules does not see it.
  dbus,
}
`, bin)
}

// apparmorProfileWorks answers the only question that matters — can the
// INSTALLED koto actually create a user namespace? — by asking the binary the
// profile names, rather than by inspecting configuration.
//
// Inspection is not available to an unprivileged preflight anyway:
// /sys/kernel/security/apparmor/profiles is root-only and aa-status needs
// root, so "is the profile loaded" cannot be read. Exec'ing the profiled path
// answers it exactly, because the profile attaches at exec — which is also why
// this cannot be answered before the binary is installed.
func apparmorProfileWorks(bin string) bool {
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return false
	}
	cmd := exec.Command(bin, "userns-check")
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return false
	}
}

// installAppArmorProfile writes the profile, loads it, and then VERIFIES by
// running the probe — because apparmor_parser exiting 0 means the policy
// compiled, not that koto can now do what it could not before. Reporting
// success on the parser's word alone would hand the operator a green install
// and a fleet that cannot boot a VM, which is the exact failure mode the
// clone3 breakage already taught this project once.
func installAppArmorProfile(u *setupUI, bin string) error {
	u.info("granting user namespaces to %s alone (Ubuntu's host-wide restriction stays on)", bin)
	if _, err := sudoWriteIfChanged(u, apparmorProfilePath, apparmorProfileText(bin), "0644"); err != nil {
		return fmt.Errorf("write %s: %w", apparmorProfilePath, err)
	}
	if err := sudoRun(u, "apparmor_parser", "-r", apparmorProfilePath); err != nil {
		return fmt.Errorf("apparmor_parser -r %s: %w", apparmorProfilePath, err)
	}
	if !apparmorProfileWorks(bin) {
		return fmt.Errorf("the profile loaded but %s still cannot create a user namespace.\n"+
			"Run `%s userns-check` to see why. The host-wide switch is the fallback:\n"+
			"  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0\n"+
			"  echo 'kernel.apparmor_restrict_unprivileged_userns=0' | sudo tee /etc/sysctl.d/60-koto.conf",
			bin, bin)
	}
	u.ok("%-18s %s", "apparmor", u.dim("profile loaded; nested userns granted to koto only"))
	return nil
}
