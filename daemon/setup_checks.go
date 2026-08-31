package main

// Host dependency probes for `koto setup` (and its --check doctor mode).
// Everything here is read-only and pure Go: no shelling out to a script the
// user then has to read. Each probe returns what it found plus, on failure,
// the remediation the operator would otherwise have to derive from the README.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// hostDistro reads /etc/os-release once: "debian" for Debian/Ubuntu, "fedora"
// for the RPM family, "" when we can't tell. Only used to phrase remediation —
// nothing branches on it behaviorally, so an unknown distro degrades to
// showing both package managers rather than to a wrong guess.
var hostDistro = func() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	txt := strings.ToLower(string(b))
	for _, line := range strings.Split(txt, "\n") {
		v := strings.Trim(strings.TrimPrefix(strings.TrimPrefix(line, "id="), "id_like="), `"`)
		if !strings.HasPrefix(line, "id=") && !strings.HasPrefix(line, "id_like=") {
			continue
		}
		for _, f := range strings.Fields(v) {
			switch f {
			case "debian", "ubuntu":
				return "debian"
			case "fedora", "rhel", "centos":
				return "fedora"
			}
		}
	}
	return ""
}()

// installHint renders "install this package" for the host's package manager.
// Package names are per-family because they genuinely differ (the id-mapping
// helpers are shadow-utils on Fedora, a separate uidmap package on Ubuntu).
func installHint(fedoraPkg, debianPkg string) string {
	switch hostDistro {
	case "debian":
		return "  sudo apt install " + debianPkg
	case "fedora":
		return "  sudo dnf install " + fedoraPkg
	}
	return "  sudo dnf install " + fedoraPkg + "      (Fedora/RHEL)\n  sudo apt install " + debianPkg + "      (Debian/Ubuntu)"
}

type checkResult struct {
	name   string
	ok     bool
	warn   bool // satisfied enough to continue, but the operator should know
	detail string
	remedy string
}

func okCheck(name, detail string) checkResult {
	return checkResult{name: name, ok: true, detail: detail}
}
func failCheck(name, detail, remedy string) checkResult {
	return checkResult{name: name, detail: detail, remedy: remedy}
}
func warnCheck(name, detail, remedy string) checkResult {
	return checkResult{name: name, ok: true, warn: true, detail: detail, remedy: remedy}
}

// runPreflight probes every host requirement. The KVM check is reported like
// any other but treated as optional by the caller: without it the daemon runs
// and the API answers, only group microVMs can't boot.
func runPreflight() []checkResult {
	var out []checkResult
	out = append(out, checkPlatform(), checkKVM())
	out = append(out, checkPodman()...)
	out = append(out, checkUserns(), checkSubuid())
	out = append(out, checkTools()...)
	out = append(out, checkDisk(), checkNetwork())
	return out
}

func checkPlatform() checkResult {
	got := runtime.GOOS + "/" + runtime.GOARCH
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return failCheck("platform", got,
			"koto needs linux/amd64: Firecracker microVMs need KVM, and the guest\nkernel and rootfs are built for x86_64.")
	}
	return okCheck("platform", got)
}

// checkKVM tests the device the way the VMM will use it, which is NOT the same
// as whether we can open it here.
//
// The process that actually opens /dev/kvm is the jailed Firecracker VMM: it
// runs as an unprivileged per-VM uid inside the container's user namespace
// with a deliberately EMPTY supplementary group set (fcjail.go). So it matches
// neither the device's owner (root) nor its group (kvm) — host group
// membership doesn't reach it, and neither does `--group-add keep-groups`.
// The world bits are the only thing that can grant it access.
//
// Fedora ships /dev/kvm 0666, so this is invisible there. Ubuntu ships
// 0660 root:kvm, where the host user can be in the kvm group, open the device
// fine, pass a naive check — and then every single microVM fails to boot with
// a bare EACCES buried in a Firecracker console log. Check the mode.
func checkKVM() checkResult { return checkKVMAt("/dev/kvm") }

func checkKVMAt(path string) checkResult {
	fi, err := os.Stat(path)
	if err != nil {
		return failCheck("/dev/kvm", "not present",
			"No KVM on this host. Enable virtualization in the BIOS/UEFI, or (in a VM)\nenable nested virtualization. Groups cannot boot without it.")
	}
	mode := fi.Mode().Perm()
	if mode&0o006 != 0o006 {
		return failCheck("/dev/kvm", fmt.Sprintf("mode %04o — not world-accessible", mode),
			"The jailed microVM monitor runs as an unprivileged id with no groups, so\n"+
				"it needs the world bits on /dev/kvm; being in the kvm group is not enough.\n"+
				"Grant them persistently with a udev rule (this is Fedora's default):\n"+
				`  echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666"' | sudo tee /etc/udev/rules.d/99-kvm.rules`+"\n"+
				"  sudo udevadm control --reload-rules && sudo udevadm trigger --name-match=kvm")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return failCheck("/dev/kvm", "present but not usable: "+err.Error(),
			"Add yourself to the kvm group and re-login:\n  sudo usermod -aG kvm $USER")
	}
	f.Close()
	return okCheck("/dev/kvm", fmt.Sprintf("usable (mode %04o)", mode))
}

// podmanInfo is the slice of `podman info` we care about.
type podmanInfo struct {
	Host struct {
		Security struct {
			Rootless bool `json:"rootless"`
		} `json:"security"`
		Pasta struct {
			Executable string `json:"executable"`
		} `json:"pasta"`
		Version string `json:"-"`
	} `json:"host"`
	Version struct {
		Version string `json:"Version"`
	} `json:"version"`
}

