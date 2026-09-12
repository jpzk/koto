package main

// The claude CLI on the HOST. The proxy refreshes a subscription (OAuth) token
// by running `claude -p ok` and letting the CLI rotate the token in
// $HOME/.claude/.credentials.json — it has done so since the first Python
// proxy, and koto has never reimplemented the OAuth refresh itself (see
// README, "Credentials": the token is the operator's, koto only forwards it).
//
// That shell-out silently broke when the daemon moved out of its container
// onto the host (2026-09-03). The container image installed claude-code into
// /usr/local/bin; the systemd unit runs with PATH=/usr/local/bin:/usr/bin and
// ProtectHome=yes, so a claude installed by the native installer into
// ~/.local/bin is both off PATH and hidden. refresh() discarded the exec
// error, so the token simply expired every ~8h and every turn 401'd until the
// operator ran `koto claude-login` again — measured 2026-09-05: three such
// outages in the first 36h of the install, each cleared by a manual login.
//
// The fix keeps the shell-out (the CLI manages the token, koto does not) and
// makes the binary reachable: `koto install` resolves claude at install time,
// records the path in koto.env as KOTO_CLAUDE_BIN, and — when that path lives
// under a directory ProtectHome hides — switches the unit to
// ProtectHome=tmpfs plus a read-only bind of exactly the directories the
// binary needs, so the rest of the operator's home stays invisible. The
// daemon execs that path; refresh() now logs its failure; and `koto
// claude-login --status` checks the binary from the DAEMON's point of view,
// which is the check preflight never made (it looked on the operator's PATH,
// where the binary was fine).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// claudeBinEnv names the claude executable the daemon runs for token
// refresh. Written by `koto install`, read by the proxy on every refresh.
const claudeBinEnv = "KOTO_CLAUDE_BIN"

// claudeBin is the executable refresh() runs: KOTO_CLAUDE_BIN when set, else a
// bare "claude" for exec.Command's PATH lookup — the dev-clone case, where the
// daemon inherits the operator's shell PATH.
func claudeBin() string {
	if v := strings.TrimSpace(os.Getenv(claudeBinEnv)); v != "" {
		return v
	}
	return "claude"
}

// claudeBinEnviron is the environment for the refresh subprocess: ours, with
// the binary's own directory prepended to PATH when we exec an absolute path.
// An npm-installed claude is `#!/usr/bin/env node` and node is its sibling
// (nvm lays out bin/{node,claude}), so the interpreter has to be findable from
// the unit's PATH, which the operator's shell PATH is not.
func claudeBinEnviron(bin string) []string {
	env := os.Environ()
	if !filepath.IsAbs(bin) {
		return env
	}
	dir := filepath.Dir(bin)
	path := os.Getenv("PATH")
	for _, p := range strings.Split(path, ":") {
		if p == dir {
			return env
		}
	}
	if path == "" {
		return setEnv(env, "PATH", dir)
	}
	return setEnv(env, "PATH", dir+":"+path)
}

// claudeBinResolve finds claude on the CURRENT process's PATH — the install-
// time answer, taken in the operator's shell, which is the only place the
// binary is guaranteed to be findable. Returns the PATH entry, not its symlink
// target: the native installer points ~/.local/bin/claude at
// ~/.local/share/claude/versions/<v> and moves the link on every update, so
// the link is the stable name and the target is not.
func claudeBinResolve() (string, error) {
	p, err := exec.LookPath("claude")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	return p, nil
}

// protectHomeHides says whether systemd's ProtectHome= makes this path
// invisible to the unit: it covers /home, /root and /run/user.
func protectHomeHides(path string) bool {
	for _, pre := range []string{"/home/", "/root/", "/run/user/"} {
		if strings.HasPrefix(path, pre) {
			return true
		}
	}
	return path == "/home" || path == "/root" || path == "/run/user"
}

// protectHomeRoot says whether path is a whole protected HOME rather than a
// directory inside one: /home/<user>, /root, /run/user/<uid>, or the parents
// of those.
//
// Binding one of these read-only into the unit would make every file in the
// operator's home visible to the daemon — unrelated credentials, private
// repositories, ssh keys — which is the confidentiality half of ProtectHome
// and not something a claude location should be able to switch off (audit
// M93). Read-only is not containment here; the daemon is tier 2 and the home
// is tier 1's.
func protectHomeRoot(path string) bool {
	path = filepath.Clean(path)
	switch path {
	case "/home", "/root", "/run/user", "/":
		return true
	}
	for _, pre := range []string{"/home/", "/run/user/"} {
		if strings.HasPrefix(path, pre) && !strings.Contains(path[len(pre):], "/") {
			return true // exactly /home/<user> or /run/user/<uid>
		}
	}
	return strings.HasPrefix(path, "/root/") && !strings.Contains(path[len("/root/"):], "/")
}

