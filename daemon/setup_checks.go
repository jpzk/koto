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

// checkKVM opens /dev/kvm read-write — existence isn't enough, the invoking
// user must be able to use it (usually via the kvm group).
func checkKVM() checkResult {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return failCheck("/dev/kvm", "not present",
			"No KVM on this host. Enable virtualization in the BIOS/UEFI, or (in a VM)\nenable nested virtualization. Groups cannot boot without it.")
	}
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return failCheck("/dev/kvm", "present but not usable: "+err.Error(),
			"Add yourself to the kvm group and re-login:\n  sudo usermod -aG kvm $USER")
	}
	f.Close()
	return okCheck("/dev/kvm", "usable")
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
			"Install podman:\n  sudo dnf install podman      (Fedora/RHEL)\n  sudo apt install podman      (Debian/Ubuntu)")}
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
			"Install passt/pasta — the rootless network backend koto assumes:\n  sudo dnf install passt"))
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
			"Ubuntu's AppArmor blocks unprivileged user namespaces:\n  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0")
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
func checkTools() []checkResult {
	var out []checkResult
	for _, t := range []struct{ bin, why, pkg string }{
		{"mkfs.ext4", "builds the guest rootfs image", "e2fsprogs"},
		{"curl", "downloads the Firecracker release", "curl"},
		{"tar", "unpacks release archives", "tar"},
		{"git", "pins the guest kernel revision", "git"},
		{"make", "drives the image builds", "make"},
	} {
		if p, err := exec.LookPath(t.bin); err == nil {
			out = append(out, okCheck(t.bin, p))
		} else {
			out = append(out, failCheck(t.bin, "not found",
				fmt.Sprintf("%s %s. Install it:\n  sudo dnf install %s", t.bin, t.why, t.pkg)))
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
