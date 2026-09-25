package main

// update.go — `koto update`: check for a newer release, and install it.
//
// It does NOT fetch or verify anything itself. install.sh is the one
// implementation of fetch-and-verify (signed SHA256SUMS against a pinned
// release key, the committed manifest as a second check), `make fetch` already
// reuses it, and this reuses it too — so there is exactly one piece of code
// deciding whether release bytes are trustworthy.
//
// WHICH install.sh is the whole design. It is EMBEDDED in this binary
// (installer/install.sh, kept byte-identical to the repo root's by a test and
// by the Makefile), not downloaded and not read from the state dir:
//   - downloaded from the new release, its pinned key fingerprint would arrive
//     over the same channel as the release it vouches for;
//   - read from the state dir, it would be writable by the daemon (the unit's
//     ReadWritePaths), so a compromised daemon could rewrite the script the
//     operator later runs with sudo — a tier-2 → tier-1 path.
// /usr/local/bin/koto is root-owned, so the trust anchor for an update is the
// release the operator already installed and trusted.
//
// The installer it runs then clones the target release, verifies it, and runs
// THAT release's `koto install` — which stops the daemon (with consent),
// replaces the binaries and any changed guest assets, and starts it again —
// followed by `koto setup`, whose steps all detect as done on an upgrade.
// --no-attach stops it there instead of opening the TUI.

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed installer/install.sh
var embeddedInstaller []byte

// updateExitAvailable is `koto update --check`'s exit code when a newer
// release exists, so a script or timer can act on it without parsing text.
const updateExitAvailable = 3

// installedKotoBinPath is the binary an installed system runs.
const installedKotoBinPath = "/usr/local/bin/koto"

func updateUsage() {
	fmt.Fprintln(os.Stderr, `usage: koto update [--check] [--version X] [-y]

Checks for a newer koto release and installs it. Installing runs the release
installer embedded in this binary (signature and checksum verification), then
the new release's `+"`koto install`"+`, which stops the daemon — every running
microVM — replaces the binaries and guest images, and starts it again.

flags:
  --check      report only: exit 0 when up to date, 3 when an update exists
  --version X  install release X instead of the latest (also allows a downgrade)
  -y           accept the prompts (the daemon stop included)`)
}