func checkPodman() []checkResult {
	if _, err := exec.LookPath("podman"); err != nil {
		return []checkResult{failCheck("podman", "not found",
			"Install podman:\n"+installHint("podman", "podman"))}
	}
	out, err := exec.Command("podman", "info", "--format", "json").Output()
	if err != nil {
		return []checkResult{failCheck("podman", "`podman info` failed: "+errText(err),
			"podman is installed but not working for this user. Try `podman info` and\nfix what it reports (often subuid/subgid, see below).")}
	}
	var info podmanInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return []checkResult{warnCheck("podman", "installed (could not parse `podman info`)", "")}
	}
	res := []checkResult{okCheck("podman", "version "+info.Version.Version)}
	if info.Host.Security.Rootless {
		res = append(res, okCheck("podman rootless", "yes"))
	} else {
		res = append(res, warnCheck("podman rootless", "running as root",
			"koto is designed for rootless podman; running as root widens the blast\nradius of a compromise well beyond what the trust model assumes."))
	}
	if info.Host.Pasta.Executable != "" {
		res = append(res, okCheck("pasta", info.Host.Pasta.Executable))
	} else {
		res = append(res, failCheck("pasta", "not found",
			"Install passt/pasta — the rootless network backend koto assumes:\n"+
				installHint("passt", "passt")))
	}
	return res
}

// checkUserns covers the nested-userns requirement: the Firecracker VMM jail
// (fcjail.go) re-execs into a fresh user namespace from inside a rootless
// container, which some distros disable outright.
func checkUserns() checkResult {
	b, err := os.ReadFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		return warnCheck("nested userns", "unknown", "")
	}
	if strings.TrimSpace(string(b)) == "0" {
		return failCheck("nested userns", "user.max_user_namespaces = 0",
			"Nested user namespaces are disabled; the VMM jailer cannot start.\n  sudo sysctl -w user.max_user_namespaces=15000\nand persist it in /etc/sysctl.d/.")
	}
	if r, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); err == nil &&
		strings.TrimSpace(string(r)) == "1" {
		return failCheck("nested userns", "restricted by AppArmor",
			"Ubuntu 23.10+ blocks unprivileged user namespaces, which the microVM\n"+
				"monitor's jail needs. Allow them, and persist it across reboots:\n"+
				"  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0\n"+
				"  echo 'kernel.apparmor_restrict_unprivileged_userns=0' | sudo tee /etc/sysctl.d/60-koto.conf\n"+
				"Running the VMM unjailed (KOTO_FC_NOJAIL=1) also avoids the restriction,\n"+
				"but drops a layer of host protection — prefer the sysctl.")
	}
	return okCheck("nested userns", "allowed ("+strings.TrimSpace(string(b))+")")
}

func checkSubuid() checkResult {
	u, err := user.Current()
	if err != nil {
		return warnCheck("subuid/subgid", "cannot determine current user", "")
	}
	has := func(path string) bool {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, u.Username+":") || strings.HasPrefix(line, u.Uid+":") {
				return true
			}
		}
		return false
	}
	if has("/etc/subuid") && has("/etc/subgid") {
		return okCheck("subuid/subgid", "ranges allocated for "+u.Username)
	}
	return failCheck("subuid/subgid", "no range for "+u.Username,
		"Rootless podman needs id ranges:\n  sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 "+u.Username+"\n  podman system migrate")
}

// checkTools: the host binaries the build pipeline shells out to. openssl and
// jq are deliberately absent — the Go PKI (pki.go) replaced them.
// mkfs.ext4 and newuidmap live in /usr/sbin, which is on PATH for a login
// shell on both families but not under a stripped PATH.
func checkTools() []checkResult {
	var out []checkResult
	for _, t := range []struct{ bin, why, fedora, debian string }{
		{"mkfs.ext4", "builds the guest rootfs image", "e2fsprogs", "e2fsprogs"},
		{"curl", "downloads the Firecracker release", "curl", "curl"},
		{"tar", "unpacks release archives", "tar", "tar"},
		{"git", "pins the guest kernel revision", "git", "git"},
		{"make", "drives the image builds", "make", "make"},
		// newuidmap/newgidmap: shadow-utils on Fedora, which is always
		// installed, so this never fails there. On Ubuntu it is the separate
		// uidmap package that podman only *recommends* — a minimal image can
		// have podman, valid /etc/subuid ranges, and still no rootless.
		{"newuidmap", "maps the uid ranges rootless podman runs in", "shadow-utils", "uidmap"},
	} {
		if p, err := exec.LookPath(t.bin); err == nil {
			out = append(out, okCheck(t.bin, p))
		} else {
			out = append(out, failCheck(t.bin, "not found",
				fmt.Sprintf("%s %s. Install it:\n%s", t.bin, t.why, installHint(t.fedora, t.debian))))
		}
	}
	return out
}

// checkDisk warns below 20 GiB: the kernel build cache alone is a few GiB,
// assets ~800 MB, images a couple of GB, and every group workspace is 8–24 GiB.
func checkDisk() checkResult {
	var st unix.Statfs_t
	wd, _ := os.Getwd()
	if err := unix.Statfs(wd, &st); err != nil {
		return warnCheck("disk space", "unknown", "")
	}
	freeGiB := float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
	detail := fmt.Sprintf("%.1f GiB free on %s", freeGiB, wd)
	if freeGiB < 20 {
		return warnCheck("disk space", detail,
			"Under 20 GiB free. The kernel build cache, the guest assets and the\nimages need roughly that much before a single group workspace exists.")
	}
	return okCheck("disk space", detail)
}

func checkNetwork() checkResult {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Head("https://github.com")
	if err != nil {
		return failCheck("network", errText(err),
			"The build downloads the Firecracker release, the guest kernel source and\nnpm packages. An offline install is not supported.")
	}
	resp.Body.Close()
	return okCheck("network", "reachable")
}

func errText(err error) string {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}
