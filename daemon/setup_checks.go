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
	group  string // section heading in the report; "" = the general group
}

// inGroup labels a run of results so the report can separate what is needed to
// BUILD koto from what the installed daemon needs at RUNTIME — the distinction
// that matters now that podman is a build-time dependency only.
func inGroup(g string, rs []checkResult) []checkResult {
	for i := range rs {
		rs[i].group = g
	}
	return rs
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
	out = append(out, checkUserns(), checkSubuid())
	out = append(out, inGroup("build-time tools (podman or docker; not needed to RUN koto)",
		append(checkContainerEngine(), checkBuildTools()...))...)
	out = append(out, inGroup("runtime tools (the installed daemon shells out to these)", checkRuntimeTools())...)
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

// containerInfo is the slice of `<engine> info` we care about. Both podman
// and docker emit this shape, though docker leaves the podman-only fields
// empty — which is fine, they are only used to sharpen the report.
type containerInfo struct {
	Host struct {
		Security struct {
			Rootless bool `json:"rootless"`
		} `json:"security"`
		Pasta struct {
			Executable string `json:"executable"`
		} `json:"pasta"`
	} `json:"host"`
	Version struct {
		Version string `json:"Version"`
	} `json:"version"`
	ServerVersion string `json:"ServerVersion"` // docker
}

// containerEngine returns the build-time container runtime, matching the
// Makefile's preference order: docker if present, else podman. This is a
// BUILD dependency only — nothing koto runs is a container — so a host that
// installs prebuilt binaries needs neither.
func containerEngine() (name, path string) {
	for _, e := range []string{"docker", "podman"} {
		if p, err := exec.LookPath(e); err == nil {
			return e, p
		}
	}
	return "", ""
}

// checkContainerEngine: either engine satisfies the build. The guest rootfs
// build is the one exception — it needs `podman unshare`, which docker cannot
// do — so a docker-only host gets a warning about that step rather than a
// failure, since prebuilt assets make it moot.
func checkContainerEngine() []checkResult {
	engine, path := containerEngine()
	if engine == "" {
		return []checkResult{failCheck("podman/docker", "neither found",
			"koto builds its binaries and guest assets in a container, so one of\n"+
				"them is needed to BUILD (never to run):\n"+installHint("podman", "podman"))}
	}
	out, err := exec.Command(path, "info", "--format", "json").Output()
	if err != nil {
		return []checkResult{failCheck(engine, "`"+engine+" info` failed: "+errText(err),
			engine+" is installed but not working for this user. Try `"+engine+" info`\nand fix what it reports (often subuid/subgid, see below).")}
	}
	var info containerInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return []checkResult{warnCheck(engine, "installed (could not parse `"+engine+" info`)", "")}
	}
	ver := info.Version.Version
	if ver == "" {
		ver = info.ServerVersion
	}
	res := []checkResult{okCheck(engine, "version "+ver)}

	if engine == "docker" {
		// Nothing further to check: every build step works under docker,
		// including the guest rootfs (build-rootfs.sh does its
		// ownership-sensitive stage inside a container rather than under
		// `podman unshare`, which is what used to make that step podman-only).
		return res
	}
	// podman-specific health below. These are warnings, not failures: they
	// describe how well the BUILD will go, and nothing koto runs is a
	// container.
	if info.Host.Security.Rootless {
		res = append(res, okCheck("podman rootless", "yes"))
	} else {
		res = append(res, warnCheck("podman rootless", "running as root",
			"koto's builds assume rootless podman; running as root means the rootfs\nbuild writes root-owned assets."))
	}
	if info.Host.Pasta.Executable != "" {
		res = append(res, okCheck("pasta", info.Host.Pasta.Executable))
	} else {
		res = append(res, warnCheck("pasta", "not found",
			"podman's rootless network backend. Only the containerized BUILD steps\n"+
				"need it — koto itself runs no containers.\n"+installHint("passt", "passt")))
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

// checkBuildTools: what building koto and its guest assets needs. podman is
// checked separately (checkPodman). openssl and jq are deliberately absent —
// the Go PKI (pki.go) replaced them.
func checkBuildTools() []checkResult {
	return lookupAll([]toolReq{
		{bin: "git", why: "pins the guest kernel revision", fedora: "git", debian: "git"},
		{bin: "make", why: "drives the image and asset builds", fedora: "make", debian: "make"},
		{bin: "curl", why: "downloads the Firecracker release", fedora: "curl", debian: "curl"},
	})
}

// checkRuntimeTools: what the DAEMON shells out to once installed. These are
// host requirements even for a binary release that never builds anything —
// podman is not among them, which is the point of keeping it build-only.
// (Verified against the call sites: proxy.go refresh(), fc.go workspace image
// creation/growth and skills delivery, plus the jailer's id mapping.)
func checkRuntimeTools() []checkResult {
	out := lookupAll([]toolReq{
		{bin: "mkfs.ext4", why: "creates each group's workspace image", fedora: "e2fsprogs", debian: "e2fsprogs"},
		{bin: "e2fsck", why: "checks a workspace image before growing it", fedora: "e2fsprogs", debian: "e2fsprogs"},
		{bin: "resize2fs", why: "grows a workspace image when the size preset increases", fedora: "e2fsprogs", debian: "e2fsprogs"},
		{bin: "tar", why: "delivers skills into a guest", fedora: "tar", debian: "tar"},
		// The jailer maps a distinct uid per VM out of the host's subuid
		// range, which needs these setuid-capability helpers — they carry
		// cap_setuid/cap_setgid, so nothing here runs as root.
		{bin: "newuidmap", why: "maps the per-VM uid the microVM monitor is jailed under", fedora: "shadow-utils", debian: "uidmap"},
		{bin: "newgidmap", why: "maps the per-VM gid the microVM monitor is jailed under", fedora: "shadow-utils", debian: "uidmap"},
	})
	// claude is only reachable for OAuth: the proxy shells out to it to
	// refresh a subscription token (proxy.go). An API-key install never calls
	// it, so a missing claude is a warning rather than a hard stop.
	if p, err := exec.LookPath("claude"); err == nil {
		out = append(out, okCheck("claude", p))
	} else {
		out = append(out, warnCheck("claude", "not found",
			"Needed only for Claude-subscription (OAuth) auth, where the proxy shells\n"+
				"out to it to refresh the token. Not needed if you authenticate with an\n"+
				"API key. Install it with:\n  npm i -g @anthropic-ai/claude-code"))
	}
	return out
}

type toolReq struct{ bin, why, fedora, debian string }

func lookupAll(reqs []toolReq) []checkResult {
	var out []checkResult
	for _, t := range reqs {
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

// preflightGate runs every host check, prints the report with remediation, and
// decides whether it is safe to continue. Shared by the wizard's first step
// and by `koto install`, so a direct install cannot skip the checks the
// guided path enforces — which it silently did until this existed.
//
// Returns noKVM=true when the operator chose to proceed without KVM: the
// daemon installs and the API answers, but no group can boot.
func preflightGate(u *setupUI) (noKVM bool, err error) {
	var hard, kvm []checkResult
	group := ""
	for _, c := range runPreflight() {
		if c.group != group {
			group = c.group
			if group != "" {
				u.printf("%s", u.dim("  "+group))
			}
		}
		switch {
		case c.ok && !c.warn:
			u.ok("%-18s %s", c.name, u.dim(c.detail))
		case c.warn:
			u.warn("%-18s %s", c.name, c.detail)
			if c.remedy != "" {
				u.hint(c.remedy)
			}
		default:
			u.fail("%-18s %s", c.name, c.detail)
			if c.remedy != "" {
				u.hint(c.remedy)
			}
			if c.name == "/dev/kvm" {
				kvm = append(kvm, c)
			} else {
				hard = append(hard, c)
			}
		}
	}
	if len(hard) > 0 {
		return false, fmt.Errorf("%d unmet requirement(s) — see the remediation above", len(hard))
	}
	if len(kvm) > 0 {
		u.blank()
		u.prose(`KVM is the one requirement you can proceed without, in a limited way: the
daemon will install and run, and the API will answer, but no group can
actually boot until KVM is available.`)
		if !u.yesno("Continue without KVM?", false) {
			return false, errSetupAborted
		}
		return true, nil
	}
	return false, nil
}