func updateMain(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	fs.Usage = updateUsage
	check := fs.Bool("check", false, "report only")
	version := fs.String("version", "", "release to install")
	assumeYes := fs.Bool("y", false, "accept the prompts")
	_ = fs.Parse(args)
	if fs.NArg() != 0 {
		updateUsage()
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	installed := updateInstalledVersion()
	target := *version
	if target == "" {
		latest, err := updateLatestRelease(ctx, http.DefaultClient, updateRepoURL())
		if err != nil {
			updateFatal(1, "cannot determine the latest release: %v", err)
		}
		target = latest
	}

	verdict := updateDecide(installed, target, *version != "")
	fmt.Println(verdict.message)
	if *check {
		if verdict.update {
			os.Exit(updateExitAvailable)
		}
		os.Exit(0)
	}
	if !verdict.update {
		return
	}
	if err := updateRun(target, *assumeYes); err != nil {
		updateFatal(1, "%v", err)
	}
}

func updateFatal(code int, format string, a ...any) {
	fmt.Fprintf(os.Stderr, "koto update: "+format+"\n", a...)
	os.Exit(code)
}

// updateRepoURL mirrors install.sh's KOTO_REPO / KOTO_REPO_URL.
func updateRepoURL() string {
	if u := os.Getenv("KOTO_REPO_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	repo := os.Getenv("KOTO_REPO")
	if repo == "" {
		repo = "jpzk/koto"
	}
	return "https://github.com/" + repo
}

// updateInstalledVersion asks the INSTALLED binary, which is what the service
// runs — not this process, which may be a clone's ./koto. Falls back to this
// binary's own version when nothing is installed at the standard path.
func updateInstalledVersion() string {
	if exists(installedKotoBinPath) {
		out, err := exec.Command(installedKotoBinPath, "version").Output()
		if v := strings.TrimSpace(string(out)); err == nil && v != "" {
			return v
		}
	}
	return kotoVersion
}

// updateLatestRelease resolves the latest release the way install.sh does:
// GitHub answers /releases/latest with a redirect to /releases/tag/<version>,
// and the tag is read off the Location header without following it. No API
// token, no JSON, and the same answer the installer would reach.
func updateLatestRelease(ctx context.Context, base *http.Client, repoURL string) (string, error) {
	c := *base
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if c.Timeout == 0 {
		c.Timeout = 20 * time.Second
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, repoURL+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode/100 != 3 || loc == "" {
		return "", fmt.Errorf("%s/releases/latest answered %s with no redirect — no published release?", repoURL, resp.Status)
	}
	if !strings.Contains(loc, "/releases/tag/") {
		return "", fmt.Errorf("unexpected redirect %q", loc)
	}
	tag := path.Base(strings.TrimRight(loc, "/"))
	if _, ok := parseSemver(tag); !ok {
		return "", fmt.Errorf("latest release tag %q is not a version", tag)
	}
	return tag, nil
}

// updateVerdict is what `koto update` decided, and what it tells the operator.
type updateVerdict struct {
	update  bool
	message string
}

// updateDecide compares the installed version with the target. A development
// build (`1.0.0-6-gd5150df-dirty`) counts as its base release, so a host
// running a build from main is "up to date" against that release rather than
// offered a downgrade to it. A target below the installed version is only
// installed when it was named explicitly.
func updateDecide(installed, target string, explicit bool) updateVerdict {
	iv, iok := parseSemver(installed)
	tv, tok := parseSemver(target)
	switch {
	case !tok:
		return updateVerdict{false, fmt.Sprintf("%q is not a release version", target)}
	case !iok:
		// An installed build with no version (a bare commit hash) cannot be
		// compared; installing a named release is how to get back onto one.
		return updateVerdict{true, fmt.Sprintf("installed %s is not a release build; %s is available", installed, target)}
	}
	dev := installed != iv.String()
	switch c := compareSemver(tv, iv); {
	case c > 0:
		return updateVerdict{true, fmt.Sprintf("koto %s is available (installed: %s)", target, installed)}
	case c == 0 && dev && explicit:
		return updateVerdict{true, fmt.Sprintf("release %s replaces development build %s (named explicitly)", target, installed)}
	case c == 0 && dev:
		return updateVerdict{false, fmt.Sprintf("up to date: development build %s is based on the latest release %s", installed, target)}
	case c == 0:
		return updateVerdict{false, fmt.Sprintf("up to date: koto %s is the latest release", installed)}
	case explicit:
		return updateVerdict{true, fmt.Sprintf("%s is a downgrade from %s (named explicitly)", target, installed)}
	default:
		return updateVerdict{false, fmt.Sprintf("installed %s is newer than the latest release %s; nothing to do", installed, target)}
	}
}

// updateRun writes the embedded installer to a private temp file and runs it
// for the target release, with the terminal handed through: its install and
// setup stages are interactive (the daemon stop is asked there).
func updateRun(target string, assumeYes bool) error {
	if os.Getuid() == 0 {
		return errors.New("run as your normal user, not root — koto installs as you and uses sudo where it must")
	}
	f, err := os.CreateTemp("", "koto-install-*.sh")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(embeddedInstaller); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	args := []string{f.Name(), "--version", target, "--no-attach"}
	if assumeYes {
		args = append(args, "--yes") // → koto install -y, koto setup -y
	}
	cmd := exec.Command("sh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	defer ttyGuard()()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the installer failed: %w", err)
	}
	return nil
}

// semver is MAJOR.MINOR.PATCH; a pre-release or build suffix is ignored for
// ordering (it marks a development build, handled by the caller).
type semver [3]int

func (v semver) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// parseSemver reads the leading MAJOR.MINOR.PATCH of s, with an optional "v".
func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var v semver
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		v[i] = n
	}
	return v, true
}

func compareSemver(a, b semver) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] > b[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}