// claudeBindDirs lists the directories a ProtectHome'd unit must bind
// read-only for bin to be executable: the directory holding the PATH entry
// and, when that is a symlink, the directory holding its target. Directories
// rather than the files themselves so an update — a new versions/<v> file
// and a re-pointed link — is picked up without a daemon restart; a bound
// FILE pins the inode at mount time and dangles once the old version is
// removed. Only directories ProtectHome actually hides are returned; a claude
// under /usr/local needs nothing.
//
// hidden is the predicate (protectHomeHides in production), a parameter so
// the symlink walk can be tested against a temp layout that ProtectHome would
// never hide.
func claudeBindDirs(bin string, hidden func(string) bool) []string {
	if bin == "" || !filepath.IsAbs(bin) {
		return nil
	}
	var out []string
	add := func(d string) {
		if !hidden(d) {
			return
		}
		if protectHomeRoot(d) {
			// A claude sitting directly in the operator's home would bind the
			// WHOLE home read-only into the service namespace. Refuse, and let
			// the caller report it — the fix is to move the binary (the native
			// installer's ~/.local/bin is already fine), not to hand tier 2 the
			// operator's home (audit M93).
			return
		}
		for _, o := range out {
			if o == d {
				return
			}
		}
		out = append(out, d)
	}
	add(filepath.Dir(bin))
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		add(filepath.Dir(real))
	}
	return out
}

// claudeBinCheck reports whether the claude the daemon would exec for OAuth
// token refresh is reachable and executable FROM INSIDE THE UNIT.
//
// The vantage point is the whole point. `koto setup --check` resolves claude on
// the OPERATOR's PATH, where it is nearly always fine, which is exactly the
// check that missed the 2026-09-03 breakage (see this file's header). The
// daemon, by contrast, is already inside the unit's mount namespace and PATH,
// so a plain stat from here answers the only question that matters: can the
// process that has to run it, run it?
//
// Errors are phrased for someone who did nothing wrong, because that is the
// usual case — the binary moved under them (an nvm major-version bump, a
// reinstall landing elsewhere, a version directory garbage-collected). The
// unit's filesystem namespace is fixed at install time, so no amount of
// re-resolution here can reach a binary that moved outside the bound
// directories; re-running `koto install` to re-render the unit is the fix, and
// the message says so rather than leaving the operator to infer it.
func claudeBinCheck() (string, error) {
	bin := claudeBin()
	if !filepath.IsAbs(bin) {
		p, err := exec.LookPath(bin)
		if err != nil {
			return bin, fmt.Errorf("%q is not on the daemon's PATH (%s) and %s is unset — "+
				"re-run `koto install` with claude installed so the path is recorded",
				bin, os.Getenv("PATH"), claudeBinEnv)
		}
		bin = p
	}
	// Stat, not Lstat: a dangling symlink is precisely the nvm/version-bump
	// failure this exists to catch, and it must read as broken, not as present.
	fi, err := os.Stat(bin)
	if err != nil {
		return bin, fmt.Errorf("%s is not reachable from inside the service "+
			"(ProtectHome/BindReadOnlyPaths are rendered at install time) — "+
			"re-run `koto install`: %w", bin, err)
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return bin, fmt.Errorf("%s is not executable", bin)
	}
	return bin, nil
}

// claudeBinWatch alerts the operator while the token is still valid, instead of
// letting a broken refresh be discovered by the fleet 401ing.
//
// refreshOnce already reports a failed refresh at error level, but only when a
// refresh is actually due — up to ~8h after the binary became unreachable, by
// which point every turn is failing. The condition is silent, static and
// entirely knowable before then, so it is worth a cheap stat.
//
// It alerts on TRANSITION only. The condition persists until someone fixes it,
// and logalert's token bucket would otherwise turn a standing fault into a
// recurring banner, which trains operators to dismiss it — the same reasoning
// as the resource alerts' hysteresis. Recovery is reported too, so the operator
// learns their fix worked without having to go looking.
func claudeBinWatch(stop <-chan struct{}) {
	const every = 30 * time.Minute
	broken := false
	check := func() {
		// An API key outranks OAuth in authHeaders and never execs claude, so
		// alerting on a host that authenticates with one would be noise about
		// a binary it has no use for.
		if currentAPIKey() != "" {
			return
		}
		if _, err := readCreds(); err != nil {
			return // no OAuth credential to refresh either
		}
		bin, err := claudeBinCheck()
		switch {
		case err != nil && !broken:
			broken = true
			emitLogf("proxy", "error", "claude is not usable by the daemon, so the OAuth token "+
				"cannot be refreshed and turns will 401 once it expires: %v", err)
		case err == nil && broken:
			broken = false
			emitLogf("proxy", "info", "claude is reachable again (%s); token refresh restored", bin)
		}
	}
	check()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			check()
		case <-stop:
			return
		}
	}
}
