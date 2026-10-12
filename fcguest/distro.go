package main

import (
	"bufio"
	"os"
	"regexp"
	"strings"
)

// Distro-neutral helpers. The guest root drive is one of several golden
// rootfs flavors (fedora by default; e.g. kali, built from
// Dockerfile.rootfs.<flavor>), and fc-agent is the same binary in all of them.

// scriptShell is the interpreter for daemon-issued scripts (execAsWorker,
// run_script). The daemon writes those scripts for bash — arrays, `read -N`,
// ${var//pat/} — and on Fedora /bin/sh IS bash, which is how that went
// unnoticed. Debian-family images (kali) link /bin/sh to dash, where the same
// scripts fail to parse. Prefer bash wherever it exists; fall back to /bin/sh
// so an image without bash still gets a shell.
func scriptShell() string {
	if fi, err := os.Stat("/bin/bash"); err == nil && fi.Mode().IsRegular() {
		return "/bin/bash"
	}
	return "/bin/sh"
}

var osIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// osReleaseID is ID= from /etc/os-release ("fedora", "kali", ...), or "" when
// it can't be read or isn't a plain token.
func osReleaseID() string {
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if v, ok := strings.CutPrefix(line, "ID="); ok {
				v = strings.ToLower(strings.Trim(v, `"'`))
				if osIDRE.MatchString(v) {
					return v
				}
				return ""
			}
		}
	}
	return ""
}

// rootOverlayDirName names the root=yes overlay tree on the workspace disk.
// The upper layers hold package-manager state (rpm db, dpkg status, /etc)
// written ON TOP OF one specific golden image; laid over a different distro
// they would shadow its /usr and /etc with foreign files and break it. So
// each distro gets its own tree. Fedora — and anything unidentifiable — keeps
// the historical ".rootovl", so existing groups keep their installs and an
// older fc-agent (which only knows ".rootovl") agrees with this one.
func rootOverlayDirName(osID string) string {
	if osID == "" || osID == "fedora" {
		return ".rootovl"
	}
	return ".rootovl-" + osID
}
